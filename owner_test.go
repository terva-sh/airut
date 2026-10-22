package airut_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

// owned lists owner, from settings_test.go, as the test platform's
// owner.
var owned = map[airut.Platform][]string{airuttest.Platform: {owner.ID}}

const (
	claimReply  = "reply:This bot has no owner: claim it with airut claim on the host."
	ownerWarned = "no owner configured; set [bot] owners or claim the bot with airut claim"
)

func TestOwnerAdmitsOnlyListedCallers(t *testing.T) {
	b, _ := newBot(airut.Config{Owners: map[airut.Platform][]string{
		airuttest.Platform: {owner.ID}, airut.Matrix: {"@someone:example.org"},
	}})
	b.Command("secret", "Owners only", func(c *airut.Ctx) error {
		_, err := c.Reply("yes")
		return err
	}).RestrictTo(airut.Owner)
	a := airuttest.Start(t, b)

	wantEvents(t, a.Text("!secret", asOwner), "reply:yes")
	wantEvents(t, a.Text("!secret"), "reply:You are not allowed to use !secret.")
	// An id listed for another platform is not an owner here.
	wantEvents(t, a.Text("!secret", airuttest.As(airut.User{Platform: airuttest.Platform, ID: "@someone:example.org"})),
		"reply:You are not allowed to use !secret.")

	ctx := context.Background()
	for p, want := range map[airut.Platform]bool{airuttest.Platform: true, airut.Matrix: true, airut.Discord: false} {
		if got, err := b.HasOwner(ctx, p); got != want || err != nil {
			t.Errorf("HasOwner(%s) = %v, %v; want %v", p, got, err, want)
		}
	}
}

func TestOwnersAreCopied(t *testing.T) {
	list := map[airut.Platform][]string{airuttest.Platform: {owner.ID}}
	b, _ := newBot(airut.Config{Owners: list, ProfileEditors: airut.Owner})
	list[airuttest.Platform][0] = "user-1"
	a := airuttest.Start(t, b)
	if evs := a.Text("!profile", asOwner); len(evs) != 1 || !strings.Contains(evs[0].Text, "Name:") {
		t.Errorf("the owner, after the caller's map changed: %q", texts(evs))
	}
	wantEvents(t, a.Text("!profile"), "reply:You are not allowed to use !profile.")
}

func TestOwnersRefuseAnEmptyID(t *testing.T) {
	defer func() {
		if r := recover(); r == nil || !strings.Contains(r.(string), "empty id") {
			t.Errorf("recover = %v, want a panic naming the empty id", r)
		}
	}()
	airut.New(airut.Config{Owners: map[airut.Platform][]string{airut.Matrix: {" "}}})
}

