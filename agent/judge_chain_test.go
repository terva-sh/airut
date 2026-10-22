package agent

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/config"
)

// judgeChain is a judge on two fake servers: "gemma-judge" on provider
// a, served as gemma, under response_format, then "humanlike" on b,
// served as h, under no output constraint at temperature 0.3.
type judgeChain struct {
	s1, s2 *judgeServer
	g      *Judge
	logs   *logBuffer
	clk    *testClock
}

func newJudgeChain(t *testing.T, providers []config.Provider, tweak func(ms []JudgeModel), replies2 ...string) *judgeChain {
	t.Helper()
	jc := &judgeChain{logs: &logBuffer{}, clk: &testClock{t: time.Unix(1000, 0)}}
	var url1, url2 string
	jc.s1, url1 = newJudgeServer(t, `{"decision":"no","target":0,"reason":"from one"}`)
	jc.s2, url2 = newJudgeServer(t, replies2...)
	warm := 0.3
	ms := []JudgeModel{
		{Provider: "a", Alias: "gemma-judge", BaseURL: url1, Model: "gemma", Output: OutputResponseFormat, Slots: 4, CachePrompt: true},
		{Provider: "b", Alias: "humanlike", BaseURL: url2, Model: "h", Output: OutputNone, Temperature: &warm, MaxTokens: 64},
	}
	if tweak != nil {
		tweak(ms)
	}
	log := jc.logs.logger()
	g, err := NewJudge(JudgeOptions{Models: ms, Personas: map[string]string{"p": "Decide."}, Logger: log,
		Health: NewHealth(HealthOptions{Providers: providers, Now: jc.clk.now, Logger: log})})
	if err != nil {
		t.Fatal(err)
	}
	jc.g = g
	return jc
}

var chainJudgement = airut.Judgement{Persona: "p", Prompt: "Speak?", Batch: judgeBatch}

func (jc *judgeChain) fellThrough(jb *judgeBot, from, reason string) float64 {
	s, _ := jb.bot.Metrics().Get("airut_model_fallthrough_total", "use", "judge", "from", from, "reason", reason)
	return s.Value
}

func TestJudgeFallsThroughPerCall(t *testing.T) {
	for _, c := range []struct {
		status int
		reason string
	}{
		{http.StatusServiceUnavailable, class5xx},
		{http.StatusTooManyRequests, class429},
		{http.StatusNotFound, classNotFound},
	} {
		jc := newJudgeChain(t, nil, nil, "Decision: yes\nTarget: 2\nReason: asked")
		jc.s1.status = c.status
		jb := newJudgeBot(t, jc.g, chainJudgement)
		jb.a.Text("!judge")
		if v := jb.got[0]; v.Decision != airut.Yes || v.Target.ID != "m2" || jb.errs[0] != nil {
			t.Errorf("%d: the verdict %+v, %v; want the loose parse of the second model's reply", c.status, v, jb.errs[0])
		}
		if n1, n2 := len(jc.s1.requests()), len(jc.s2.requests()); n1 != 1 || n2 != 1 {
			t.Errorf("%d: %d and %d calls", c.status, n1, n2)
		}
		if n := jc.fellThrough(jb, "gemma-judge", c.reason); n != 1 {
			t.Errorf("%d: airut_model_fallthrough_total{reason=%s} = %v", c.status, c.reason, n)
		}
		// Each model's request is its own: its served id, its output, its
		// slot and cache hints, its temperature, and its reply cap.
		b1, b2 := jc.s1.requests()[0], jc.s2.requests()[0]
		if b1["model"] != "gemma" || b1["response_format"] == nil || b1["id_slot"] == nil || b1["cache_prompt"] != true ||
			b1["temperature"] != 0.0 || b1["max_tokens"] != 256.0 {
			t.Errorf("the first request: %v", b1)
		}
		if b2["model"] != "h" || b2["response_format"] != nil || b2["id_slot"] != nil || b2["cache_prompt"] != nil ||
			b2["temperature"] != 0.3 || b2["max_tokens"] != 64.0 {
			t.Errorf("the second request: %v", b2)
		}
		reqs := jc.logs.lines("judge request")
		if len(reqs) != 2 || !strings.Contains(reqs[0], "model=gemma-judge") || !strings.Contains(reqs[0], "served=gemma") ||
			!strings.Contains(reqs[0], "fell_through=0") || !strings.Contains(reqs[1], "model=humanlike") ||
			!strings.Contains(reqs[1], "served=h") || !strings.Contains(reqs[1], "fell_through=1") {
			t.Errorf("the request lines: %v", reqs)
		}
		m := jb.bot.Metrics()
		if s, _ := m.Get("airut_judge_request_seconds", "model", "humanlike", "served", "h", "outcome", "ok"); s.Count != 1 {
			t.Errorf("the second call was timed %d times", s.Count)
		}
		if s := jb.logs.String() + jc.logs.String(); strings.Contains(s, "anyone know") || strings.Contains(s, "secret") ||
			strings.Contains(s, "asked") {
			t.Errorf("text reached the log:\n%s", s)
		}
	}
}

