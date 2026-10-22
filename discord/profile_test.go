package discord

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"terva.sh/airut"
)

var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")

func TestConnProfile(t *testing.T) {
	x := start(t, airut.New(airut.Config{}))
	x.f.on("GET", "/users/@me", 200, `{"id": "99", "username": "baphomet", "avatar": "abc123"}`)
	x.f.on("GET", "/applications/@me", 200, `{"id": "1", "description": "Finds posts."}`)
	x.f.on("PATCH", "/users/@me", 200, `{"id": "99", "username": "Baphomet", "avatar": "def456"}`)
	x.f.on("PATCH", "/applications/@me", 200, `{"id": "1"}`)
	ctx := context.Background()

	p, err := x.s.Profile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "baphomet" || p.About != "Finds posts." || !strings.Contains(p.Avatar, "/avatars/99/abc123") {
		t.Errorf("Profile = %+v", p)
	}

	name, about := "Baphomet", "The archive's librarian."
	if err := x.s.SetProfile(ctx, airut.ProfileChange{Name: &name, About: &about, Avatar: &airut.Image{Data: png, Type: "image/png"}}); err != nil {
		t.Fatal(err)
	}
	var user struct{ Username, Avatar string }
	if err := json.Unmarshal([]byte(x.f.waitFor(t, "PATCH", "/users/@me").Body), &user); err != nil {
		t.Fatal(err)
	}
	if user.Username != "Baphomet" || user.Avatar != "data:image/png;base64,"+base64.StdEncoding.EncodeToString(png) {
		t.Errorf("the user update sent %+v", user)
	}
	if body := x.f.waitFor(t, "PATCH", "/applications/@me").Body; !strings.Contains(body, `"description":"The archive's librarian."`) {
		t.Errorf("the application update sent %s", body)
	}
}

func TestConnProfileSkipsWhatIsAlreadySet(t *testing.T) {
	x := start(t, airut.New(airut.Config{}))
	x.f.on("GET", "/users/@me", 200, `{"id": "99", "username": "Baphomet"}`)
	x.f.on("GET", "/applications/@me", 200, `{"id": "1", "description": "Finds posts."}`)
	ctx := context.Background()

	name, about := "Baphomet", "Finds posts."
	if err := x.s.SetProfile(ctx, airut.ProfileChange{Name: &name, About: &about}); err != nil {
		t.Fatal(err)
	}
	for _, r := range x.f.sent() {
		if r.Method == "PATCH" {
			t.Errorf("sent %s %s for a profile already set", r.Method, r.Path)
		}
	}

	// A change no platform takes is refused before any request.
	before := len(x.f.sent())
	blank := " "
	if err := x.s.SetProfile(ctx, airut.ProfileChange{Name: &blank}); err == nil {
		t.Error("a blank name was accepted")
	}
	if err := x.s.SetProfile(ctx, airut.ProfileChange{Avatar: &airut.Image{Data: []byte("text"), Type: "image/png"}}); err == nil || errors.Is(err, airut.ErrUnsupported) {
		t.Errorf("a mislabelled picture = %v", err)
	}
	if n := len(x.f.sent()); n != before {
		t.Errorf("%d requests for refused changes", n-before)
	}
}
