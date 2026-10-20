package discord

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"terva.sh/airut"
)

var askButton = regexp.MustCompile(`"custom_id":"(airut-ask:[0-9a-f]+):(\d+)"`)

// press delivers a button press by user on the button custom names.
func press(x *harness, user, custom string) { pressOn(x, user, custom, nil) }

// pressOn is press on the message m, as Discord sends it.
func pressOn(x *harness, user, custom string, m *discordgo.Message) {
	x.s.onInteraction(x.s.dg, &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Message: m,
		ID:      "b-" + user, AppID: "42", Token: "tok-" + user, Type: discordgo.InteractionMessageComponent,
		GuildID: "g1", ChannelID: "c1",
		Member: &discordgo.Member{User: &discordgo.User{ID: user}},
		Data:   discordgo.MessageComponentInteractionData{CustomID: custom, ComponentType: discordgo.ButtonComponent},
	}})
}

// asking starts a command that asks, and returns the ask's id once the
// question is posted, and a channel with what Ask returned.
func asking(t *testing.T, timeout time.Duration) (*harness, string, chan error, chan string) {
	t.Helper()
	b := airut.New(airut.Config{})
	errs, answers := make(chan error, 1), make(chan string, 1)
	b.Command("pick", "Pick a colour", func(c *airut.Ctx) error {
		got, err := c.AskWith(airut.Ask{Question: "Which colour?", Choices: []string{"red", "blue"}, Timeout: timeout})
		errs <- err
		answers <- got
		return nil
	})
	x := start(t, b)
	for _, u := range []string{"u1", "u2"} {
		x.f.on("POST", "/interactions/b-"+u+"/tok-"+u+"/callback", 204, "")
	}
	go x.s.onInteraction(x.s.dg, interaction("pick", nil, nil))
	r := x.f.waitFor(t, "PATCH", "/webhooks/42/tok/messages/@original")
	m := askButton.FindAllStringSubmatch(r.Body, -1)
	if len(m) != 2 || !strings.Contains(r.Body, `"label":"red"`) || !strings.Contains(r.Body, "Which colour?") {
		t.Fatalf("question = %s", r.Body)
	}
	return x, m[0][1], errs, answers
}

func callback(t *testing.T, x *harness, user string) map[string]any {
	t.Helper()
	r := x.f.waitFor(t, "POST", "/interactions/b-"+user+"/tok-"+user+"/callback")
	var v map[string]any
	if err := json.Unmarshal([]byte(r.Body), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAskReturnsTheCallersChoice(t *testing.T) {
	x, id, errs, answers := asking(t, time.Minute)
	press(x, "u2", id+":0")
	v := callback(t, x, "u2")
	d, _ := v["data"].(map[string]any)
	if v["type"] != float64(4) || d["flags"] != float64(64) || !strings.Contains(d["content"].(string), "someone else") {
		t.Errorf("a stranger's press got %v", v)
	}
	press(x, "u1", id+":1")
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if got := <-answers; got != "blue" {
		t.Errorf("answer = %q", got)
	}
	v = callback(t, x, "u1")
	d, _ = v["data"].(map[string]any)
	if v["type"] != float64(7) || !strings.HasSuffix(d["content"].(string), "Answer: blue") {
		t.Errorf("the answer updated the question to %v", v)
	}
	if c, ok := d["components"].([]any); !ok || len(c) != 0 {
		t.Errorf("the answer left the buttons: %v", d["components"])
	}
}

func TestAskTimesOut(t *testing.T) {
	x, id, errs, _ := asking(t, 50*time.Millisecond)
	if err := <-errs; !errors.Is(err, airut.ErrAskTimeout) {
		t.Fatalf("err = %v", err)
	}
	var closing *fakeRequest
	for _, r := range x.f.sent() {
		if r.Method == "PATCH" && strings.Contains(r.Body, "No answer in time.") {
			closing = &r
		}
	}
	if closing == nil || !strings.Contains(closing.Body, `"components":[]`) {
		t.Fatalf("the question was not closed: %v", x.f.sent())
	}
	press(x, "u1", id+":0")
	v := callback(t, x, "u1")
	d, _ := v["data"].(map[string]any)
	if !strings.Contains(d["content"].(string), "closed") {
		t.Errorf("a press after the timeout got %v", v)
	}
}

func TestAskChoiceLimits(t *testing.T) {
	r := &responder{}
	for _, n := range []int{0, 26} {
		if _, err := r.Ask(t.Context(), airut.Ask{Question: "?", Choices: make([]string, n)}); err == nil {
			t.Errorf("an ask with %d choices was accepted", n)
		}
	}
}

func TestButtons(t *testing.T) {
	choices := []string{"a", "b", "c", "d", "e", "f", strings.Repeat("g", 100)}
	rows := buttons("x", choices)
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	first, second := rows[0].(discordgo.ActionsRow), rows[1].(discordgo.ActionsRow)
	if len(first.Components) != 5 || len(second.Components) != 2 {
		t.Errorf("rows hold %d and %d buttons", len(first.Components), len(second.Components))
	}
	last := second.Components[1].(discordgo.Button)
	if units(last.Label) != maxLabel || last.CustomID != "airut-ask:x:6" {
		t.Errorf("last button = %+v", last)
	}
}

func TestTruncate(t *testing.T) {
	for _, c := range []struct {
		in    string
		limit int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"a bit too long", 10, "a bit too…"},
		{"😀😀😀", 4, "😀…"},
		{"\xff\xff\xff\xff", 3, "\xff\xff…"},
	} {
		if got := truncate(c.in, c.limit); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.limit, got, c.want)
		}
	}
}