func TestByPlatform(t *testing.T) {
	got := airut.ByPlatform(map[string][]string{"matrix": {"@you:example.org"}, "discord": {"123"}})
	want := map[airut.Platform][]string{airut.Matrix: {"@you:example.org"}, airut.Discord: {"123"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ByPlatform = %q, want %q", got, want)
	}
	if airut.ByPlatform[[]string](nil) != nil {
		t.Error("ByPlatform(nil) is not nil")
	}
}

func TestAnUnownedBotWarnsAtStart(t *testing.T) {
	b, logs := newBot(airut.Config{})
	airuttest.Start(t, b)
	l := logs.String()
	if strings.Count(l, ownerWarned) != 1 || !strings.Contains(l, "level=WARN") || !strings.Contains(l, "platforms=test") {
		t.Errorf("an unowned bot's log:\n%s", l)
	}

	b, logs = newBot(airut.Config{Owners: owned})
	airuttest.Start(t, b)
	if l := logs.String(); strings.Contains(l, ownerWarned) {
		t.Errorf("an owned bot warned:\n%s", l)
	}

	// Owners on a platform the bot does not serve leave it unowned, and
	// the warning never names one.
	b, logs = newBot(airut.Config{Owners: map[airut.Platform][]string{airut.Matrix: {"@you:example.org"}}})
	airuttest.Start(t, b)
	if l := logs.String(); !strings.Contains(l, ownerWarned) || strings.Contains(l, "@you:example.org") {
		t.Errorf("a bot owned only elsewhere:\n%s", l)
	}
}

func TestStatusCommand(t *testing.T) {
	b, logs := newBot(airut.Config{Version: "v1.3.0", Owners: map[airut.Platform][]string{
		airuttest.Platform: {owner.ID}, airut.Matrix: {"@you:example.org"},
	}})
	a := airuttest.Start(t, b)
	if got := specNames(a); !reflect.DeepEqual(got, []string{"roll", "status", "version"}) {
		t.Fatalf("specs = %q", got)
	}

	evs := a.Text("!status")
	wantEvents(t, evs, "reply:Running v1.3.0.\nPlatforms:\n- test: has an owner")
	if len(evs) == 1 && !evs[0].Private {
		t.Error("status was not private")
	}
	evs = a.Text("!status", asOwner)
	wantEvents(t, evs, "reply:Running v1.3.0.\nPlatforms:\n- test: has an owner\n"+
		"Owners:\n- matrix @you:example.org (config)\n- test owner (config)")
	if len(evs) == 1 && !evs[0].Private {
		t.Error("an owner's status was not private")
	}
	defer func() {
		if recover() == nil {
			t.Error("Tools handed out the status command")
		}
	}()
	if l := logs.String(); strings.Contains(l, "@you:example.org") {
		t.Errorf("the log holds an owner id:\n%s", l)
	}
	b.Tools("status")
}

func TestStatusOfAnUnownedBot(t *testing.T) {
	b, _ := newBot(airut.Config{})
	a := airuttest.Start(t, b)
	// Without a Version there is no version line.
	wantEvents(t, a.Text("!status"), "reply:Platforms:\n- test: no owner: claim it with airut claim on the host")
	wantEvents(t, a.Slash("status", nil, asOwner), "reply:Platforms:\n- test: no owner: claim it with airut claim on the host")
}

func TestAppStatusCommandWins(t *testing.T) {
	b, _ := newBot(airut.Config{})
	b.Command("status", "The bot's own status", func(c *airut.Ctx) error {
		_, err := c.Reply("the bot's own status")
		return err
	})
	wantEvents(t, airuttest.Start(t, b).Text("!status"), "reply:the bot's own status")
}

func TestADirectMessageToAnUnownedBotGetsTheClaimLine(t *testing.T) {
	b, logs := newBot(airut.Config{})
	var seen []string
	b.OnMessage(func(c *airut.Ctx, ev airut.MessageEvent) error {
		seen = append(seen, ev.Text)
		return nil
	})
	a := airuttest.Start(t, b)
	dana := airuttest.As(airut.User{Platform: airuttest.Platform, ID: "dana"})

	wantEvents(t, a.DirectMessage("hello, private words"), claimReply)
	wantEvents(t, a.DirectMessage("hello again"))
	wantEvents(t, a.DirectMessage("hi", dana), claimReply)
	wantEvents(t, a.DirectMessage("hi again", dana))
	// A command in a direct message runs it, with no claim line, and a
	// message in a shared channel is not answered.
	wantEvents(t, a.DirectMessage("!roll 1d6", airuttest.As(airut.User{Platform: airuttest.Platform, ID: "erin"})), "reply:rolled 1d6 x1")
	wantEvents(t, a.Text("hello room", airuttest.As(airut.User{Platform: airuttest.Platform, ID: "frank"})))
	// The handlers still see every message.
	if want := []string{"hello, private words", "hello again", "hi", "hi again", "hello room"}; !reflect.DeepEqual(seen, want) {
		t.Errorf("handlers saw %q, want %q", seen, want)
	}

	l := logs.String()
	if strings.Count(l, "told a direct message's sender the bot has no owner") != 2 {
		t.Errorf("the claim lines are not logged once each:\n%s", l)
	}
	for _, text := range []string{"private words", "hello again", "hello room", "dana"} {
		if strings.Contains(l, text) {
			t.Errorf("the log holds %q:\n%s", text, l)
		}
	}

	// A bot with no handlers answers too, and an owned bot does not.
	b, _ = newBot(airut.Config{})
	wantEvents(t, airuttest.Start(t, b).DirectMessage("hello"), claimReply)
	b, _ = newBot(airut.Config{Owners: owned})
	wantEvents(t, airuttest.Start(t, b).DirectMessage("hello"))
}
