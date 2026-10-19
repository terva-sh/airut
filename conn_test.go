package airut_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

var mods = airut.Channel{Platform: airuttest.Platform, ID: "mods"}

func TestConnSendsElsewhereAndOpensAttachments(t *testing.T) {
	b, _ := newBot(airut.Config{})
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		conn, err := c.Conn()
		if err != nil {
			return err
		}
		for _, f := range m.Files {
			r, err := conn.Open(c.Context(), f)
			if err != nil {
				return err
			}
			data, err := io.ReadAll(io.LimitReader(r, 1<<20))
			r.Close()
			if err != nil {
				return err
			}
			// Post to another channel, as a reply to nothing there, and
			// answer the original message from elsewhere too.
			if _, err := conn.Send(c.Context(), mods, airut.Message{Text: fmt.Sprintf("review %s: %d bytes", f.Name, len(data))}); err != nil {
				return err
			}
			if _, err := conn.Send(c.Context(), c.Channel(), airut.Message{Text: "uploaded", ReplyTo: m.Message}); err != nil {
				return err
			}
		}
		return nil
	})
	a := airuttest.Start(t, b)
	a.AddFile("mxc://example.org/cat", []byte("PNGDATA"))

	evs := a.Text("", airuttest.WithFiles(airut.Attachment{Name: "cat.png", URL: "mxc://example.org/cat"}))
	if len(evs) != 2 {
		t.Fatalf("events = %+v", evs)
	}
	if e := evs[0]; e.Kind != "send" || e.Channel != "mods" || e.Text != "review cat.png: 7 bytes" {
		t.Errorf("the review post was %+v", e)
	}
	if e := evs[1]; e.Kind != "send" || e.Channel != "channel-1" || e.ReplyTo == "" || e.Text != "uploaded" {
		t.Errorf("the reply was %+v", e)
	}
}

func TestConnHistoryWalksToTheStartAndResumes(t *testing.T) {
	b, _ := newBot(airut.Config{})
	a := airuttest.Start(t, b)
	ch := airut.Channel{Platform: airuttest.Platform, ID: "channel-1"}
	for i := range 25 {
		a.AddHistory(ch.ID, airut.MessageEvent{ID: fmt.Sprint(i), Message: airut.MessageRef{ID: fmt.Sprintf("$m%d", i)}, Text: fmt.Sprint(i)})
	}
	conn, err := b.Conn(airuttest.Platform)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var seen []string
	var sizes []int
	cursor := ""
	var saved string
	for {
		page, err := conn.History(ctx, ch, cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(page.Messages))
		for _, m := range page.Messages {
			seen = append(seen, m.Text)
		}
		if saved == "" {
			saved = page.Next
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if fmt.Sprint(sizes) != "[10 10 5]" || seen[0] != "24" || seen[len(seen)-1] != "0" || len(seen) != 25 {
		t.Fatalf("walked pages %v: %v; want 25 messages newest first in pages of 10", sizes, seen)
	}
	// A walk resumed from a saved cursor, as after a restart, continues
	// where the first page ended.
	page, err := conn.History(ctx, ch, saved, 10)
	if err != nil || len(page.Messages) != 10 || page.Messages[0].Text != "14" {
		t.Fatalf("resumed page = %+v, %v; want it to start at 14", page.Messages, err)
	}
}

func TestConnMembership(t *testing.T) {
	b, _ := newBot(airut.Config{})
	b.Command("tag", "Mods only", func(c *airut.Ctx) error {
		conn, err := c.Conn()
		if err != nil {
			return err
		}
		ok, err := conn.Member(c.Context(), mods, c.Caller())
		if err != nil {
			return err
		}
		if !ok {
			return airut.Errorf("Only moderators can edit tags.")
		}
		_, err = c.Reply("tagged")
		return err
	})
	a := airuttest.Start(t, b)
	a.SetMembers("mods", "mod-1")
	wantEvents(t, a.Text("!tag", airuttest.As(airut.User{Platform: airuttest.Platform, ID: "mod-1"})), "reply:tagged")
	wantEvents(t, a.Text("!tag"), "reply:Only moderators can edit tags.")
}

func TestOnReady(t *testing.T) {
	b, logs := newBot(airut.Config{})
	got := make(chan airut.Conn, 1)
	stopped := make(chan struct{})
	b.OnReady(func(ctx context.Context, c airut.Conn) {
		got <- c
		// A long-lived job, such as a resumed backfill, runs until the
		// bot stops.
		<-ctx.Done()
		close(stopped)
	})
	b.OnReady(func(context.Context, airut.Conn) { panic("boom") })

	a := airuttest.New("")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx, a) }()
	<-a.Ready()

	select {
	case c := <-got:
		if c.Platform() != airuttest.Platform {
			t.Errorf("OnReady got a Conn for %s", c.Platform())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnReady never ran")
	}
	if c, err := b.Conn(airuttest.Platform); err != nil || c == nil {
		t.Errorf("Bot.Conn = %v, %v", c, err)
	}
	if _, err := b.Conn(airut.Discord); !errors.Is(err, airut.ErrNoConn) {
		t.Errorf("Bot.Conn for a platform with no adapter: %v, want ErrNoConn", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	default:
		t.Error("Run returned before its OnReady handler finished")
	}
	if !strings.Contains(logs.String(), "ready handler panicked") {
		t.Errorf("a panicking ready handler was not logged:\n%s", logs.String())
	}

	defer func() {
		if recover() == nil {
			t.Error("OnReady after Run did not panic")
		}
	}()
	b.OnReady(func(context.Context, airut.Conn) {})
}
