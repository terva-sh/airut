package discord

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"terva.sh/airut"
)

// bundlePrefix opens the custom id of every control a bundle posts. The
// rest is the bundle's id, then "q" and a question's index for a select
// menu, "text" for the button that opens the typed answers, "modal" for
// that form, or "yes" or "no" for answering every question at once.
const bundlePrefix = "airut-bundle:"

// Discord's limits: 25 options to a select menu, 100 characters to an
// option's label and 150 to a placeholder, and 45 to a modal's title and
// to a text input's label. A typed answer is capped well inside the
// 4000 an input allows, so a whole bundle's outcome fits one message.
const (
	maxSelect      = 25
	maxOptionLabel = 100
	maxPlaceholder = 150
	maxModalLabel  = 45
	// maxInputPlaceholder is the 100 characters a text input's
	// placeholder holds.
	maxInputPlaceholder = 100
	maxTypedAnswer      = 500
)

// bundle is a Bundle waiting for its answers.
type bundle struct {
	id   string
	b    airut.Bundle
	part part
	r    *responder

	mu      sync.Mutex
	answers map[string]airut.Answer
	closed  bool
	end     string        // marks each open question once closed
	done    chan struct{} // closed when every question is answered

	// shown serializes what shows the bundle, an interaction's answer or
	// the closing edit, each rendering the bundle as it is when its turn
	// comes, so whichever runs last shows the latest state.
	shown sync.Mutex
}

// show hands the bundle's state now to send.
func (p *bundle) show(send func(text string, controls []discordgo.MessageComponent)) {
	p.shown.Lock()
	defer p.shown.Unlock()
	p.mu.Lock()
	text, controls := p.render(p.end), p.controls(p.id)
	p.mu.Unlock()
	send(text, controls)
}

func (p *bundle) complete() bool { return len(p.answers) == len(p.b.Questions) }

// render is the bundle's message: the intro, then each question with
// its answer when it has one, or end set once it can have no more. The
// core bounds the intro and the questions, so all of them fit; the
// answers share what room is left, each cut to its share, so a long
// typed answer never pushes a later question or answer out.
func (p *bundle) render(end string) string {
	answered := 0
	for _, q := range p.b.Questions {
		if _, ok := p.answers[q.Key]; ok {
			answered++
		}
	}
	share := 0
	if answered > 0 {
		share = max(0, maxText-units(p.write(end, func(string) string { return "" }))) / answered
	}
	return truncate(p.write(end, func(a string) string { return truncate(a, share) }), maxText)
}

// write lays the bundle out, showing each answer through show.
func (p *bundle) write(end string, show func(string) string) string {
	var b strings.Builder
	if p.b.Intro != "" {
		b.WriteString(p.b.Intro + "\n\n")
	}
	for i, q := range p.b.Questions {
		fmt.Fprintf(&b, "%d. %s", i+1, q.Text)
		if a, ok := p.answers[q.Key]; ok {
			b.WriteString("\n    Answer: " + show(a.Text))
		} else if end != "" {
			b.WriteString("\n    " + end)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// controls are the rows of the questions still open: a select menu for
// each choice question, then a row with the button for typed answers
// and the buttons that answer every question at once.
func (p *bundle) controls(id string) []discordgo.MessageComponent {
	rows := []discordgo.MessageComponent{}
	if p.closed {
		return rows
	}
	typed, yesNo := false, true
	for i, q := range p.b.Questions {
		if _, ok := p.answers[q.Key]; ok {
			continue
		}
		yesNo = yesNo && q.IsYesNo()
		if len(q.Choices) == 0 {
			typed = true
			continue
		}
		var opts []discordgo.SelectMenuOption
		for j, c := range q.Choices {
			opts = append(opts, discordgo.SelectMenuOption{Label: truncate(c, maxOptionLabel), Value: strconv.Itoa(j)})
		}
		rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.SelectMenu{
			CustomID:    bundlePrefix + id + ":q" + strconv.Itoa(i),
			Placeholder: truncate(fmt.Sprintf("%d. %s", i+1, q.Text), maxPlaceholder),
			Options:     opts,
		}}})
	}
	var last []discordgo.MessageComponent
	if typed {
		last = append(last, discordgo.Button{Label: "Answer", Style: discordgo.PrimaryButton, CustomID: bundlePrefix + id + ":text"})
	}
	if yesNo && len(rows) > 1 {
		last = append(last,
			discordgo.Button{Label: "All yes", Style: discordgo.SecondaryButton, CustomID: bundlePrefix + id + ":yes"},
			discordgo.Button{Label: "All no", Style: discordgo.SecondaryButton, CustomID: bundlePrefix + id + ":no"})
	}
	if len(last) > 0 {
		rows = append(rows, discordgo.ActionsRow{Components: last})
	}
	return rows
}