func TestAPressOnAForgottenAskClosesIt(t *testing.T) {
	x := start(t, airut.New(airut.Config{}))
	x.f.on("POST", "/interactions/b-u1/tok-u1/callback", 204, "")
	pressOn(x, "u1", askPrefix+"0123456789abcdef:0", &discordgo.Message{ID: "m5", Content: "Which colour?"})
	v := callback(t, x, "u1")
	d, _ := v["data"].(map[string]any)
	if v["type"] != float64(7) || d["content"] != "Which colour?\n\nThis question is closed." {
		t.Errorf("a press on a forgotten ask got %v", v)
	}
	if c, ok := d["components"].([]any); !ok || len(c) != 0 {
		t.Errorf("the buttons stayed: %v", d["components"])
	}
}

func TestALatePressLeavesTheOutcome(t *testing.T) {
	x, id, errs, _ := asking(t, time.Minute)
	m := &discordgo.Message{ID: "orig", Content: "Which colour?"}
	pressOn(x, "u1", id+":1", m)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	// A second press, by the same person or another, was already on its
	// way: it is told the question is closed and rewrites nothing.
	pressOn(x, "u2", id+":0", m)
	v := callback(t, x, "u2")
	d, _ := v["data"].(map[string]any)
	if v["type"] != float64(4) || d["flags"] != float64(64) || !strings.Contains(d["content"].(string), "closed") {
		t.Errorf("a late press got %v", v)
	}
}

func TestAPressAfterTheTimeoutLeavesTheOutcome(t *testing.T) {
	x, id, errs, _ := asking(t, 50*time.Millisecond)
	if err := <-errs; !errors.Is(err, airut.ErrAskTimeout) {
		t.Fatalf("err = %v", err)
	}
	pressOn(x, "u1", id+":0", &discordgo.Message{ID: "orig", Content: "Which colour?\n\nNo answer in time."})
	v := callback(t, x, "u1")
	if v["type"] != float64(4) {
		t.Errorf("a press after the timeout rewrote the message: %v", v)
	}
}

func TestALongChoiceStillFits(t *testing.T) {
	long := strings.Repeat("c", 3000)
	text := closed(strings.Repeat("q", 3000), "Answer: "+long)
	if units(text) > maxText || !strings.Contains(text, "Answer: ccc") || !strings.HasPrefix(text, "qqq") {
		t.Errorf("closed is %d units: %.40q", units(text), text)
	}
}
