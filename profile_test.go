package airut_test

import (
	"strings"
	"testing"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

// png is enough of a PNG file for its type to be sniffed.
var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00")

func ptr[T any](v T) *T { return &v }

func TestProfileChangeCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    airut.ProfileChange
		ok   bool
	}{
		{"nothing", airut.ProfileChange{}, true},
		{"name", airut.ProfileChange{Name: ptr("Baphomet")}, true},
		{"blank name", airut.ProfileChange{Name: ptr("  ")}, false},
		{"long name", airut.ProfileChange{Name: ptr(strings.Repeat("é", 33))}, false},
		{"name of 32", airut.ProfileChange{Name: ptr(strings.Repeat("é", 32))}, true},
		{"name with a newline", airut.ProfileChange{Name: ptr("Bapho\nmet")}, false},
		{"empty about clears", airut.ProfileChange{About: ptr("")}, true},
		{"long about", airut.ProfileChange{About: ptr(strings.Repeat("a", 401))}, false},
		{"png", airut.ProfileChange{Avatar: &airut.Image{Data: png, Type: "image/png"}}, true},
		{"mislabelled", airut.ProfileChange{Avatar: &airut.Image{Data: png, Type: "image/jpeg"}}, false},
		{"not an image", airut.ProfileChange{Avatar: &airut.Image{Data: []byte("<svg/>"), Type: "image/svg+xml"}}, false},
		{"empty image", airut.ProfileChange{Avatar: &airut.Image{Type: "image/png"}}, false},
		{"too large", airut.ProfileChange{Avatar: &airut.Image{Data: append(png, make([]byte, airut.MaxAvatarBytes)...), Type: "image/png"}}, false},
	} {
		if err := tc.p.Check(); (err == nil) != tc.ok {
			t.Errorf("%s: Check() = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
}

var asOwner = airuttest.As(owner)

func TestProfileCommand(t *testing.T) {
	b, logs := newBot(airut.Config{ProfileEditors: isOwner})
	a := airuttest.Start(t, b)
	a.AddFile("https://files.example.org/face.png", png)
	a.AddFile("https://files.example.org/notes.txt", []byte("not a picture"))

	wantEvents(t, a.Text("!profile name Baphomet", asOwner), "profile:name", "reply:The bot's name here is changed.")
	wantEvents(t, a.Text("!profile about The archive's librarian.", asOwner), "profile:about", "reply:The bot's about here is changed.")
	face := airut.Attachment{Name: "face.png", ContentType: "image/png", Size: int64(len(png)), URL: "https://files.example.org/face.png"}
	wantEvents(t, a.Text("!profile avatar", asOwner, airuttest.WithFiles(face)), "profile:avatar", "reply:The bot's avatar here is changed.")

	p := a.BotProfile()
	if p.Name != "Baphomet" || p.About != "The archive's librarian." || p.Avatar == "" {
		t.Errorf("profile = %+v", p)
	}
	evs := a.Text("!profile", asOwner)
	if len(evs) != 1 || !evs[0].Private || !strings.Contains(evs[0].Text, "Name: Baphomet") {
		t.Errorf("showing the profile: %+v", evs)
	}

	// The platform's word on an attachment's type is not taken.
	notes := airut.Attachment{Name: "face.png", ContentType: "image/png", Size: 13, URL: "https://files.example.org/notes.txt"}
	evs = a.Text("!profile avatar", asOwner, airuttest.WithFiles(notes))
	if len(evs) != 1 || !strings.Contains(evs[0].Text, "is not a profile avatar") {
		t.Errorf("a text file as an avatar: %+v", evs)
	}
	wantEvents(t, a.Text("!profile avatar", asOwner), "reply:Attach the new picture to the command.")
	if evs := a.Text("!profile name", asOwner); len(evs) != 1 || evs[0].Kind != "reply" || !strings.Contains(evs[0].Text, "1 to 32") {
		t.Errorf("an empty name: %+v", evs)
	}

	wantEvents(t, a.Text("!profile name Mallory"), "reply:You are not allowed to use !profile.")
	if a.BotProfile().Name != "Baphomet" {
		t.Errorf("someone else renamed the bot to %q", a.BotProfile().Name)
	}

	l := logs.String()
	if strings.Contains(l, "Baphomet") || strings.Contains(l, "librarian") || !strings.Contains(l, "profile changed") {
		t.Errorf("the log holds profile text, or no change:\n%s", l)
	}
}

func TestNoProfileCommandWithoutEditors(t *testing.T) {
	b, _ := newBot(airut.Config{})
	a := airuttest.Start(t, b)
	a.Text("!profile name Mallory")
	if a.BotProfile().Name != "" {
		t.Errorf("the profile command ran without ProfileEditors")
	}
}

func TestProfileCommandIsNeverATool(t *testing.T) {
	b, _ := newBot(airut.Config{ProfileEditors: isOwner})
	airuttest.Start(t, b)
	defer func() {
		if recover() == nil {
			t.Error("Tools handed out the profile command")
		}
	}()
	b.Tools("profile")
}
