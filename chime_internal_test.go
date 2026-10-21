package airut

import (
	"context"
	"testing"
	"time"
)

type judgeFunc func(*Ctx, Judgement) (Verdict, error)

func (f judgeFunc) Judge(c *Ctx, j Judgement) (Verdict, error) { return f(c, j) }

// A platform with no text commands, as Discord, delivers a line that
// opens with the prefix as chat. Chime leaves it alone all the same.
func TestChimeSkipsCommandLinesDeliveredAsChat(t *testing.T) {
	asked := make(chan Judgement, 4)
	b := New(Config{Judge: judgeFunc(func(_ *Ctx, j Judgement) (Verdict, error) {
		asked <- j
		return Verdict{Decision: No}, nil
	})})
	ch := Channel{Platform: "test", ID: "c"}
	b.Chime(ChimeOptions{Channels: []Channel{ch}, MaxPerHour: 1, Settle: 50 * time.Millisecond,
		Handler: func(*Ctx, Chimed) error { return nil }})
	for i, text := range []string{"!roll 2d6", "nice roll"} {
		b.deliver(context.Background(), MessageEvent{ID: "e", Message: MessageRef{ID: string(rune('a' + i))}, Channel: ch, Text: text}, nil)
	}
	select {
	case j := <-asked:
		if len(j.Batch) != 1 || j.Batch[0].Text != "nice roll" {
			t.Errorf("the batch is %+v", j.Batch)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no judgement")
	}
	b.inflight.Wait()
}

// fetchConn is a Conn whose Fetch waits for the test.
type fetchConn struct {
	Conn
	entered chan struct{}
	release chan MessageEvent
}

func (f fetchConn) Fetch(ctx context.Context, _ Channel, _ MessageRef) (MessageEvent, error) {
	f.entered <- struct{}{}
	return <-f.release, nil
}

// A message that arrives while StayOut reads the replied message still
// overtakes the yes.
func TestChimeRechecksAfterStayingOutWaits(t *testing.T) {
	ch := Channel{Platform: "test", ID: "c"}
	target := MessageEvent{ID: "e1", Message: MessageRef{ID: "m1"}, Channel: ch, Sender: User{ID: "alice"},
		Text: "thanks", ReplyTo: MessageRef{ID: "unseen"}}
	b := New(Config{Judge: judgeFunc(func(*Ctx, Judgement) (Verdict, error) {
		return Verdict{Decision: Yes, Target: target.Message}, nil
	})})
	conn := fetchConn{entered: make(chan struct{}), release: make(chan MessageEvent)}
	b.conns = map[Platform]Conn{"test": conn}
	ran := make(chan struct{}, 1)
	b.Chime(ChimeOptions{Channels: []Channel{ch}, MaxPerHour: 1, Settle: 10 * time.Millisecond, StayOut: true,
		Handler: func(*Ctx, Chimed) error { ran <- struct{}{}; return nil }})
	b.deliver(context.Background(), target, nil)
	<-conn.entered
	b.deliver(context.Background(), MessageEvent{ID: "e2", Message: MessageRef{ID: "m2"}, Channel: ch,
		Sender: User{ID: "bob"}, Text: "you're welcome"}, nil)
	conn.release <- MessageEvent{FromBot: true}
	b.inflight.Wait()
	select {
	case <-ran:
		t.Error("the handler ran on a yes the room overtook")
	default:
	}
	if s, _ := b.metrics.Get("airut_chime_total", "persona", "", "outcome", "stale"); s.Value != 1 {
		t.Errorf("stale counted %v times", s.Value)
	}
}
