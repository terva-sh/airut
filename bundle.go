package airut

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
)

// MaxBundle is the most questions one Bundle may hold. Discord gives a
// message five rows of controls: one for each question, and one for the
// answers typed in and those given all at once.
const MaxBundle = 4

// The longest intro and question text a Bundle may have, so that every
// question, and room for its answer, fits one message on every platform.
// They count UTF-16 code units, as Discord does, so a character outside
// the Basic Multilingual Plane, such as most emoji, counts as two.
const (
	MaxBundleIntro = 400
	MaxQuestion    = 200
)

// Question is one question of a Bundle. Without choices it takes a
// free-text answer.
type Question struct {
	// Key names the question's answer in what AskAll returns. Keys are
	// unique within a bundle.
	Key     string   `json:"key"`
	Text    string   `json:"text"`
	Choices []string `json:"choices,omitempty"`
}

// Bundle is several questions asked in one message. Each is answered on
// its own, and when every question is answered yes or no, all can be
// answered at once.
type Bundle struct {
	// Intro, if set, opens the message above the numbered questions.
	Intro     string
	Questions []Question
	// Who admits people other than the caller to answer, such as anyone
	// with a moderator role. The caller may always answer. A nil Who
	// admits nobody else. An adapter calls it with the answerer as the
	// platform reports them, and it must not block.
	Who func(Answerer) bool
	// Timeout bounds the wait for the whole bundle. The core fills in
	// Config.AskTimeout when it is zero.
	Timeout time.Duration
}

// Answer is one question's answer and who gave it.
type Answer struct {
	Text string
	By   User
}

// Answerer is someone answering a Bundle, with the roles the platform
// reported with the answer. Matrix reports none.
type Answerer struct {
	User  User
	Roles []Role
}

// BundleAsker is a Responder that can ask a Bundle in one message. Who,
// as it reaches the adapter, already admits the caller. The adapter
// edits the message to show each outcome as it lands, and returns when
// every question is answered or the context ends, with the answers that
// arrived. A Responder without it asks a bundle of choice questions one
// question at a time.
type BundleAsker interface {
	AskAll(ctx context.Context, b Bundle) (map[string]Answer, error)
}

// IsYesNo reports whether a question's choices are yes and no, in that
// order, which lets a bundle offer to answer every question at once.
func (q Question) IsYesNo() bool {
	return len(q.Choices) == 2 && strings.EqualFold(q.Choices[0], "yes") && strings.EqualFold(q.Choices[1], "no")
}

// AskAll puts the bundle's questions in one message and waits until each
// is answered or the time runs out. It returns the answers by key, each
// with who gave it. When
// the time runs out first, it returns the answers that arrived with
// ErrAskTimeout: a question with no answer has no key, and is never read
// as yes. Typing pauses while it waits, as for Ask.
func (c *Ctx) AskAll(b Bundle) (map[string]Answer, error) {
	if err := checkBundle(b); err != nil {
		return nil, err
	}
	caller, who := c.inv.Caller, b.Who
	b.Who = func(a Answerer) bool {
		return a.User.Platform == caller.Platform && a.User.ID == caller.ID || who != nil && who(a)
	}
	if b.Timeout <= 0 {
		b.Timeout = c.bot.cfg.AskTimeout
	}
	ctx, cancel := context.WithTimeout(c.ctx, b.Timeout)
	defer cancel()
	var got map[string]Answer
	ask := func() (string, error) {
		var err error
		got, err = c.askAll(ctx, b)
		return "", err
	}
	var err error
	if c.turn != nil {
		_, err = c.turn.asking(ask)
	} else {
		_, err = ask()
	}
	if got == nil {
		got = map[string]Answer{}
	}
	if errors.Is(err, context.DeadlineExceeded) && c.ctx.Err() == nil {
		err = ErrAskTimeout
	}
	return got, err
}

func (c *Ctx) askAll(ctx context.Context, b Bundle) (map[string]Answer, error) {
	if ba, ok := c.resp.(BundleAsker); ok {
		return ba.AskAll(ctx, b)
	}
	// One question at a time, put to the caller alone: Ask has no way
	// to admit anyone else.
	for _, q := range b.Questions {
		if len(q.Choices) == 0 {
			return nil, errors.New("airut: this platform cannot ask a bundle with a free-text question")
		}
	}
	got := map[string]Answer{}
	for i, q := range b.Questions {
		text := fmt.Sprintf("%d. %s", i+1, q.Text)
		if i == 0 && b.Intro != "" {
			text = b.Intro + "\n\n" + text
		}
		ans, err := c.resp.Ask(ctx, Ask{Question: text, Choices: q.Choices, Who: c.inv.Caller, Timeout: b.Timeout})
		if err != nil {
			return got, err
		}
		got[q.Key] = Answer{Text: ans, By: c.inv.Caller}
	}
	return got, nil
}

func checkBundle(b Bundle) error {
	if n := len(b.Questions); n == 0 || n > MaxBundle {
		return fmt.Errorf("airut: a bundle takes 1 to %d questions, not %d", MaxBundle, n)
	}
	if units(b.Intro) > MaxBundleIntro {
		return fmt.Errorf("airut: a bundle's intro is longer than %d characters, counting most emoji as two", MaxBundleIntro)
	}
	seen := map[string]bool{}
	for i, q := range b.Questions {
		switch {
		case q.Key == "":
			return fmt.Errorf("airut: question %d has no key", i+1)
		case seen[q.Key]:
			return fmt.Errorf("airut: two questions have the key %q", q.Key)
		case strings.TrimSpace(q.Text) == "":
			return fmt.Errorf("airut: question %d has no text", i+1)
		case units(q.Text) > MaxQuestion:
			return fmt.Errorf("airut: question %d is longer than %d characters, counting most emoji as two", i+1, MaxQuestion)
		}
		seen[q.Key] = true
	}
	return nil
}

// units is the length of s in UTF-16 code units. A byte that is not
// valid UTF-8 counts as one, as its replacement character does.
func units(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}
