package airut_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

func TestAskAll(t *testing.T) {
	b, _ := newBot(airut.Config{Gate: openGate})
	var got map[string]airut.Answer
	var gotErr error
	bundle := airut.Bundle{
		Intro: "Before I retag these:",
		Questions: []airut.Question{
			{Key: "post", Text: "Which post?", Choices: []string{"the troll", "the Tolkien one"}},
			{Key: "rating", Text: "Keep the rating?", Choices: []string{"yes", "no"}},
			{Key: "tag", Text: "What tag instead?"},
		},
		Timeout: 50 * time.Millisecond,
	}
	b.Command("retag", "Retag", func(c *airut.Ctx) error {
		got, gotErr = c.AskAll(bundle)
		return nil
	})
	a := airuttest.Start(t, b)
	user := func(id string) airut.User { return airut.User{Platform: airuttest.Platform, ID: id} }
	mod := airut.Role{Platform: airuttest.Platform, ID: "mods"}

	// Everyone answers; only the caller's answers count, one per
	// question.
	a.OnAskAll(func(airut.Bundle) []airuttest.Answer {
		return []airuttest.Answer{
			{By: user("user-2"), Key: "post", Text: "the troll"},
			{By: user("user-2"), Roles: []airut.Role{mod}, Key: "rating", Text: "yes"},
			{By: user("user-1"), Key: "post", Text: "the Tolkien one"},
			{By: user("user-1"), Key: "post", Text: "the troll"},
			{By: user("user-1"), Key: "rating", Text: "no"},
			{By: user("user-1"), Key: "tag", Text: "elves"},
		}
	})
	evs := a.Text("!retag")
	wantEvents(t, evs, "ask-all:Before I retag these:\n1. Which post?\n2. Keep the rating?\n3. What tag instead?",
		"answered:post: the Tolkien one", "answered:rating: no", "answered:tag: elves")
	if want := map[string]airut.Answer{"post": {Text: "the Tolkien one", By: user("user-1")}, "rating": {Text: "no", By: user("user-1")}, "tag": {Text: "elves", By: user("user-1")}}; gotErr != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("AskAll = %v, %v", got, gotErr)
	}

	// Who widens the answerers; the time runs out before the tag, which
	// comes back missing, not as any answer.
	bundle.Who = func(a airut.Answerer) bool {
		for _, r := range a.Roles {
			if r == mod {
				return true
			}
		}
		return false
	}
	a.OnAskAll(func(airut.Bundle) []airuttest.Answer {
		return []airuttest.Answer{
			{By: user("user-2"), Roles: []airut.Role{mod}, Key: "rating", Text: "yes"},
			{By: user("user-3"), Key: "post", Text: "the troll"},
			{By: user("user-1"), Key: "post", Text: "the troll"},
		}
	})
	a.Text("!retag")
	// The moderator's answer is theirs, not the caller's.
	if want := map[string]airut.Answer{"post": {Text: "the troll", By: user("user-1")}, "rating": {Text: "yes", By: user("user-2")}}; !errors.Is(gotErr, airut.ErrAskTimeout) || !reflect.DeepEqual(got, want) {
		t.Fatalf("AskAll = %v, %v; want the answers that came and ErrAskTimeout", got, gotErr)
	}
}

func TestAskAllRefusesABadBundle(t *testing.T) {
	q := func(key, text string) airut.Question { return airut.Question{Key: key, Text: text} }
	for _, c := range []struct {
		name string
		qs   []airut.Question
		want string
	}{
		{"none", nil, "1 to 4"},
		{"too many", []airut.Question{q("a", "?"), q("b", "?"), q("c", "?"), q("d", "?"), q("e", "?")}, "1 to 4"},
		{"no key", []airut.Question{q("", "?")}, "no key"},
		{"a key twice", []airut.Question{q("a", "?"), q("a", "?")}, "key"},
		{"no text", []airut.Question{q("a", " ")}, "no text"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, _ := newBot(airut.Config{})
			var err error
			b.Command("ask", "Ask", func(ctx *airut.Ctx) error {
				_, err = ctx.AskAll(airut.Bundle{Questions: c.qs})
				return nil
			})
			if evs := airuttest.Start(t, b).Text("!ask"); len(evs) != 0 || err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, events %q", err, texts(evs))
			}
		})
	}
}

