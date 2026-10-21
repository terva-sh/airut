package airut_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

func TestAddQuote(t *testing.T) {
	q := func(n int) airut.Quoted { return airut.Quoted{Text: strings.Repeat("x", n)} }
	chain, more := airut.AddQuote(nil, q(10))
	if !more || chain[0].Text != strings.Repeat("x", 10) {
		t.Fatalf("a short quote: %v %v", chain, more)
	}
	chain, _ = airut.AddQuote(nil, q(airut.MaxQuote+50))
	if len(chain[0].Text) > airut.MaxQuote || !strings.HasSuffix(chain[0].Text, "…") {
		t.Fatalf("a long quote kept %d bytes", len(chain[0].Text))
	}
	// The chain as a whole is capped; the link that fills it says so.
	chain = nil
	for more = true; more; {
		chain, more = airut.AddQuote(chain, q(airut.MaxQuote))
	}
	total := 0
	for _, c := range chain {
		total += len(c.Text)
	}
	if total > airut.MaxChain || len(chain) != airut.MaxChain/airut.MaxQuote {
		t.Fatalf("the chain holds %d links and %d bytes", len(chain), total)
	}
	if again, more := airut.AddQuote(chain, q(1)); more || len(again) != len(chain) {
		t.Fatal("a full chain took another link")
	}
	// With a few bytes left, a quote that fits is kept whole, and one
	// that must be cut is cut with its mark inside the cap, or left out.
	for left := 1; left <= 4; left++ {
		var base []airut.Quoted
		for range airut.MaxChain/airut.MaxQuote - 1 {
			base, _ = airut.AddQuote(base, q(airut.MaxQuote))
		}
		base, _ = airut.AddQuote(base, q(airut.MaxQuote-left))
		for _, n := range []int{left, left + 1} {
			got, _ := airut.AddQuote(base, q(n))
			total := 0
			for _, c := range got {
				total += len(c.Text)
			}
			if total > airut.MaxChain {
				t.Errorf("%d bytes left, a %d-byte quote: the chain holds %d bytes", left, n, total)
			}
			if n == left && (len(got) != len(base)+1 || got[len(got)-1].Text != q(n).Text) {
				t.Errorf("%d bytes left, a quote that fits was not kept whole", left)
			}
		}
	}
	// A cut never splits a character.
	chain, _ = airut.AddQuote(nil, airut.Quoted{Text: strings.Repeat("é", airut.MaxQuote)})
	if !strings.HasSuffix(strings.TrimSuffix(chain[0].Text, "…"), "é") {
		t.Fatalf("the cut split a character: %q", chain[0].Text[len(chain[0].Text)-5:])
	}
}

func TestReplyChainReachesTheHandler(t *testing.T) {
	b, _ := newBot(airut.Config{})
	var got [][]airut.Quoted
	b.Command("what", "What was that?", func(c *airut.Ctx) error {
		got = append(got, c.ReplyChain())
		return nil
	})
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error {
		got = append(got, m.ReplyChain, c.ReplyChain())
		return nil
	})
	a := airuttest.Start(t, b)
	chain := []airut.Quoted{
		{Ref: airut.MessageRef{ID: "m2"}, Text: "the other one", FromBot: true},
		{Ref: airut.MessageRef{ID: "m1"}, Text: "which post?"},
	}
	a.Text("!what", airuttest.WithReplyChain(chain...))
	a.Mention("no, that one", airuttest.WithReplyChain(chain...))
	if len(got) != 3 || !reflect.DeepEqual(got[0], chain) || !reflect.DeepEqual(got[1], chain) || !reflect.DeepEqual(got[2], chain) {
		t.Fatalf("chains = %v", got)
	}
}

func TestFetchAndHistoryWithTheBot(t *testing.T) {
	a := airuttest.New(airuttest.Platform)
	a.AddHistory("c", airut.MessageEvent{Message: airut.MessageRef{ID: "m1"}, Text: "hi"},
		airut.MessageEvent{Message: airut.MessageRef{ID: "m2"}, Text: "hello", FromBot: true})
	ch := airut.Channel{Platform: airuttest.Platform, ID: "c"}
	ctx := context.Background()
	if page, _ := a.History(ctx, ch, "", 10); len(page.Messages) != 1 {
		t.Fatalf("history without IncludeBot held %d messages", len(page.Messages))
	}
	if page, _ := a.History(ctx, ch, "", 10, airut.IncludeBot); len(page.Messages) != 2 || !page.Messages[0].FromBot {
		t.Fatalf("history with IncludeBot = %+v", page.Messages)
	}
	if m, err := a.Fetch(ctx, ch, airut.MessageRef{ID: "m2"}); err != nil || !m.FromBot || m.Text != "hello" {
		t.Fatalf("Fetch = %+v, %v", m, err)
	}
	if _, err := a.Fetch(ctx, ch, airut.MessageRef{ID: "nope"}); err == nil {
		t.Fatal("Fetch found a message that is not there")
	}
}
