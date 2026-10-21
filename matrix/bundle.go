package matrix

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"terva.sh/airut"
)

// maxTypedAnswer caps a free-text answer, so a bundle's outcome fits one
// message.
const maxTypedAnswer = 500

// bundle is a Bundle waiting for its answers, which arrive as replies
// to the message that asks it.
type bundle struct {
	b    airut.Bundle
	room id.RoomID
	ev   id.EventID
	// deliver edits the bundle's message to text.
	deliver func(ctx context.Context, text string) error

	mu      sync.Mutex
	answers map[string]airut.Answer
	closed  bool
	end     string        // marks each open question once closed
	done    chan struct{} // closed when every question is answered

	// shown serializes the edits, each of which renders the bundle as it
	// is when its turn comes, so whichever runs last shows the latest
	// state, never an older one.
	shown sync.Mutex
}

// show edits the bundle's message to its state now.
func (p *bundle) show(ctx context.Context) error {
	p.shown.Lock()
	defer p.shown.Unlock()
	p.mu.Lock()
	text := p.render(p.end)
	p.mu.Unlock()
	return p.deliver(ctx, text)
}

// accept records answers from who to the questions still open, and
// reports whether it took any. It closes done when the last lands.
func (p *bundle) accept(who airut.User, answers map[int]string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.complete() {
		return false
	}
	took := false
	for i, a := range answers {
		q := p.b.Questions[i]
		if _, done := p.answers[q.Key]; !done {
			p.answers[q.Key] = airut.Answer{Text: a, By: who}
			took = true
		}
	}
	if took && p.complete() {
		close(p.done)
	}
	return took
}

func (p *bundle) complete() bool { return len(p.answers) == len(p.b.Questions) }

func allYesNo(qs []airut.Question) bool {
	for _, q := range qs {
		if !q.IsYesNo() {
			return false
		}
	}
	return true
}

