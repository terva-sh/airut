package discord

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"terva.sh/airut"
)

var bundleID = regexp.MustCompile(`"custom_id":"airut-bundle:([0-9a-f]+):`)

// interact delivers an interaction as Discord sends it: JSON, so the
// components decode to the types discordgo gives a real one. It waits
// for the adapter's answer, which comes from a goroutine of its own, and
// returns it.
func interact(t *testing.T, x *harness, user string, roles []string, kind discordgo.InteractionType, data string) map[string]any {
	t.Helper()
	interactions++
	n := user + "-" + itoa(interactions)
	member, _ := json.Marshal(map[string]any{"user": map[string]any{"id": user, "username": user}, "roles": roles})
	raw := `{"id":"b-` + n + `","application_id":"42","token":"tok-` + n + `","type":` + itoa(int(kind)) +
		`,"guild_id":"g1","channel_id":"c1","member":` + string(member) + `,"data":` + data + `}`
	var ic discordgo.InteractionCreate
	if err := json.Unmarshal([]byte(raw), &ic); err != nil {
		t.Fatal(err)
	}
	path := "/interactions/b-" + n + "/tok-" + n + "/callback"
	x.f.on("POST", path, 204, "")
	x.s.onInteraction(x.s.dg, &ic)
	var v map[string]any
	if err := json.Unmarshal([]byte(x.f.waitFor(t, "POST", path).Body), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

var interactions int

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func choose(t *testing.T, x *harness, user, id string, q int, value string) map[string]any {
	t.Helper()
	return interact(t, x, user, nil, discordgo.InteractionMessageComponent,
		`{"custom_id":"airut-bundle:`+id+`:q`+itoa(q)+`","component_type":3,"values":["`+value+`"]}`)
}

// bundling starts a command that asks b, and returns the bundle's id
// once it is posted, with the post, and a channel with what AskAll
// returned.
func bundling(t *testing.T, b airut.Bundle) (*harness, string, string, chan map[string]airut.Answer, chan error) {
	t.Helper()
	bot := airut.New(airut.Config{})
	got, errs := make(chan map[string]airut.Answer, 1), make(chan error, 1)
	bot.Command("retag", "Retag", func(c *airut.Ctx) error {
		ans, err := c.AskAll(b)
		got <- ans
		errs <- err
		return nil
	})
	x := start(t, bot)
	go x.s.onInteraction(x.s.dg, interaction("retag", nil, nil))
	r := x.f.waitFor(t, "PATCH", "/webhooks/42/tok/messages/@original")
	m := bundleID.FindStringSubmatch(r.Body)
	if m == nil {
		t.Fatalf("the bundle = %s", r.Body)
	}
	return x, m[1], r.Body, got, errs
}

func TestBundleAnswers(t *testing.T) {
	mod := airut.Role{Platform: airut.Discord, ID: "r-mod"}
	x, id, posted, got, errs := bundling(t, airut.Bundle{
		Intro: "Before I retag these:",
		Questions: []airut.Question{
			{Key: "post", Text: "Which post?", Choices: []string{"the troll", "the Tolkien one"}},
			{Key: "rating", Text: "Keep the rating?", Choices: []string{"yes", "no"}},
			{Key: "tag", Text: "What tag instead?"},
		},
		Who: func(a airut.Answerer) bool { return hasRole(a.Roles, mod) },
	})
	// A menu per choice question, and a row with the Answer button; no
	// all-yes, because not every question is yes or no.
	for _, want := range []string{"1. Which post?", `"custom_id":"airut-bundle:` + id + `:q0"`, `"custom_id":"airut-bundle:` + id + `:q1"`, `"custom_id":"airut-bundle:` + id + `:text"`} {
		if !strings.Contains(posted, want) {
			t.Errorf("the bundle lacks %s: %s", want, posted)
		}
	}
	if strings.Contains(posted, ":yes") {
		t.Errorf("all-yes offered with a free-text question: %s", posted)
	}

	// A stranger is turned away; a moderator, whom Who admits, answers.
	if v := choose(t, x, "u2", id, 0, "0"); v["type"] != float64(4) || !strings.Contains(v["data"].(map[string]any)["content"].(string), "someone else") {
		t.Errorf("a stranger's choice got %v", v)
	}
	v := interact(t, x, "u3", []string{"r-mod"}, discordgo.InteractionMessageComponent,
		`{"custom_id":"airut-bundle:`+id+`:q0","component_type":3,"values":["1"]}`)
	d := v["data"].(map[string]any)
	if v["type"] != float64(7) || !strings.Contains(d["content"].(string), "Answer: the Tolkien one") || strings.Contains(mustJSON(d["components"]), ":q0") {
		t.Errorf("the moderator's answer updated the bundle to %v", v)
	}

	// The caller answers the rest: a choice, then the typed answer
	// through the form the Answer button opens.
	choose(t, x, "u1", id, 1, "1")
	if v := interact(t, x, "u1", nil, discordgo.InteractionMessageComponent, `{"custom_id":"airut-bundle:`+id+`:text","component_type":2}`); v["type"] != float64(9) || !strings.Contains(mustJSON(v), `"custom_id":"q2"`) {
		t.Fatalf("the Answer button opened %v", v)
	}
	v = interact(t, x, "u1", nil, discordgo.InteractionModalSubmit,
		`{"custom_id":"airut-bundle:`+id+`:modal","components":[{"type":1,"components":[{"type":4,"custom_id":"q2","value":"  elves  "}]}]}`)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	u1, u3 := airut.User{Platform: airut.Discord, ID: "u1", Name: "u1"}, airut.User{Platform: airut.Discord, ID: "u3", Name: "u3"}
	want := map[string]airut.Answer{"post": {Text: "the Tolkien one", By: u3}, "rating": {Text: "no", By: u1}, "tag": {Text: "elves", By: u1}}
	if g := <-got; !reflect.DeepEqual(g, want) {
		t.Fatalf("answers = %v, want %v", g, want)
	}
	if d := v["data"].(map[string]any); !strings.Contains(d["content"].(string), "Answer: elves") || mustJSON(d["components"]) != "[]" {
		t.Errorf("the last answer left %v", v)
	}
}

func TestBundleAllYesAndTimeout(t *testing.T) {
	yn := []string{"yes", "no"}
	x, id, posted, got, errs := bundling(t, airut.Bundle{
		Questions: []airut.Question{{Key: "a", Text: "Retag 1?", Choices: yn}, {Key: "b", Text: "Retag 2?", Choices: yn}},
	})
	if !strings.Contains(posted, ":yes") || !strings.Contains(posted, ":no") {
		t.Fatalf("no all-yes and all-no for a yes-or-no bundle: %s", posted)
	}
	interact(t, x, "u1", nil, discordgo.InteractionMessageComponent, `{"custom_id":"airut-bundle:`+id+`:yes","component_type":2}`)
	if err := <-errs; err != nil || !reflect.DeepEqual(answerTexts(<-got), map[string]string{"a": "yes", "b": "yes"}) {
		t.Fatalf("all yes: %v", err)
	}

	x, id, _, got, errs = bundling(t, airut.Bundle{
		Questions: []airut.Question{{Key: "a", Text: "Retag 1?", Choices: yn}, {Key: "b", Text: "Retag 2?", Choices: yn}},
		Timeout:   80 * time.Millisecond,
	})
	choose(t, x, "u1", id, 1, "1")
	if err := <-errs; !errors.Is(err, airut.ErrAskTimeout) {
		t.Fatalf("err = %v", err)
	}
	// The unanswered question is missing, never yes.
	if g := answerTexts(<-got); !reflect.DeepEqual(g, map[string]string{"b": "no"}) {
		t.Fatalf("answers = %v", g)
	}
	var closing string
	for _, r := range x.f.sent() {
		if r.Method == "PATCH" && strings.Contains(r.Body, "No answer in time.") {
			closing = r.Body
		}
	}
	if !strings.Contains(closing, `"components":[]`) || !strings.Contains(closing, "Answer: no") {
		t.Fatalf("the bundle was not closed: %s", closing)
	}
	if v := choose(t, x, "u1", id, 0, "0"); !strings.Contains(mustJSON(v), "closed") {
		t.Errorf("a choice after the timeout got %v", v)
	}
}

func TestBundleChoiceLimit(t *testing.T) {
	r := &responder{}
	many := make([]string, maxSelect+1)
	for i := range many {
		many[i] = itoa(i)
	}
	if _, err := r.AskAll(t.Context(), airut.Bundle{Questions: []airut.Question{{Key: "a", Text: "?", Choices: many}}}); err == nil {
		t.Fatal("a menu with too many choices was accepted")
	}
}

func hasRole(rs []airut.Role, r airut.Role) bool {
	for _, x := range rs {
		if x == r {
			return true
		}
	}
	return false
}

func answerTexts(m map[string]airut.Answer) map[string]string {
	out := map[string]string{}
	for k, a := range m {
		out[k] = a.Text
	}
	return out
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// TestBundleShowsTheLatestStateLast holds the answer to the first
// interaction until the bundle has closed, and checks that what is shown
// last is the closed bundle, without controls.
func TestBundleShowsTheLatestStateLast(t *testing.T) {
	yn := []string{"yes", "no"}
	p := &bundle{id: "x", b: airut.Bundle{Questions: []airut.Question{{Key: "a", Text: "A?", Choices: yn}, {Key: "b", Text: "B?", Choices: yn}}},
		answers: map[string]airut.Answer{}, done: make(chan struct{})}
	var mu sync.Mutex
	var shown []string
	record := func(text string, controls []discordgo.MessageComponent) {
		mu.Lock()
		shown = append(shown, text+" controls:"+itoa(len(controls)))
		mu.Unlock()
	}
	entered, release := make(chan struct{}), make(chan struct{})
	p.answers["a"] = airut.Answer{Text: "yes"}
	firstDone := make(chan struct{})
	go func() {
		p.show(func(text string, controls []discordgo.MessageComponent) {
			close(entered)
			<-release
			record(text, controls)
		})
		close(firstDone)
	}()
	<-entered
	p.mu.Lock()
	p.closed, p.end = true, "No answer in time."
	p.mu.Unlock()
	closedDone := make(chan struct{})
	go func() { p.show(record); close(closedDone) }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	<-firstDone
	<-closedDone
	mu.Lock()
	defer mu.Unlock()
	if last := shown[len(shown)-1]; !strings.Contains(last, "B?\n    No answer in time.") || !strings.HasSuffix(last, "controls:0") {
		t.Fatalf("shown last: %q", last)
	}
}

// TestBundleFitsOneMessage fills a bundle to every limit, answers each
// question at the longest a typed answer may be, and checks that every
// question still shows whole and every answer shows, within the limit.
func TestBundleFitsOneMessage(t *testing.T) {
	// Text at the core's limits fits, whether it is letters or emoji,
	// each of which Discord counts as two units.
	for _, wide := range []bool{false, true} {
		long := func(c string, n int) string { return strings.Repeat(c, n) }
		if wide {
			long = func(_ string, n int) string { return strings.Repeat("😀", n/2) }
		}
		var qs []airut.Question
		for i := range airut.MaxBundle {
			qs = append(qs, airut.Question{Key: itoa(i), Text: long(string(rune('a'+i)), airut.MaxQuestion)})
		}
		p := &bundle{id: "x", b: airut.Bundle{Intro: long("i", airut.MaxBundleIntro), Questions: qs}, answers: map[string]airut.Answer{}}
		for _, q := range qs[:3] {
			p.answers[q.Key] = airut.Answer{Text: "zz" + long("z", maxTypedAnswer)}
		}
		for _, end := range []string{"", "No answer in time."} {
			text := p.render(end)
			if units(text) > maxText {
				t.Fatalf("wide %v: the bundle takes %d units, more than %d", wide, units(text), maxText)
			}
			if p.write(end, func(string) string { return "" }) == text {
				t.Fatalf("wide %v: no answer has any room", wide)
			}
			for i, q := range qs {
				if line := itoa(i+1) + ". " + q.Text; !strings.Contains(text, line+"\n") && !strings.HasSuffix(text, line) {
					t.Errorf("wide %v: question %d is cut", wide, i+1)
				}
			}
			if n := strings.Count(text, "Answer: zz"); n != 3 {
				t.Errorf("wide %v: %d answers show, want 3:\n%s", wide, n, text)
			}
			if end != "" && !strings.Contains(text, "\n    "+end) {
				t.Errorf("wide %v: the open question is not marked", wide)
			}
		}
	}
}
