package discord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"terva.sh/airut"
)

// Discord's limits on buttons: five to a row, five rows to a message,
// and a label of at most 80 characters.
const (
	buttonsPerRow = 5
	maxButtons    = 25
	maxLabel      = 80
)

// askPrefix opens the custom id of every button an Ask posts. The rest
// is the ask's id and the choice's index.
const askPrefix = "airut-ask:"

// pending is an Ask waiting for its answer.
type pending struct {
	question string
	who      string
	choices  []string
	answer   chan string
}

// asks holds the session's open asks by id. Taking an ask removes it, so
// exactly one of an answer and a timeout closes it, and remembers its id
// as done, so a press that arrives after it closed is not mistaken for
// one on an ask from before a restart.
type asks struct {
	mu   sync.Mutex
	open map[string]*pending
	done []string
}

// keptDone is how many closed asks the session remembers.
const keptDone = 1024

func (a *asks) add(id string, p *pending) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.open == nil {
		a.open = map[string]*pending{}
	}
	a.open[id] = p
}

func (a *asks) peek(id string) *pending {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.open[id]
}

func (a *asks) take(id string) *pending {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.open[id]
	if p == nil {
		return nil
	}
	delete(a.open, id)
	if len(a.done) == keptDone {
		a.done = a.done[1:]
	}
	a.done = append(a.done, id)
	return p
}

// closed reports whether id is an ask this session closed.
func (a *asks) closed(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Contains(a.done, id)
}

// Ask posts the question with a button for each choice and waits for
// a.Who to press one. The message then shows the answer, without the
// buttons; a press by anyone else is told privately that the question is
// not theirs. When the time runs out the buttons are removed and Ask
// returns airut.ErrAskTimeout.
func (r *responder) Ask(ctx context.Context, a airut.Ask) (string, error) {
	if len(a.Choices) == 0 || len(a.Choices) > maxButtons {
		return "", fmt.Errorf("discord: an ask takes 1 to %d choices, not %d", maxButtons, len(a.Choices))
	}
	id := newAskID()
	question := truncate(strings.ToValidUTF8(a.Question, "\uFFFD"), maxText)
	p := &pending{question: question, who: a.Who.ID, choices: a.Choices, answer: make(chan string, 1)}
	r.s.asks.add(id, p)
	_, parts, err := r.send(ctx, airut.Message{Text: question}, buttons(id, a.Choices))
	if err != nil {
		r.s.asks.take(id)
		return "", err
	}
	timer := time.NewTimer(a.Timeout)
	defer timer.Stop()
	var done error
	select {
	case ans := <-p.answer:
		return ans, nil
	case <-timer.C:
		done = airut.ErrAskTimeout
	case <-ctx.Done():
		done = ctx.Err()
	}
	if r.s.asks.take(id) == nil {
		// Answered as the wait ended.
		return <-p.answer, nil
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	none := []discordgo.MessageComponent{}
	if err := r.editPart(cctx, parts[len(parts)-1], closed(question, "No answer in time."), &none); err != nil {
		r.s.log.Warn("could not close a question", "message", parts[len(parts)-1].id, "error_kind", errKind(err))
	}
	return "", done
}

// buttons lays out one button per choice, five to a row.
func buttons(id string, choices []string) []discordgo.MessageComponent {
	var rows []discordgo.MessageComponent
	var row []discordgo.MessageComponent
	for i, c := range choices {
		row = append(row, discordgo.Button{
			Label:    truncate(c, maxLabel),
			Style:    discordgo.SecondaryButton,
			CustomID: askPrefix + id + ":" + strconv.Itoa(i),
		})
		if len(row) == buttonsPerRow || i == len(choices)-1 {
			rows = append(rows, discordgo.ActionsRow{Components: row})
			row = nil
		}
	}
	return rows
}

// maxOutcome bounds the line closed adds, so a long choice still leaves
// room for the question and the whole fits one message.
const maxOutcome = maxText / 2

// closed is the text of a question once it is answered or has expired.
func closed(question, outcome string) string {
	outcome = truncate(outcome, maxOutcome)
	return truncate(question, maxText-units(outcome)-2) + "\n\n" + outcome
}

// onComponent answers a button press on an Ask.
func (s *session) onComponent(ic *discordgo.InteractionCreate) {
	data := ic.MessageComponentData()
	rest, ok := strings.CutPrefix(data.CustomID, askPrefix)
	if !ok {
		return
	}
	id, n, _ := strings.Cut(rest, ":")
	index, err := strconv.Atoi(n)
	presser := ""
	switch {
	case ic.Member != nil && ic.Member.User != nil:
		presser = ic.Member.User.ID
	case ic.User != nil:
		presser = ic.User.ID
	}
	p := s.asks.peek(id)
	switch {
	case p == nil && s.asks.closed(id):
		// Answered or expired while this press was on its way: its
		// message already shows the outcome.
		s.notice(ic, "This question is closed.")
		return
	case p == nil && ic.Message != nil:
		// An ask from before a restart, whose buttons were never
		// removed: remove them now.
		s.update(ic, closed(ic.Message.Content, "This question is closed."))
		return
	case p == nil || err != nil || index < 0 || index >= len(p.choices):
		s.notice(ic, "This question is closed.")
		return
	case p.who != "" && presser != p.who:
		s.notice(ic, "This question is for someone else.")
		return
	}
	if s.asks.take(id) == nil {
		s.notice(ic, "This question is closed.")
		return
	}
	choice := p.choices[index]
	p.answer <- choice
	s.update(ic, closed(p.question, "Answer: "+choice))
}

// update answers a button press by rewriting the message it is on to
// text, without its buttons.
func (s *session) update(ic *discordgo.InteractionCreate, text string) {
	err := s.dg.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:         text,
			Components:      []discordgo.MessageComponent{},
			AllowedMentions: noMentions,
		},
	}, discordgo.WithContext(s.ctx))
	if err != nil {
		s.log.Warn("could not close a question", "interaction", ic.ID, "error_kind", errKind(err))
	}
}

// notice answers a button press with a message only the presser sees.
func (s *session) notice(ic *discordgo.InteractionCreate, text string) {
	err := s.dg.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noMentions},
	}, discordgo.WithContext(s.ctx))
	if err != nil {
		s.log.Warn("could not answer a button press", "interaction", ic.ID, "error_kind", errKind(err))
	}
}

func newAskID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// truncate cuts text to at most limit UTF-16 units, marking the cut.
func truncate(text string, limit int) string {
	if units(text) <= limit {
		return text
	}
	const mark = "…" // one UTF-16 unit
	n, cut := 0, 0
	for i, c := range text {
		n += utf16.RuneLen(c)
		if n > limit-1 {
			break
		}
		_, w := utf8.DecodeRuneInString(text[i:])
		cut = i + w
	}
	return text[:cut] + mark
}