// bundles holds the session's open bundles by id. A closed bundle stays
// until its wait returns, so a late press is told it is closed.
type bundles struct {
	mu   sync.Mutex
	open map[string]*bundle
}

func (bs *bundles) add(id string, p *bundle) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.open == nil {
		bs.open = map[string]*bundle{}
	}
	bs.open[id] = p
}

func (bs *bundles) get(id string) *bundle {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return bs.open[id]
}

func (bs *bundles) remove(id string) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	delete(bs.open, id)
}

// AskAll posts the bundle as one message with a control for each
// question and waits for every answer from someone b.Who admits. Each
// answer rewrites the message to show it and drops its control. When the
// time runs out the controls are removed, each open question is marked
// unanswered, and AskAll returns what arrived.
func (r *responder) AskAll(ctx context.Context, b airut.Bundle) (map[string]airut.Answer, error) {
	for i, q := range b.Questions {
		if len(q.Choices) > maxSelect {
			return nil, fmt.Errorf("discord: question %d has %d choices, more than the %d a menu holds", i+1, len(q.Choices), maxSelect)
		}
	}
	id := newAskID()
	p := &bundle{id: id, b: b, r: r, answers: map[string]airut.Answer{}, done: make(chan struct{})}
	r.s.bundles.add(id, p)
	defer r.s.bundles.remove(id)
	_, parts, err := r.send(ctx, airut.Message{Text: p.render("")}, p.controls(id))
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.part = parts[len(parts)-1]
	p.mu.Unlock()

	select {
	case <-p.done:
	case <-ctx.Done():
	}
	p.mu.Lock()
	got := make(map[string]airut.Answer, len(p.answers))
	for k, v := range p.answers {
		got[k] = v
	}
	complete := p.complete()
	if !complete {
		p.closed, p.end = true, "No answer in time."
	}
	at := p.part
	p.mu.Unlock()
	if complete {
		return got, nil
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	p.show(func(text string, controls []discordgo.MessageComponent) {
		if err := r.editPart(cctx, at, text, &controls); err != nil {
			r.s.log.Warn("could not close a question", "message", at.id, "error_kind", errKind(err))
		}
	})
	return got, ctx.Err()
}

// answerer is who sent an interaction, with the roles it carries.
func (s *session) answerer(ic *discordgo.InteractionCreate) airut.Answerer {
	var a airut.Answerer
	switch {
	case ic.Member != nil && ic.Member.User != nil:
		a.User = user(ic.Member.User, ic.Member)
		for _, r := range ic.Member.Roles {
			a.Roles = append(a.Roles, airut.Role{Platform: airut.Discord, ID: r})
		}
	case ic.User != nil:
		a.User = user(ic.User, nil)
	}
	return a
}

// onBundle answers a press, a choice, or a form on a bundle. It reports
// whether the interaction was a bundle's.
func (s *session) onBundle(ic *discordgo.InteractionCreate, custom string, values func() []string, typed func() map[string]string) bool {
	rest, ok := strings.CutPrefix(custom, bundlePrefix)
	if !ok {
		return false
	}
	id, what, _ := strings.Cut(rest, ":")
	p := s.bundles.get(id)
	if p == nil {
		if ic.Message != nil && ic.Type == discordgo.InteractionMessageComponent {
			// A bundle from before a restart: remove its controls.
			s.update(ic, closed(ic.Message.Content, "This question is closed."))
		} else {
			s.notice(ic, "This question is closed.")
		}
		return true
	}
	who := s.answerer(ic)
	if !p.b.Who(who) {
		s.notice(ic, "This question is for someone else.")
		return true
	}
	p.mu.Lock()
	if p.closed || p.complete() {
		p.mu.Unlock()
		s.notice(ic, "This question is closed.")
		return true
	}
	if what == "text" {
		form := p.form(id)
		p.mu.Unlock()
		if form == nil {
			s.notice(ic, "Those questions are answered.")
			return true
		}
		s.respond(ic, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseModal, Data: form})
		return true
	}
	switch {
	case what == "yes" || what == "no":
		for _, q := range p.b.Questions {
			if _, done := p.answers[q.Key]; !done && q.IsYesNo() {
				p.answers[q.Key] = airut.Answer{Text: q.Choices[map[string]int{"yes": 0, "no": 1}[what]], By: who.User}
			}
		}
	case what == "modal":
		for k, v := range typed() {
			i, err := strconv.Atoi(strings.TrimPrefix(k, "q"))
			if err != nil || i < 0 || i >= len(p.b.Questions) {
				continue
			}
			q := p.b.Questions[i]
			if _, done := p.answers[q.Key]; done || len(q.Choices) > 0 || strings.TrimSpace(v) == "" {
				continue
			}
			p.answers[q.Key] = airut.Answer{Text: truncate(strings.TrimSpace(v), maxTypedAnswer), By: who.User}
		}
	case strings.HasPrefix(what, "q"):
		i, err := strconv.Atoi(what[1:])
		vs := values()
		if err != nil || i < 0 || i >= len(p.b.Questions) || len(vs) != 1 {
			break
		}
		q := p.b.Questions[i]
		j, err := strconv.Atoi(vs[0])
		if _, done := p.answers[q.Key]; !done && err == nil && j >= 0 && j < len(q.Choices) {
			p.answers[q.Key] = airut.Answer{Text: q.Choices[j], By: who.User}
		}
	}
	if p.complete() {
		select {
		case <-p.done:
		default:
			close(p.done)
		}
	}
	p.mu.Unlock()
	p.show(func(text string, controls []discordgo.MessageComponent) {
		s.respond(ic, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{Content: text, Components: controls, AllowedMentions: noMentions},
		})
	})
	return true
}