// render is the bundle's message: the intro, each question with its
// choices or its answer, and how to answer. end, once set, marks each
// open question when the bundle can have no more answers.
func (p *bundle) render(end string) string {
	var b strings.Builder
	if p.b.Intro != "" {
		b.WriteString(p.b.Intro + "\n\n")
	}
	for i, q := range p.b.Questions {
		fmt.Fprintf(&b, "%d. %s", i+1, q.Text)
		a, answered := p.answers[q.Key]
		switch {
		case answered:
			b.WriteString(" Answer: " + a.Text)
		case end != "":
			b.WriteString(" " + end)
		case len(q.Choices) > 0:
			b.WriteString(" (" + strings.Join(q.Choices, " / ") + ")")
		}
		b.WriteString("\n")
	}
	if end == "" && !p.complete() {
		b.WriteString("\nReply to this message with your answers by number, one to a line or after a comma, as in \"1 yes, 2 the first one\".")
		if allYesNo(p.b.Questions) && len(p.b.Questions) > 1 {
			b.WriteString(" Or reply \"all yes\" or \"all no\".")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// bundles holds the session's open bundles by the event that asks each.
type bundles struct {
	mu   sync.Mutex
	open map[id.EventID]*bundle
}

func (bs *bundles) add(p *bundle) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.open == nil {
		bs.open = map[id.EventID]*bundle{}
	}
	bs.open[p.ev] = p
}

func (bs *bundles) get(ev id.EventID) *bundle {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return bs.open[ev]
}

func (bs *bundles) remove(ev id.EventID) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	delete(bs.open, ev)
}

// AskAll posts the bundle as one message and takes its answers from
// replies to it by people b.Who admits, in the grammar its text gives.
// Each answer edits the message to show it. When the time runs out each
// open question is marked unanswered, and AskAll returns what arrived.
func (r *responder) AskAll(ctx context.Context, b airut.Bundle) (map[string]airut.Answer, error) {
	p := &bundle{b: b, room: r.room, answers: map[string]airut.Answer{}, done: make(chan struct{})}
	ref, err := r.s.send(ctx, r.room, r.thread, r.target, airut.Message{Text: p.render("")})
	if err != nil {
		return nil, err
	}
	p.ev = id.EventID(ref.ID)
	p.deliver = func(ctx context.Context, text string) error {
		_, err := r.s.c.SendMessageEvent(ctx, p.room, event.EventMessage, editContent(p.ev, text))
		return err
	}
	r.s.bundles.add(p)
	defer r.s.bundles.remove(p.ev)

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
	p.mu.Unlock()
	if complete {
		return got, nil
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := p.show(cctx); err != nil {
		r.s.log.Warn("could not close a question", "room", string(p.room), "error_kind", errKind(err))
	}
	return got, ctx.Err()
}

// answerBundle takes a reply to an open bundle as answers, when its
// sender may answer and it answers something, and edits the bundle to
// show them. It reports whether it took the reply; one it did not take
// is ordinary chat.
func (s *session) answerBundle(ctx context.Context, evt *event.Event, in inbound) bool {
	p := s.bundles.get(in.reply)
	if p == nil || p.room != evt.RoomID {
		return false
	}
	who := s.user(ctx, evt.RoomID, evt.Sender)
	if !p.b.Who(airut.Answerer{User: who}) || !p.accept(who, parseAnswers(in.text, p.b.Questions)) {
		return false
	}
	s.spawn(func() {
		if err := p.show(s.ctx); err != nil {
			s.log.Warn("could not show a question's answer", "room", string(p.room), "error_kind", errKind(err))
		}
	})
	return true
}

// match finds the choice an answer names, ignoring case: the choice as
// written first, then without the full stop or exclamation mark a reply
// may end on, so "yes." is yes and a choice "Keep!" is still "Keep!".
func match(choices []string, a string) (string, bool) {
	for _, c := range choices {
		if strings.EqualFold(c, a) {
			return c, true
		}
	}
	bare := strings.TrimRight(a, ".!")
	for _, c := range choices {
		if strings.EqualFold(strings.TrimRight(c, ".!"), bare) {
			return c, true
		}
	}
	return "", false
}

// answerStart finds where an answer begins: a question's number at the
// start of a line or after a comma or semicolon, then an optional
// separator.
var answerStart = regexp.MustCompile(`(?:^|[\n,;])[ \t]*(\d+)[ \t]*[:.)-]?[ \t]*`)

// parseAnswers reads a reply's answers, by question index: "all yes" or
// "all no" when every question is yes or no, or answers by number, as
// "1 yes, 2: the troll". A number is taken as the start of an answer
// only when it names one of the questions, so free text may hold other
// numbers. A choice must match one of the question's choices, ignoring
// case; anything else answers a free-text question.
func parseAnswers(text string, qs []airut.Question) map[int]string {
	out := map[int]string{}
	text = strings.TrimSpace(text)
	all := strings.ToLower(strings.TrimRight(text, ".!"))
	if (all == "all yes" || all == "all no") && allYesNo(qs) {
		for i, q := range qs {
			out[i] = q.Choices[map[string]int{"all yes": 0, "all no": 1}[all]]
		}
		return out
	}
	type start struct{ q, at, end int }
	var starts []start
	for _, m := range answerStart.FindAllStringSubmatchIndex(text, -1) {
		n, err := strconv.Atoi(text[m[2]:m[3]])
		if err != nil || n < 1 || n > len(qs) {
			continue
		}
		starts = append(starts, start{q: n - 1, at: m[0], end: m[1]})
	}
	for i, st := range starts {
		stop := len(text)
		if i+1 < len(starts) {
			stop = starts[i+1].at
		}
		a := strings.TrimSpace(text[st.end:stop])
		q := qs[st.q]
		if _, seen := out[st.q]; seen || a == "" {
			continue
		}
		if len(q.Choices) == 0 {
			out[st.q] = truncate(a, maxTypedAnswer)
			continue
		}
		if c, ok := match(q.Choices, a); ok {
			out[st.q] = c
		}
	}
	return out
}