func TestJudgeStopsOnABadRequestOrAReply(t *testing.T) {
	jc := newJudgeChain(t, nil, nil)
	jc.s1.status = http.StatusBadRequest
	jb := newJudgeBot(t, jc.g, chainJudgement)
	jb.a.Text("!judge")
	if jb.got[0].Decision != airut.Abstain || len(jc.s2.requests()) != 0 {
		t.Errorf("a bad request: %+v, and %d calls to the next", jb.got[0], len(jc.s2.requests()))
	}

	// A reply that is no verdict is the model's answer: it abstains there.
	jc2 := newJudgeChain(t, nil, nil)
	jc2.s1.replies = []string{"not json"}
	jb2 := newJudgeBot(t, jc2.g, chainJudgement)
	jb2.a.Text("!judge")
	if jb2.got[0].Decision != airut.Abstain || jb2.errs[0] == nil || len(jc2.s2.requests()) != 0 {
		t.Errorf("a broken reply: %+v, %v, and %d calls to the next", jb2.got[0], jb2.errs[0], len(jc2.s2.requests()))
	}
}

func TestJudgeTimeoutIsPerModel(t *testing.T) {
	jc := newJudgeChain(t, nil, func(ms []JudgeModel) {
		ms[0].Timeout = 100 * time.Millisecond
		ms[1].Timeout = 2 * time.Second
	})
	jc.s1.hold = make(chan struct{})
	defer close(jc.s1.hold)
	jb := newJudgeBot(t, jc.g, chainJudgement)
	jb.a.Text("!judge")
	if jb.got[0].Decision != airut.No || jb.errs[0] != nil || len(jc.s2.requests()) != 1 {
		t.Errorf("after the first model timed out: %+v, %v", jb.got[0], jb.errs[0])
	}
	if n := jc.fellThrough(jb, "gemma-judge", classTimeout); n != 1 {
		t.Errorf("timeout counted %v", n)
	}
}

func TestJudgeChecksTheCapPerModel(t *testing.T) {
	// The first model's cap is too small for even the instructions.
	jc := newJudgeChain(t, nil, func(ms []JudgeModel) { ms[0].MaxInputTokens = 257 })
	if jc.g.InputTokens() != 0 || !jc.g.Fits(chainJudgement) {
		t.Errorf("a chain with an uncapped model caps at %d, or does not fit", jc.g.InputTokens())
	}
	jb := newJudgeBot(t, jc.g, chainJudgement)
	jb.a.Text("!judge")
	if jb.errs[0] != nil || len(jc.s1.requests()) != 0 || len(jc.s2.requests()) != 1 {
		t.Errorf("too large for the first: %v, %d and %d calls", jb.errs[0], len(jc.s1.requests()), len(jc.s2.requests()))
	}
	if n := jc.fellThrough(jb, "gemma-judge", reasonTooLarge); n != 1 {
		t.Errorf("too_large counted %v", n)
	}

	// Too large for every model, it abstains without a call.
	jc2 := newJudgeChain(t, nil, func(ms []JudgeModel) { ms[0].MaxInputTokens, ms[1].MaxInputTokens = 300, 100 })
	if jc2.g.InputTokens() != 300 || jc2.g.Fits(chainJudgement) {
		t.Errorf("the chain caps at %d, and fits", jc2.g.InputTokens())
	}
	jb2 := newJudgeBot(t, jc2.g, chainJudgement)
	jb2.a.Text("!judge")
	if !errors.Is(jb2.errs[0], ErrJudgementTooLarge) || len(jc2.s1.requests())+len(jc2.s2.requests()) != 0 {
		t.Errorf("too large for all: %v", jb2.errs[0])
	}

	// Each model cuts the judgement to its own cap: the first, smaller,
	// keeps less of the context than the second, which is sent whole.
	var context []airut.Quoted
	for i := range 10 {
		context = append(context, airut.Quoted{Ref: airut.MessageRef{ID: string(rune('a' + i))}, Text: strings.Repeat("x", 99)})
	}
	big := chainJudgement
	big.Context = context
	jc3 := newJudgeChain(t, nil, func(ms []JudgeModel) { ms[0].MaxInputTokens = 600 })
	jc3.s1.status = http.StatusBadGateway
	jb3 := newJudgeBot(t, jc3.g, big)
	jb3.a.Text("!judge")
	user := func(s *judgeServer) string {
		return s.requests()[0]["messages"].([]any)[1].(map[string]any)["content"].(string)
	}
	if u1, u2 := user(jc3.s1), user(jc3.s2); strings.Count(u1, `x"`) >= 10 || strings.Count(u2, `x"`) != 10 {
		t.Errorf("the first kept %d quotes, the second %d", strings.Count(u1, `x"`), strings.Count(u2, `x"`))
	}
}

