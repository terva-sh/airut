package discord

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

type everyKind struct {
	Mode  string           `desc:"Which mode" choices:"fast,slow"`
	Count int              `desc:"How many" min:"1" max:"10"`
	Scale float64          `desc:"How much" min:"-2.5" max:"0"`
	Loud  bool             `desc:"Whether to shout"`
	Who   airut.User       `desc:"Whom to ask"`
	Where airut.Channel    `desc:"Where to post"`
	Group airut.Role       `desc:"Which role"`
	Image airut.Attachment `desc:"An image"`
	Note  *string          `desc:"A note" min:"0" max:"200"`
}

func specs(t *testing.T) []airut.Spec {
	t.Helper()
	b := airut.New(airut.Config{})
	b.Command("every", "Take every kind", func(*airut.Ctx, everyKind) error { return nil })
	b.Command("ping", "Answer pong", func(*airut.Ctx) error { return nil })
	return airuttest.Start(t, b).Specs()
}

func TestCommands(t *testing.T) {
	cmds, err := commands(specs(t))
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(cmds)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"type":1,"name":"every","description":"Take every kind","options":[` +
		`{"type":3,"name":"mode","description":"Which mode","required":true,"choices":[{"name":"fast","value":"fast"},{"name":"slow","value":"slow"}]},` +
		`{"type":4,"name":"count","description":"How many","required":true,"min_value":1,"max_value":10},` +
		`{"type":10,"name":"scale","description":"How much","required":true,"min_value":-2.5,"max_value":0},` +
		`{"type":5,"name":"loud","description":"Whether to shout","required":true},` +
		`{"type":6,"name":"who","description":"Whom to ask","required":true},` +
		`{"type":7,"name":"where","description":"Where to post","required":true},` +
		`{"type":8,"name":"group","description":"Which role","required":true},` +
		`{"type":11,"name":"image","description":"An image","required":true},` +
		`{"type":3,"name":"note","description":"A note","min_length":0,"max_length":200}]},` +
		`{"type":1,"name":"ping","description":"Answer pong"}]`
	if string(got) != want {
		t.Errorf("commands =\n%s\nwant\n%s", got, want)
	}
}

func TestEveryKindHasAnOptionType(t *testing.T) {
	for k := airut.KindString; k <= airut.KindAttachment; k++ {
		if _, ok := optionTypes[k]; !ok {
			t.Errorf("kind %s has no option type", k)
		}
	}
	_, err := commands([]airut.Spec{{Name: "x", Description: "x", Params: []airut.Param{{Name: "p", Description: "p", Kind: airut.Kind(99)}}}})
	if err == nil || !strings.Contains(err.Error(), "has no Discord option type") {
		t.Errorf("err = %v", err)
	}
}

func TestRegister(t *testing.T) {
	cmds, err := commands(specs(t))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(cmds)
	for _, c := range []struct {
		name   string
		guilds []string
		paths  []string
	}{
		{"global", nil, []string{"/applications/42/commands"}},
		{"guilds", []string{"7", "8"}, []string{"/applications/42/guilds/7/commands", "/applications/42/guilds/8/commands"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeDiscord()
			for _, p := range c.paths {
				f.on("PUT", p, 200, "[]")
			}
			if err := register(context.Background(), f.session(t), "42", c.guilds, cmds); err != nil {
				t.Fatal(err)
			}
			sent := f.sent()
			if len(sent) != len(c.paths) {
				t.Fatalf("sent %d requests, want %d: %v", len(sent), len(c.paths), sent)
			}
			for i, r := range sent {
				if r.Method != "PUT" || r.Path != c.paths[i] || r.Body != string(body) {
					t.Errorf("request %d = %s %s %s, want PUT %s with the commands", i, r.Method, r.Path, r.Body, c.paths[i])
				}
			}
		})
	}
}

func TestRegisterAgain(t *testing.T) {
	// Starting twice sends the same whole set twice, which Discord takes
	// as a replacement, not an addition.
	cmds, _ := commands(specs(t))
	f := newFakeDiscord()
	f.on("PUT", "/applications/42/commands", 200, "[]")
	s := f.session(t)
	for range 2 {
		if err := register(context.Background(), s, "42", nil, cmds); err != nil {
			t.Fatal(err)
		}
	}
	sent := f.sent()
	if len(sent) != 2 || sent[0] != sent[1] {
		t.Errorf("sent = %v", sent)
	}
}

func TestRegisterFails(t *testing.T) {
	cmds, _ := commands(specs(t))
	f := newFakeDiscord()
	f.on("PUT", "/applications/42/guilds/7/commands", 200, "[]")
	f.on("PUT", "/applications/42/guilds/8/commands", 403, `{"message": "Missing Access", "code": 50001}`)
	err := register(context.Background(), f.session(t), "42", []string{"7", "8"}, cmds)
	if err == nil || !strings.Contains(err.Error(), "in guild 8") {
		t.Errorf("err = %v", err)
	}
}