// typedValues reads a submitted form's text inputs by custom id.
func typedValues(rows []discordgo.MessageComponent) map[string]string {
	out := map[string]string{}
	for _, row := range rows {
		r, ok := row.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, c := range r.Components {
			if in, ok := c.(*discordgo.TextInput); ok {
				out[in.CustomID] = in.Value
			}
		}
	}
	return out
}

// form is the modal for the bundle's open free-text questions, or nil
// when none is open. It is called with p.mu held.
func (p *bundle) form(id string) *discordgo.InteractionResponseData {
	var rows []discordgo.MessageComponent
	for i, q := range p.b.Questions {
		if _, done := p.answers[q.Key]; done || len(q.Choices) > 0 {
			continue
		}
		// The label holds 45 characters, so it leads with the number the
		// message shows the whole question under.
		rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
			CustomID:    "q" + strconv.Itoa(i),
			Label:       truncate(fmt.Sprintf("%d. %s", i+1, q.Text), maxModalLabel),
			Placeholder: truncate(q.Text, maxInputPlaceholder),
			Style:       discordgo.TextInputParagraph,
			MaxLength:   maxTypedAnswer,
		}}})
	}
	if rows == nil {
		return nil
	}
	return &discordgo.InteractionResponseData{CustomID: bundlePrefix + id + ":modal", Title: "Answer", Components: rows}
}

// respond answers an interaction, logging a failure by kind.
func (s *session) respond(ic *discordgo.InteractionCreate, resp *discordgo.InteractionResponse) {
	if err := s.dg.InteractionRespond(ic.Interaction, resp, discordgo.WithContext(s.ctx)); err != nil {
		s.log.Warn("could not answer a question's control", "interaction", ic.ID, "error_kind", errKind(err))
	}
}
