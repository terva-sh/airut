package agent

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"terva.sh/terva/packages/provider"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

func TestTurnsTimeTheirRequestsAndTools(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	var mu sync.Mutex
	var logs strings.Builder
	e.agent.log = slogTo(func(p []byte) {
		mu.Lock()
		defer mu.Unlock()
		logs.Write(p)
	})
	e.ask(airut.Turn{Prompt: "roll", Tools: e.bot.Tools("roll")})
	slow := call("c1", "roll", `{"dice":"wait"}`)
	slow.delay = 30 * time.Millisecond
	// The answer's first token comes at once, and the rest after a gap.
	answer := reply{content: []provider.Content{provider.TextBlock{Text: "You "}, provider.TextBlock{Text: "rolled."}},
		stop: provider.StopEnd, tokens: 100, gap: 40 * time.Millisecond}
	e.model.play(
		slow,
		call("c2", "roll", `{"dice":"fail"}`),
		answer,
	)
	airuttest.Start(t, e.bot).Text("!ask")
	mu.Lock()
	out := logs.String()
	mu.Unlock()

	lines := func(msg string) []string {
		var got []string
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "msg=\""+msg+"\"") {
				got = append(got, l)
			}
		}
		return got
	}
	reqs := lines("agent request")
	if len(reqs) != 3 {
		t.Fatalf("%d request lines, want 3:\n%s", len(reqs), out)
	}
	for i, l := range reqs {
		for _, want := range []string{"step=" + string(rune('1'+i)), "provider=fake", "model=m", "ms=", "input_tokens=50", "output_tokens=50"} {
			if !strings.Contains(l, want) {
				t.Errorf("request %d lacks %s: %s", i+1, want, l)
			}
		}
	}
	// Only the last request streamed text, so only it has a first token.
	if strings.Contains(reqs[0], "first_token_ms") || !strings.Contains(reqs[2], "first_token_ms=") ||
		!strings.Contains(reqs[0], "stop=tool_use") || !strings.Contains(reqs[2], "stop=end") {
		t.Errorf("the request lines:\n%s", strings.Join(reqs, "\n"))
	}
	tools := lines("agent tool")
	if len(tools) != 2 || !strings.Contains(tools[0], "tool=roll") || !strings.Contains(tools[0], "outcome=ok") ||
		!strings.Contains(tools[1], "outcome=error") || !strings.Contains(tools[0], "step=1") || !strings.Contains(tools[1], "step=2") {
		t.Errorf("the tool lines:\n%s", strings.Join(tools, "\n"))
	}
	fin := lines("agent turn finished")
	if len(fin) != 1 {
		t.Fatalf("the finished lines: %v", fin)
	}
	// The slow request and the slow tool show in their lines and in the
	// turn's sums.
	for line, keys := range map[string][]string{reqs[0]: {"ms"}, tools[0]: {"ms"}, fin[0]: {"ms", "model_ms", "tool_ms"}} {
		for _, k := range keys {
			if v := field(line, k); v < 30 {
				t.Errorf("%s=%d, want at least 30, in %s", k, v, line)
			}
		}
	}
	if first, all := field(reqs[2], "first_token_ms"), field(reqs[2], "ms"); first >= 40 || all < 40 {
		t.Errorf("the answer's first token at %dms of %dms", first, all)
	}
	if total, parts := field(fin[0], "ms"), field(fin[0], "model_ms")+field(fin[0], "tool_ms"); parts > total {
		t.Errorf("the turn took %dms, less than its parts' %dms", total, parts)
	}
	for _, secret := range []string{"postgres", "rolled", "You rolled", "wait"} {
		if strings.Contains(out, secret) {
			t.Errorf("%q reached the log:\n%s", secret, out)
		}
	}

	m := e.bot.Metrics()
	for _, c := range []struct {
		name   string
		labels []string
		want   uint64
	}{
		{"airut_agent_request_seconds", []string{"provider", "fake", "model", "m", "served", "m"}, 3},
		{"airut_agent_first_token_seconds", []string{"provider", "fake", "model", "m", "served", "m"}, 1},
		{"airut_agent_tool_seconds", []string{"tool", "roll"}, 2},
	} {
		if s, _ := m.Get(c.name, c.labels...); s.Kind != airut.Timing || s.Count != c.want {
			t.Errorf("%s counted %d, want %d", c.name, s.Count, c.want)
		}
	}
}

// field reads the whole number of a log line's key=N.
func field(line, key string) int {
	i := strings.Index(line, " "+key+"=")
	if i < 0 {
		return -1
	}
	rest := line[i+len(key)+2:]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		rest = rest[:j]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return -1
	}
	return n
}

// A retried step's attempts are timed apart: each from the last one's
// end, so the step's time is not counted twice.
func TestRetriedRequestsAreTimedApart(t *testing.T) {
	e := newEnv(t, nil, nil, airut.Config{})
	var model time.Duration
	var second string
	e.bot.Command("retry", "Time a retried step", func(c *airut.Ctx) error {
		var mu sync.Mutex
		var logs strings.Builder
		tm := &timing{c: c, provider: "fake", model: "m", log: slogTo(func(p []byte) {
			mu.Lock()
			defer mu.Unlock()
			logs.Write(p)
		})}
		tm.begin(1)
		time.Sleep(40 * time.Millisecond)
		tm.end("error")
		tm.end("end")
		model, _ = tm.totals()
		mu.Lock()
		second = strings.Split(strings.TrimSpace(logs.String()), "\n")[1]
		mu.Unlock()
		return nil
	})
	airuttest.Start(t, e.bot).Text("!retry")
	if model < 40*time.Millisecond || model >= 80*time.Millisecond {
		t.Errorf("the step's two attempts took %v together", model)
	}
	if v := field(second, "ms"); v >= 40 {
		t.Errorf("the retry took %dms, the first attempt's time again", v)
	}
}