func TestJudgeSkipsUnavailableAndOpenBreakers(t *testing.T) {
	ls := newLlamaSwap(t, "booru-tagger")
	jc := newJudgeChain(t, []config.Provider{ls.provider("a", config.LoadWhenIdle)}, nil)
	jb := newJudgeBot(t, jc.g, chainJudgement)
	jb.a.Text("!judge")
	if len(jc.s1.requests()) != 0 || len(jc.s2.requests()) != 1 {
		t.Errorf("a busy llama-swap: %d and %d calls", len(jc.s1.requests()), len(jc.s2.requests()))
	}
	if n := jc.fellThrough(jb, "gemma-judge", reasonUnavailable); n != 1 {
		t.Errorf("unavailable counted %v", n)
	}
	// Idle, it is used, once the kept busy answer has run out.
	ls.set()
	jb.a.Text("!judge")
	if len(jc.s1.requests()) != 0 {
		t.Error("the kept busy answer was not used")
	}
	jc.clk.advance(DefaultNotLoadedFor)
	jb.a.Text("!judge")
	if len(jc.s1.requests()) != 1 {
		t.Errorf("an idle llama-swap under when_idle was not used")
	}

	// Nothing available abstains with ErrNoModel.
	jc2 := newJudgeChain(t, []config.Provider{ls.provider("a", config.LoadNever), ls.provider("b", config.LoadNever)}, nil)
	ls.set("booru-tagger")
	jb2 := newJudgeBot(t, jc2.g, chainJudgement)
	jb2.a.Text("!judge")
	if !errors.Is(jb2.errs[0], ErrNoModel) || len(jc2.s1.requests())+len(jc2.s2.requests()) != 0 {
		t.Errorf("nothing available: %v", jb2.errs[0])
	}

	// Three failed calls open the first model's breaker.
	jc3 := newJudgeChain(t, nil, nil)
	jc3.s1.status = http.StatusInternalServerError
	jb3 := newJudgeBot(t, jc3.g, chainJudgement)
	for range 4 {
		jb3.a.Text("!judge")
	}
	if n1, n2 := len(jc3.s1.requests()), len(jc3.s2.requests()); n1 != 3 || n2 != 4 {
		t.Errorf("with the breaker: %d and %d calls", n1, n2)
	}
	if n := jc3.fellThrough(jb3, "gemma-judge", reasonBreakerOpen); n != 1 {
		t.Errorf("breaker_open counted %v", n)
	}
	// The last model failing too abstains with its error.
	jc3.s2.status = http.StatusInternalServerError
	jc3.clk.advance(DefaultBreakerCoolOff)
	jb3.a.Text("!judge")
	if err := jb3.errs[len(jb3.errs)-1]; err == nil || errors.Is(err, ErrNoModel) {
		t.Errorf("both failing: %v", err)
	}
}

// A model called because the one before it was passed over has its own
// timeout, not the passed-over model's.
func TestJudgeFallbackHasItsOwnTimeout(t *testing.T) {
	ls := newLlamaSwap(t, "booru-tagger")
	jc := newJudgeChain(t, []config.Provider{ls.provider("a", config.LoadNever)}, func(ms []JudgeModel) {
		ms[0].Timeout = 100 * time.Millisecond
		ms[1].Timeout = 2 * time.Second
	}, `{"decision":"no","target":0,"reason":"slow"}`)
	jc.s2.hold = make(chan struct{})
	go func() {
		<-jc.s2.started
		time.Sleep(300 * time.Millisecond)
		close(jc.s2.hold)
	}()
	jb := newJudgeBot(t, jc.g, chainJudgement)
	jb.a.Text("!judge")
	if jb.errs[0] != nil || jb.got[0].Decision != airut.No || len(jc.s1.requests()) != 0 {
		t.Errorf("the slow fallback: %+v, %v", jb.got[0], jb.errs[0])
	}
}
