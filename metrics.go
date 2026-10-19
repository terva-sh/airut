package airut

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metrics holds a bot's counters, gauges, and timings. The core records
// commands, events, agent turns, and adapters in it, and a bot records
// its own with Add, Set, and Observe. It is safe for concurrent use.
//
// A series is a name and label pairs, as Add("jobs_total", 1, "lane",
// "live"). Names and label names match [a-zA-Z_][a-zA-Z0-9_]*. Label
// values are ids and kinds, never message text: each distinct value is a
// series kept for the bot's lifetime, and a value from chat would let
// anyone grow it without bound.
type Metrics struct {
	mu     sync.Mutex
	start  time.Time
	series map[string]*series
	// kinds holds the kind each name was first used as, so one name is
	// not both a counter and a timing.
	kinds map[string]MetricKind
	// exposed holds each name the text format writes, by the series name
	// that writes it, so that a timing's NAME_count and a counter called
	// NAME_count cannot both be written.
	exposed map[string]string
	// last holds each series as the previous summary saw it.
	last map[string]Sample
}

// MetricKind is what a series measures.
type MetricKind string

// The kinds of series.
const (
	// Counter only goes up: Add.
	Counter MetricKind = "counter"
	// Gauge is a current value: Set.
	Gauge MetricKind = "gauge"
	// Timing is a count, a sum, and a maximum of durations: Observe.
	Timing MetricKind = "timing"
)

// Label is one label pair of a series.
type Label struct{ Name, Value string }

// Sample is a series as it stood when read. Value is a counter's total
// or a gauge's value; a timing has Count, Sum, and Max instead.
type Sample struct {
	Name   string
	Labels []Label
	Kind   MetricKind
	Value  float64
	Count  uint64
	Sum    time.Duration
	Max    time.Duration
}

// Key names the series as name{label="value",...}, as the Prometheus
// text format does.
func (s Sample) Key() string { return seriesKey(s.Name, s.Labels) }

// Mean is a timing's mean duration, or 0 before its first observation.
func (s Sample) Mean() time.Duration {
	if s.Count == 0 {
		return 0
	}
	return s.Sum / time.Duration(s.Count)
}

type series struct {
	Sample
}

var metricNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// uptimeName is the gauge WriteText adds, which no series may take.
const uptimeName = "airut_uptime_seconds"

func newMetrics() *Metrics {
	return &Metrics{start: time.Now(), series: map[string]*series{}, kinds: map[string]MetricKind{},
		exposed: map[string]string{uptimeName: uptimeName}, last: map[string]Sample{}}
}

// exposedNames are the names the text format writes for a series.
func exposedNames(name string, kind MetricKind) []string {
	if kind == Timing {
		return []string{name, name + "_count", name + "_sum", name + "_max"}
	}
	return []string{name}
}

// Uptime is how long since the bot was made.
func (m *Metrics) Uptime() time.Duration { return time.Since(m.start) }

// Add adds n to a counter. A negative n is ignored: a counter only goes
// up.
func (m *Metrics) Add(name string, n float64, labels ...string) {
	if n < 0 || math.IsNaN(n) {
		return
	}
	m.update(name, Counter, labels, func(s *series) { s.Value += n })
}

// Set sets a gauge.
func (m *Metrics) Set(name string, v float64, labels ...string) {
	m.update(name, Gauge, labels, func(s *series) { s.Value = v })
}

// Observe records a duration in a timing.
func (m *Metrics) Observe(name string, d time.Duration, labels ...string) {
	m.update(name, Timing, labels, func(s *series) {
		s.Count++
		s.Sum += d
		s.Max = max(s.Max, d)
	})
}

// Since records the time since start in a timing, for
// defer m.Since("x_seconds", time.Now()).
func (m *Metrics) Since(name string, start time.Time, labels ...string) {
	m.Observe(name, time.Since(start), labels...)
}

// update applies f to the series. It panics on a malformed name or
// labels, a name used as two kinds, or a name whose exposition another
// name already writes, such as a counter called NAME_count beside a
// timing called NAME: each is a mistake in the bot's code, not in its
// input, and shows on the first call.
func (m *Metrics) update(name string, kind MetricKind, labels []string, f func(*series)) {
	ls := pairs(name, labels)
	key := seriesKey(name, ls)
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.kinds[name]; ok && k != kind {
		panic(fmt.Sprintf("airut: metric %q is a %s, not a %s", name, k, kind))
	} else if !ok {
		names := exposedNames(name, kind)
		for _, n := range names {
			if owner, taken := m.exposed[n]; taken {
				panic(fmt.Sprintf("airut: metric %q would write %q, which %q already writes", name, n, owner))
			}
		}
		for _, n := range names {
			m.exposed[n] = name
		}
	}
	m.kinds[name] = kind
	s := m.series[key]
	if s == nil {
		s = &series{Sample{Name: name, Labels: ls, Kind: kind}}
		m.series[key] = s
	}
	f(s)
}

// pairs reads label pairs, sorted by name.
func pairs(name string, labels []string) []Label {
	if !metricNameRE.MatchString(name) {
		panic(fmt.Sprintf("airut: metric name %q must match %s", name, metricNameRE))
	}
	if len(labels)%2 != 0 {
		panic(fmt.Sprintf("airut: metric %q: labels come in name, value pairs", name))
	}
	ls := make([]Label, 0, len(labels)/2)
	for i := 0; i < len(labels); i += 2 {
		if !metricNameRE.MatchString(labels[i]) || strings.HasPrefix(labels[i], "__") {
			panic(fmt.Sprintf("airut: metric %q: label name %q must match %s", name, labels[i], metricNameRE))
		}
		ls = append(ls, Label{labels[i], labels[i+1]})
	}
	slices.SortFunc(ls, func(a, b Label) int { return strings.Compare(a.Name, b.Name) })
	for i := 1; i < len(ls); i++ {
		if ls[i].Name == ls[i-1].Name {
			panic(fmt.Sprintf("airut: metric %q: label %q given twice", name, ls[i].Name))
		}
	}
	return ls
}

