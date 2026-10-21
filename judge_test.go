package airut_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

// judgeFunc is a Judge that answers with f.
type judgeFunc func(*airut.Ctx, airut.Judgement) (airut.Verdict, error)

func (f judgeFunc) Judge(c *airut.Ctx, j airut.Judgement) (airut.Verdict, error) { return f(c, j) }

var batch = []airut.Quoted{
	{Ref: airut.MessageRef{ID: "m1"}, Text: "secret chat one"},
	{Ref: airut.MessageRef{ID: "m2"}, Text: "secret chat two"},
}

// judgeOnce runs one judgement through a command and returns its result.
func judgeOnce(t *testing.T, cfg airut.Config, j airut.Judgement) (airut.Verdict, error, *airut.Bot, string) {
	t.Helper()
	b, logs := newBot(cfg)
	var v airut.Verdict
	var err error
	b.Command("judge", "Judge", func(c *airut.Ctx) error {
		v, err = c.Judge(j)
		return nil
	})
	airuttest.Start(t, b).Text("!judge")
	return v, err, b, logs.String()
}

func TestJudgeTakesOnlyACleanAnswer(t *testing.T) {
	boom := errors.New("connection refused")
	for _, c := range []struct {
		name  string
		got   airut.Verdict
		err   error
		batch []airut.Quoted
		want  airut.Verdict
	}{
		{"a yes about a message of the batch", airut.Verdict{Decision: airut.Yes, Target: batch[1].Ref, Reason: "r"}, nil, batch,
			airut.Verdict{Decision: airut.Yes, Target: batch[1].Ref, Reason: "r"}},
		{"a no, whose target is dropped", airut.Verdict{Decision: airut.No, Target: batch[0].Ref}, nil, batch, airut.Verdict{Decision: airut.No}},
		{"a yes about a message outside the batch", airut.Verdict{Decision: airut.Yes, Target: airut.MessageRef{ID: "elsewhere"}}, nil, batch,
			airut.Verdict{Decision: airut.Abstain}},
		{"a yes about no message of a batch", airut.Verdict{Decision: airut.Yes}, nil, batch, airut.Verdict{Decision: airut.Abstain}},
		{"a yes with no batch", airut.Verdict{Decision: airut.Yes}, nil, nil, airut.Verdict{Decision: airut.Yes}},
		{"a decision that is not one", airut.Verdict{Decision: "maybe"}, nil, batch, airut.Verdict{Decision: airut.Abstain}},
		{"a failure, whatever came with it", airut.Verdict{Decision: airut.Yes, Target: batch[0].Ref}, boom, batch, airut.Verdict{Decision: airut.Abstain}},
	} {
		t.Run(c.name, func(t *testing.T) {
			j := judgeFunc(func(*airut.Ctx, airut.Judgement) (airut.Verdict, error) { return c.got, c.err })
			v, err, _, _ := judgeOnce(t, airut.Config{Judge: j}, airut.Judgement{Persona: "chime", Batch: c.batch})
			if v != c.want || err != c.err {
				t.Errorf("Judge = %+v, %v; want %+v, %v", v, err, c.want, c.err)
			}
		})
	}
	if v, err, _, _ := judgeOnce(t, airut.Config{}, airut.Judgement{}); v.Decision != airut.Abstain || !errors.Is(err, airut.ErrNoJudge) {
		t.Errorf("with no judge: %+v, %v", v, err)
	}
}

func TestJudgeKeysByChannel(t *testing.T) {
	var key string
	j := judgeFunc(func(_ *airut.Ctx, jm airut.Judgement) (airut.Verdict, error) {
		key = jm.Key
		return airut.Verdict{Decision: airut.No}, nil
	})
	judgeOnce(t, airut.Config{Judge: j}, airut.Judgement{})
	if key != "test/channel/channel-1" {
		t.Errorf("the default key is %q", key)
	}
	judgeOnce(t, airut.Config{Judge: j}, airut.Judgement{Key: "mine"})
	if key != "mine" {
		t.Errorf("the app's key became %q", key)
	}
}

func TestJudgeHasItsOwnRate(t *testing.T) {
	calls := 0
	j := judgeFunc(func(*airut.Ctx, airut.Judgement) (airut.Verdict, error) {
		calls++
		return airut.Verdict{Decision: airut.No}, nil
	})
	// A gate that would refuse any turn does not stop a judgement, and
	// judgements do not count against the gate.
	g := &airuttest.Agent{}
	b, _ := newBot(airut.Config{Judge: j, Agent: g, JudgeRate: airut.JudgeRate{PerChannel: 2, Window: time.Hour},
		Gate: airut.Gate{Access: openGate.Access, PerChannel: 1, Window: time.Hour}})
	var errs []error
	b.Command("judge", "Judge", func(c *airut.Ctx) error {
		_, err := c.Judge(airut.Judgement{})
		errs = append(errs, err)
		return nil
	})
	b.Command("ask", "Ask", func(c *airut.Ctx) error { return c.Agent(airut.Turn{Prompt: "?"}).StreamTo(c) })
	a := airuttest.Start(t, b)
	for range 3 {
		a.Text("!judge")
	}
	if calls != 2 || errs[0] != nil || errs[1] != nil || !errors.Is(errs[2], airut.ErrJudgeRate) {
		t.Errorf("three judgements under a rate of two: %d calls, %v", calls, errs)
	}
	a.Text("!judge", airuttest.In(airut.Channel{Platform: airuttest.Platform, ID: "other"}))
	if calls != 3 {
		t.Error("another channel's judgement was refused")
	}
	a.Text("!ask")
	if len(g.Turns()) != 1 {
		t.Errorf("the channel's one agent turn was refused after its judgements")
	}
}

func TestJudgeCountsAndLogsNoText(t *testing.T) {
	j := judgeFunc(func(*airut.Ctx, airut.Judgement) (airut.Verdict, error) {
		return airut.Verdict{Decision: airut.Yes, Target: batch[0].Ref, Reason: "secret reason"}, nil
	})
	_, _, b, logs := judgeOnce(t, airut.Config{Judge: j}, airut.Judgement{Persona: "chime", Prompt: "secret prompt", Batch: batch,
		Context: []airut.Quoted{{Text: "secret context"}}})
	if s, ok := b.Metrics().Get("airut_judge_total", "persona", "chime", "decision", "yes"); !ok || s.Value != 1 {
		t.Errorf("airut_judge_total = %+v, %v", s, ok)
	}
	if !strings.Contains(logs, "decision=yes") {
		t.Errorf("the log has no decision:\n%s", logs)
	}
	for _, leak := range []string{"secret"} {
		if strings.Contains(logs, leak) {
			t.Errorf("the log holds %q:\n%s", leak, logs)
		}
	}
}