// TestAskAllInATurn checks that a bundle, like an Ask, posts below a
// complete reply: it goes through the turn's asking, which also pauses
// typing, as TestTypingKeepalive shows for an Ask.
func TestAskAllInATurn(t *testing.T) {
	agent := agentFunc(func(c *airut.Ctx, out airut.TurnOutput) error {
		out.Text("Let me ")
		out.Text("check two things.")
		got, err := c.AskAll(airut.Bundle{Questions: []airut.Question{{Key: "ok", Text: "OK?", Choices: []string{"yes", "no"}}}})
		if err != nil {
			return err
		}
		out.Text(" " + got["ok"].Text + ".")
		return nil
	})
	b, _ := newBot(airut.Config{Gate: openGate, Agent: agent, EditInterval: time.Hour})
	b.Command("ask", "Ask", func(c *airut.Ctx) error { return c.Agent(airut.Turn{Prompt: "go"}).StreamTo(c) })
	a := airuttest.Start(t, b)
	a.OnAskAll(func(airut.Bundle) []airuttest.Answer {
		return []airuttest.Answer{{By: airut.User{Platform: airuttest.Platform, ID: "user-1"}, Key: "ok", Text: "yes"}}
	})
	wantEvents(t, a.Text("!ask"), "reply:Let me ", "edit:Let me check two things.",
		"ask-all:1. OK?", "answered:ok: yes", "edit:Let me check two things. yes.")
}

func TestAskAllBoundsItsText(t *testing.T) {
	for _, b := range []airut.Bundle{
		{Intro: strings.Repeat("i", airut.MaxBundleIntro+1), Questions: []airut.Question{{Key: "a", Text: "?"}}},
		{Questions: []airut.Question{{Key: "a", Text: strings.Repeat("é", airut.MaxQuestion+1)}}},
		// An emoji is two UTF-16 units, as Discord counts it.
		{Intro: strings.Repeat("😀", airut.MaxBundleIntro/2+1), Questions: []airut.Question{{Key: "a", Text: "?"}}},
		{Questions: []airut.Question{{Key: "a", Text: strings.Repeat("😀", airut.MaxQuestion/2+1)}}},
	} {
		bot, _ := newBot(airut.Config{})
		var err error
		b.Timeout = time.Millisecond
		bot.Command("ask", "Ask", func(c *airut.Ctx) error {
			_, err = c.AskAll(b)
			return nil
		})
		if airuttest.Start(t, bot).Text("!ask"); err == nil || !strings.Contains(err.Error(), "longer than") {
			t.Errorf("err = %v", err)
		}
	}
	// At the limits, counted in UTF-16 units, the bundle is accepted.
	for _, b := range []airut.Bundle{
		{Intro: strings.Repeat("i", airut.MaxBundleIntro), Questions: []airut.Question{{Key: "a", Text: strings.Repeat("é", airut.MaxQuestion)}}},
		{Intro: strings.Repeat("😀", airut.MaxBundleIntro/2), Questions: []airut.Question{{Key: "a", Text: strings.Repeat("😀", airut.MaxQuestion/2)}}},
	} {
		bot, _ := newBot(airut.Config{})
		var err error
		b.Timeout = time.Millisecond
		bot.Command("ask", "Ask", func(c *airut.Ctx) error {
			_, err = c.AskAll(b)
			return nil
		})
		if airuttest.Start(t, bot).Text("!ask"); !errors.Is(err, airut.ErrAskTimeout) {
			t.Errorf("a bundle at the limits: %v", err)
		}
	}
}
