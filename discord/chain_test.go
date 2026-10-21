package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"terva.sh/airut"
)

func reply(m *discordgo.Message, to string) *discordgo.Message {
	m.Type = discordgo.MessageTypeReply
	m.MessageReference = &discordgo.MessageReference{MessageID: to, ChannelID: m.ChannelID}
	return m
}

func TestReplyChain(t *testing.T) {
	b := airut.New(airut.Config{})
	got := make(chan airut.MessageEvent, 4)
	b.OnMessage(func(c *airut.Ctx, ev airut.MessageEvent) error {
		got <- ev
		return nil
	})
	x := start(t, b)
	x.s.me = "bot"
	// m4 replies to the bot's m3, which replies to m2, which replies to
	// m1, which the bot may not read. The gateway carries m3; the rest
	// is fetched.
	x.f.on("GET", "/channels/c1/messages/m2", 200, `{"id": "m2", "channel_id": "c1", "type": 19, "content": "which post?",
		"author": {"id": "u1"}, "message_reference": {"message_id": "m1", "channel_id": "c1"}, "timestamp": "2027-01-15T07:00:00Z"}`)
	x.f.on("GET", "/channels/c1/messages/m1", 403, `{"message": "Missing Access", "code": 50001}`)
	m3 := reply(message("m3", "bot", "the troll, or the Tolkien one?"), "m2")
	m4 := reply(message("m4", "u1", "<@bot> the other one"), "m3")
	m4.Mentions = []*discordgo.User{{ID: "bot"}}
	m4.ReferencedMessage = m3
	x.s.onMessage(x.s.dg, &discordgo.MessageCreate{Message: m4})
	// Ordinary chat carries no chain, and costs no request.
	plain := reply(message("m5", "u1", "nice"), "m3")
	plain.ReferencedMessage = m3
	x.s.onMessage(x.s.dg, &discordgo.MessageCreate{Message: plain})
	x.s.stop()

	byID := map[string]airut.MessageEvent{}
	for range 2 {
		ev := <-got
		byID[ev.ID] = ev
	}
	ev := byID["m4"]
	if len(ev.ReplyChain) != 2 || ev.ReplyChain[0].Ref.ID != "m3" || !ev.ReplyChain[0].FromBot ||
		ev.ReplyChain[1].Text != "which post?" || ev.ReplyChain[1].FromBot || ev.ReplyChain[1].Sent.IsZero() {
		t.Fatalf("chain = %+v", ev.ReplyChain)
	}
	if ev := byID["m5"]; ev.ReplyChain != nil || ev.ReplyTo.ID != "m3" {
		t.Fatalf("ordinary chat carried %+v", ev.ReplyChain)
	}
	fetched := 0
	for _, r := range x.f.sent() {
		if r.Method == "GET" && strings.HasPrefix(r.Path, "/channels/c1/messages/") {
			fetched++
		}
	}
	if fetched != 2 {
		t.Errorf("fetched %d messages, want m2 and m1", fetched)
	}

	// The depth bounds the chain.
	b2 := airut.New(airut.Config{})
	b2.OnMessage(func(c *airut.Ctx, ev airut.MessageEvent) error {
		got <- ev
		return nil
	})
	x2 := start(t, b2)
	x2.f.on("GET", "/channels/c1/messages/m2", 200, `{"id": "m2", "channel_id": "c1", "type": 0, "content": "which post?", "author": {"id": "u1"}}`)
	x2.s.me, x2.s.replyDepth = "bot", 1
	x2.s.onMessage(x2.s.dg, &discordgo.MessageCreate{Message: m4})
	x2.s.stop()
	if ev := <-got; len(ev.ReplyChain) != 1 {
		t.Fatalf("a depth of 1 gave %d links", len(ev.ReplyChain))
	}
}

func TestFetchAndHistoryWithTheBot(t *testing.T) {
	x := start(t, airut.New(airut.Config{}))
	x.s.me = "bot"
	x.f.on("GET", "/channels/c1/messages/m2", 200, `{"id": "m2", "channel_id": "c1", "type": 0, "content": "mine, edited", "author": {"id": "bot"}, "timestamp": "2027-01-15T07:00:00Z"}`)
	m, err := x.s.Fetch(context.Background(), airut.Channel{ID: "c1"}, airut.MessageRef{ID: "m2"})
	if err != nil || !m.FromBot || m.Text != "mine, edited" || m.Mentioned {
		t.Fatalf("Fetch = %+v, %v", m, err)
	}
	x.f.on("GET", "/channels/c1/messages", 200, `[
		{"id": "m3", "channel_id": "c1", "type": 0, "content": "newest", "author": {"id": "u1"}, "timestamp": "2027-01-15T08:00:00Z"},
		{"id": "m2", "channel_id": "c1", "type": 0, "content": "mine", "author": {"id": "bot"}, "timestamp": "2027-01-15T07:00:00Z"}
	]`)
	page, err := x.s.History(context.Background(), airut.Channel{ID: "c1"}, "", 10, airut.IncludeBot)
	if err != nil || len(page.Messages) != 2 || !page.Messages[1].FromBot || page.Messages[0].FromBot {
		t.Fatalf("history with the bot = %+v, %v", page.Messages, err)
	}
}
