package airut_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"terva.sh/airut"
	"terva.sh/airut/airuttest"
)

func count(t *testing.T, m *airut.Metrics, name string, labels ...string) float64 {
	t.Helper()
	s, ok := m.Get(name, labels...)
	if !ok {
		return 0
	}
	if s.Kind == airut.Timing {
		return float64(s.Count)
	}
	return s.Value
}

func TestCommandsAreCounted(t *testing.T) {
	b, _ := newBot(airut.Config{})
	b.Command("fail", "Fails", func(c *airut.Ctx) error { return errors.New("detail") })
	b.Command("boom", "Panics", func(c *airut.Ctx) error { panic("boom") })
	b.Command("mods", "For moderators", func(c *airut.Ctx) error { return nil }).
		RestrictTo(airut.Listed(map[airut.Platform]airut.Access{airuttest.Platform: {Users: []string{"dana"}}}))
	a := airuttest.Start(t, b)

	a.Text("!roll 2d6")
	a.Text("!roll 2d6")
	a.Text("!roll")
	a.Text("!fail")
	a.Text("!boom")
	a.Text("!mods")
	a.Text("!a-secret-typed-name")

	m := b.Metrics()
	for _, c := range []struct {
		command, outcome string
		want             float64
	}{
		{"roll", "ok", 2},
		{"roll", "arguments", 1},
		{"fail", "handler", 1},
		{"boom", "panic", 1},
		{"mods", "refused_access", 1},
		{"", "unknown", 1},
	} {
		if got := count(t, m, "airut_commands_total", "command", c.command, "outcome", c.outcome); got != c.want {
			t.Errorf("commands{%s,%s} = %v, want %v", c.command, c.outcome, got, c.want)
		}
	}
	if got := count(t, m, "airut_command_seconds", "command", "roll"); got != 3 {
		t.Errorf("roll's timing has %v observations, want 3", got)
	}
	if _, ok := m.Get("airut_command_seconds", "command", "mods"); ok {
		t.Error("a refused command was timed; only one that ran is")
	}
	if got := count(t, m, "airut_adapter_up", "platform", string(airuttest.Platform)); got != 1 {
		t.Errorf("adapter_up = %v, want 1 while serving", got)
	}
	for _, s := range m.Snapshot() {
		if strings.Contains(s.Key(), "secret") {
			t.Errorf("typed text reached a label: %s", s.Key())
		}
	}
}

func TestEventsAndAgentTurnsAreCounted(t *testing.T) {
	agent := &airuttest.Agent{Steps: []airuttest.Step{{Text: "hi"}}}
	b, _ := newBot(airut.Config{Gate: openGate, Agent: agent})
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		_, err := c.Agent(airut.Turn{Prompt: "p"}).Text()
		return err
	})
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error { return nil })
	b.OnMessage(func(c *airut.Ctx, m airut.MessageEvent) error { return errors.New("x") })
	a := airuttest.Start(t, b)

	a.Text("hello")
	a.Text("!ask")
	a.Text("!ask", airuttest.As(airut.User{Platform: airuttest.Platform, ID: "stranger"}))

	m := b.Metrics()
	if got := count(t, m, "airut_events_total", "kind", "message", "outcome", "ok"); got != 1 {
		t.Errorf("events ok = %v, want 1", got)
	}
	if got := count(t, m, "airut_events_total", "kind", "message", "outcome", "handler"); got != 1 {
		t.Errorf("events failed = %v, want 1", got)
	}
	if got := count(t, m, "airut_event_seconds", "kind", "message"); got != 1 {
		t.Errorf("event timings = %v, want 1", got)
	}
	if got := count(t, m, "airut_agent_turns_total", "command", "ask", "outcome", "ok"); got != 1 {
		t.Errorf("turns ok = %v, want 1", got)
	}
	if got := count(t, m, "airut_agent_turns_total", "command", "ask", "outcome", "refused_access"); got != 1 {
		t.Errorf("turns refused = %v, want 1", got)
	}
	if got := count(t, m, "airut_agent_turn_seconds", "command", "ask"); got != 1 {
		t.Errorf("turn timings = %v, want 1: a refused turn is not timed", got)
	}
}

type panickingAgent struct{}

func (panickingAgent) Run(*airut.Ctx, airut.Turn, airut.TurnOutput) error { panic("agent") }

func TestAPanickingAgentTurnIsCounted(t *testing.T) {
	b, _ := newBot(airut.Config{Gate: openGate, Agent: panickingAgent{}})
	b.Command("ask", "Ask", func(c *airut.Ctx) error {
		_, err := c.Agent(airut.Turn{Prompt: "p"}).Text()
		return err
	})
	airuttest.Start(t, b).Text("!ask")
	m := b.Metrics()
	if got := count(t, m, "airut_agent_turns_total", "command", "ask", "outcome", "panic"); got != 1 {
		t.Errorf("panicked turns = %v, want 1", got)
	}
	if got := count(t, m, "airut_agent_turn_seconds", "command", "ask"); got != 1 {
		t.Errorf("turn timings = %v, want 1", got)
	}
	if got := count(t, m, "airut_commands_total", "command", "ask", "outcome", "panic"); got != 1 {
		t.Errorf("the command's own recovery did not see the panic: %v", got)
	}
}