func seriesKey(name string, ls []Label) string {
	if len(ls) == 0 {
		return name
	}
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('{')
	for i, l := range ls {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Name)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(l.Value))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(v string) string { return labelEscaper.Replace(v) }

// Snapshot returns every series, sorted by name and then labels.
func (m *Metrics) Snapshot() []Sample {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot()
}

// snapshot is Snapshot with m.mu held.
func (m *Metrics) snapshot() []Sample {
	out := make([]Sample, 0, len(m.series))
	for _, s := range m.series {
		s := s.Sample
		s.Labels = slices.Clone(s.Labels)
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Sample) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Key(), b.Key())
	})
	return out
}

// Get returns one series, and whether it exists.
func (m *Metrics) Get(name string, labels ...string) (Sample, bool) {
	key := seriesKey(name, pairs(name, labels))
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.series[key]
	if !ok {
		return Sample{}, false
	}
	out := s.Sample
	out.Labels = slices.Clone(out.Labels)
	return out, true
}

// Changed returns the series that changed since the last call, as
// deltas: a counter's increase, a gauge's new value, and a timing's
// observations since then, with the maximum of all time.
func (m *Metrics) Changed() []Sample {
	var out []Sample
	// One lock covers the read and the new baseline, so two calls
	// cannot each subtract the other's baseline.
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.snapshot() {
		key := s.Key()
		prev, seen := m.last[key]
		m.last[key] = s
		switch s.Kind {
		case Counter:
			if s.Value != prev.Value {
				s.Value -= prev.Value
				out = append(out, s)
			}
		case Gauge:
			if !seen || s.Value != prev.Value {
				out = append(out, s)
			}
		case Timing:
			if s.Count != prev.Count {
				s.Count -= prev.Count
				s.Sum -= prev.Sum
				out = append(out, s)
			}
		}
	}
	return out
}

// logSummary logs the series that changed since the last summary, one
// attribute per series.
func (m *Metrics) logSummary(log *slog.Logger) {
	changed := m.Changed()
	attrs := make([]any, 0, len(changed)+1)
	attrs = append(attrs, slog.Int64("uptime_seconds", int64(m.Uptime().Seconds())))
	for _, s := range changed {
		switch s.Kind {
		case Timing:
			attrs = append(attrs, slog.Group(s.Key(),
				slog.Uint64("count", s.Count),
				slog.Float64("mean_seconds", round(s.Mean().Seconds())),
				slog.Float64("max_seconds", round(s.Max.Seconds()))))
		default:
			attrs = append(attrs, slog.Float64(s.Key(), s.Value))
		}
	}
	log.Info("metrics summary", attrs...)
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }

// Handler serves the metrics in the Prometheus text exposition format.
// A timing is a summary: NAME_count and NAME_sum in seconds, with
// NAME_max beside it as a gauge.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.WriteText(w)
	})
}

// WriteText writes the metrics in the Prometheus text exposition format.
func (m *Metrics) WriteText(w io.Writer) error {
	snap := m.Snapshot()
	snap = append(snap, Sample{Name: uptimeName, Kind: Gauge, Value: m.Uptime().Seconds()})
	var b strings.Builder
	typed := map[string]bool{}
	head := func(name, kind string) {
		if !typed[name] {
			typed[name] = true
			fmt.Fprintf(&b, "# TYPE %s %s\n", name, kind)
		}
	}
	for _, s := range snap {
		switch s.Kind {
		case Counter:
			head(s.Name, "counter")
			fmt.Fprintf(&b, "%s %s\n", s.Key(), num(s.Value))
		case Gauge:
			head(s.Name, "gauge")
			fmt.Fprintf(&b, "%s %s\n", s.Key(), num(s.Value))
		case Timing:
			head(s.Name, "summary")
			fmt.Fprintf(&b, "%s %d\n", seriesKey(s.Name+"_count", s.Labels), s.Count)
			fmt.Fprintf(&b, "%s %s\n", seriesKey(s.Name+"_sum", s.Labels), num(s.Sum.Seconds()))
		}
	}
	// A summary family holds only _count and _sum, so the maxima follow
	// as gauge families of their own.
	for _, s := range snap {
		if s.Kind == Timing {
			head(s.Name+"_max", "gauge")
			fmt.Fprintf(&b, "%s %s\n", seriesKey(s.Name+"_max", s.Labels), num(s.Max.Seconds()))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func num(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// serveMetrics listens on addr and serves /metrics until stop is called.
func (b *Bot) serveMetrics(addr string) (stop func(), err error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("airut: metrics: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", b.metrics.Handler())
	// A scrape is small, so a client that takes longer than this to read
	// one is holding the connection, not reading it.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	b.cfg.Logger.Info("serving metrics", "addr", ln.Addr().String())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			b.cfg.Logger.Error("metrics server stopped", "error_kind", "serve")
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if srv.Shutdown(ctx) != nil {
			// Shutdown leaves the connections it could not wait out open.
			srv.Close()
		}
		<-done
	}, nil
}

// summarize logs a summary every period until ctx ends. Run logs the
// last one, once the invocations in flight have finished.
func (b *Bot) summarize(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.metrics.logSummary(b.cfg.Logger)
		}
	}
}
