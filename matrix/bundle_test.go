package matrix

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"terva.sh/airut"
)

func TestParseAnswers(t *testing.T) {
	yn := []string{"yes", "no"}
	qs := []airut.Question{
		{Key: "a", Text: "Which post?", Choices: []string{"the troll", "the Tolkien one"}},
		{Key: "b", Text: "Keep the rating?", Choices: yn},
		{Key: "c", Text: "Why?"},
	}
	for _, c := range []struct {
		text string
		want map[int]string
	}{
		{"1 the troll, 2 yes, 3 it is funnier", map[int]string{0: "the troll", 1: "yes", 2: "it is funnier"}},
		{"1: The Tolkien one\n2. NO\n3) because 4 elves, 10 orcs", map[int]string{0: "the Tolkien one", 1: "no", 2: "because 4 elves, 10 orcs"}},
		// A number outside the questions is part of an answer, and a
		// choice that matches none is no answer.
		{"3 7 reasons; 1 the orc", map[int]string{2: "7 reasons"}},
		{"2 yes, 2 no", map[int]string{1: "yes"}},
		{"2 - yes.", map[int]string{1: "yes"}},
		{"sounds good", map[int]string{}},
		// "all yes" needs every question to be yes or no.
		{"all yes", map[int]string{}},
		{"1:", map[int]string{}},
	} {
		if got := parseAnswers(c.text, qs); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseAnswers(%q) = %v, want %v", c.text, got, c.want)
		}
	}
	both := []airut.Question{{Key: "a", Text: "Retag 1?", Choices: yn}, {Key: "b", Text: "Retag 2?", Choices: yn}}
	if got := parseAnswers("All no!", both); !reflect.DeepEqual(got, map[int]string{0: "no", 1: "no"}) {
		t.Errorf("all no = %v", got)
	}
	if got := parseAnswers("3 "+strings.Repeat("x", 2*maxTypedAnswer), qs); len([]rune(got[2])) > maxTypedAnswer {
		t.Errorf("a long answer kept %d characters", len([]rune(got[2])))
	}
}

func TestBundleText(t *testing.T) {
	yn := []string{"yes", "no"}
	p := &bundle{b: airut.Bundle{Intro: "Before I retag:", Questions: []airut.Question{
		{Key: "a", Text: "Retag 1?", Choices: yn}, {Key: "b", Text: "Retag 2?", Choices: yn},
	}}, answers: map[string]airut.Answer{}}
	if got := p.render(""); !strings.Contains(got, "1. Retag 1? (yes / no)") || !strings.Contains(got, `"all yes"`) || !strings.HasPrefix(got, "Before I retag:\n\n") {
		t.Errorf("open bundle:\n%s", got)
	}
	p.answers["a"] = airut.Answer{Text: "no"}
	if got := p.render("No answer in time."); !strings.Contains(got, "1. Retag 1? Answer: no") || !strings.Contains(got, "2. Retag 2? No answer in time.") || strings.Contains(got, "Reply to") {
		t.Errorf("closed bundle:\n%s", got)
	}
}

// TestBundleShowsTheLatestStateLast holds the edit for the first answer
// until the second has landed, as a slow homeserver might, and checks
// that the last edit sent shows both.
func TestBundleShowsTheLatestStateLast(t *testing.T) {
	yn := []string{"yes", "no"}
	p := &bundle{b: airut.Bundle{Questions: []airut.Question{{Key: "a", Text: "A?", Choices: yn}, {Key: "b", Text: "B?", Choices: yn}}},
		answers: map[string]airut.Answer{}, done: make(chan struct{})}
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var sent []string
	p.deliver = func(_ context.Context, text string) error {
		mu.Lock()
		first := len(sent) == 0 && entered != nil
		mu.Unlock()
		if first {
			close(entered)
			entered = nil
			<-release
		}
		mu.Lock()
		sent = append(sent, text)
		mu.Unlock()
		return nil
	}
	who := airut.User{ID: "@alice:example.org"}
	p.accept(who, map[int]string{0: "yes"})
	firstDone := make(chan struct{})
	wait := entered
	go func() { p.show(t.Context()); close(firstDone) }()
	<-wait
	p.accept(who, map[int]string{1: "no"})
	secondDone := make(chan struct{})
	go func() { p.show(t.Context()); close(secondDone) }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	<-firstDone
	<-secondDone
	mu.Lock()
	defer mu.Unlock()
	if last := sent[len(sent)-1]; !strings.Contains(last, "A? Answer: yes") || !strings.Contains(last, "B? Answer: no") {
		t.Fatalf("the last edit shows:\n%s", last)
	}
}

func TestParseChoicesWithPunctuation(t *testing.T) {
	qs := []airut.Question{{Key: "a", Text: "Keep it?", Choices: []string{"Keep!", "Discard!"}}, {Key: "b", Text: "Sure?", Choices: []string{"yes", "no"}}}
	for text, want := range map[string]map[int]string{
		"1 Keep!, 2 yes.": {0: "Keep!", 1: "yes"},
		"1 discard":       {0: "Discard!"},
		"1 keep!!":        {0: "Keep!"},
	} {
		if got := parseAnswers(text, qs); !reflect.DeepEqual(got, want) {
			t.Errorf("parseAnswers(%q) = %v, want %v", text, got, want)
		}
	}
	// Where two choices differ only by punctuation, the one written wins.
	qs = []airut.Question{{Key: "a", Text: "Go?", Choices: []string{"Go", "Go!"}}}
	for text, want := range map[string]map[int]string{"1 Go!": {0: "Go!"}, "1 go": {0: "Go"}} {
		if got := parseAnswers(text, qs); !reflect.DeepEqual(got, want) {
			t.Errorf("parseAnswers(%q) = %v, want %v", text, got, want)
		}
	}
}