func TestMetricsRefuseMistakes(t *testing.T) {
	m := airut.New(airut.Config{}).Metrics()
	m.Add("jobs_total", 1)
	for name, f := range map[string]func(){
		"a bad name":        func() { m.Add("jobs-total", 1) },
		"an odd label list": func() { m.Add("jobs_total", 1, "lane") },
		"a bad label name":  func() { m.Add("jobs_total", 1, "the lane", "x") },
		"a reserved label":  func() { m.Add("jobs_total", 1, "__name__", "x") },
		"a label twice":     func() { m.Add("jobs_total", 1, "a", "x", "a", "y") },
		"a second kind":     func() { m.Observe("jobs_total", time.Second) },
		"a timing's count":  func() { m.Observe("step_seconds", time.Second); m.Add("step_seconds_count", 1) },
		"a timing over one": func() { m.Add("wait_seconds_max", 1); m.Observe("wait_seconds", time.Second) },
		"the uptime":        func() { m.Set("airut_uptime_seconds", 1) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic", name)
				}
			}()
			f()
		}()
	}
	m.Add("jobs_total", -5)
	if got := count(t, m, "jobs_total"); got != 1 {
		t.Errorf("a counter went to %v; it only goes up", got)
	}
}

func TestMetricsText(t *testing.T) {
	m := airut.New(airut.Config{}).Metrics()
	m.Add("jobs_total", 2, "lane", "live", "kind", `a"b\c`)
	m.Set("queue", 3)
	m.Observe("step_seconds", 1500*time.Millisecond, "step", "tag")
	m.Observe("step_seconds", 500*time.Millisecond, "step", "tag")

	var b strings.Builder
	if err := m.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	for _, want := range []string{
		"# TYPE jobs_total counter\njobs_total{kind=\"a\\\"b\\\\c\",lane=\"live\"} 2\n",
		"# TYPE queue gauge\nqueue 3\n",
		"# TYPE step_seconds summary\nstep_seconds_count{step=\"tag\"} 2\nstep_seconds_sum{step=\"tag\"} 2\n",
		"# TYPE step_seconds_max gauge\nstep_seconds_max{step=\"tag\"} 1.5\n",
		"# TYPE airut_uptime_seconds gauge\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the text lacks %q:\n%s", want, got)
		}
	}
	if s, _ := m.Get("step_seconds", "step", "tag"); s.Mean() != time.Second {
		t.Errorf("mean = %v", s.Mean())
	}
}

func TestChangedReportsDeltas(t *testing.T) {
	m := airut.New(airut.Config{}).Metrics()
	m.Add("a_total", 2)
	m.Set("g", 1)
	m.Observe("t_seconds", time.Second)
	if got := len(m.Changed()); got != 3 {
		t.Fatalf("first summary has %d series, want 3", got)
	}
	if got := m.Changed(); len(got) != 0 {
		t.Errorf("nothing changed, but the summary has %+v", got)
	}
	m.Add("a_total", 3)
	m.Observe("t_seconds", 3*time.Second)
	got := m.Changed()
	if len(got) != 2 || got[0].Value != 3 || got[1].Count != 1 || got[1].Mean() != 3*time.Second {
		t.Errorf("deltas = %+v", got)
	}
}

func TestChangedIsConsistentUnderConcurrency(t *testing.T) {
	m := airut.New(airut.Config{}).Metrics()
	var total float64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				m.Add("a_total", 1)
				for _, s := range m.Changed() {
					if s.Value < 0 {
						t.Errorf("a negative delta: %v", s.Value)
					}
					mu.Lock()
					total += s.Value
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	for _, s := range m.Changed() {
		total += s.Value
	}
	if total != 800 {
		t.Errorf("the deltas sum to %v, want 800", total)
	}
}

func TestRunServesMetricsAndSummaries(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	logs := &lockedBuffer{}
	b := airut.New(airut.Config{MetricsAddr: addr, SummaryEvery: 10 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	b.Command("ping", "Ping", func(c *airut.Ctx) error { return nil })
	a := airuttest.Start(t, b)
	a.Text("!ping")

	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("content type %q", ct)
	}
	if !strings.Contains(string(body), `airut_commands_total{command="ping",outcome="ok"} 1`) {
		t.Errorf("/metrics lacks the ping:\n%s", body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), `msg="metrics summary"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no summary was logged:\n%s", logs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestTheLastSummaryCountsWorkFinishedAtShutdown stops a bot while a
// command runs, and checks the last summary counts it.
func TestTheLastSummaryCountsWorkFinishedAtShutdown(t *testing.T) {
	logs := &lockedBuffer{}
	b := airut.New(airut.Config{SummaryEvery: time.Hour, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	started := make(chan struct{})
	b.Command("slow", "Slow", func(c *airut.Ctx) error {
		close(started)
		<-c.Context().Done()
		time.Sleep(20 * time.Millisecond)
		return nil
	})
	a := airuttest.New("")
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan error, 1)
	go func() { ran <- b.Run(ctx, a) }()
	<-a.Ready()
	go a.Text("!slow")
	<-started
	cancel()
	if err := <-ran; err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "metrics summary") || !strings.Contains(last, `"airut_commands_total{command=\"slow\",outcome=\"ok\"}"=1`) {
		t.Errorf("the last line is not a summary counting the slow command:\n%s", logs)
	}
}

func TestRunFailsWhenMetricsCannotListen(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := airut.New(airut.Config{MetricsAddr: ln.Addr().String()})
	a := airuttest.New("")
	err = b.Run(context.Background(), a)
	if err == nil || !strings.Contains(err.Error(), "metrics") {
		t.Fatalf("Run = %v, want a metrics listen error", err)
	}
	select {
	case <-a.Ready():
		t.Error("the adapter started although the metrics could not be served")
	default:
	}
	ln.Close()
	b.Command("ping", "Ping", func(c *airut.Ctx) error { return nil })
	airuttest.Start(t, b).Text("!ping")
	if got := count(t, b.Metrics(), "airut_commands_total", "command", "ping", "outcome", "ok"); got != 1 {
		t.Errorf("the bot did not run once the address was free: %v", got)
	}
}
