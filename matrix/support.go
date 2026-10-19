package matrix

import (
	"container/list"
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"sync"

	"github.com/rs/zerolog"
)

// lru is a bounded map that forgets what was least recently put or read.
type lru[K comparable, V any] struct {
	mu    sync.Mutex
	max   int
	order *list.List
	items map[K]*list.Element
}

type entry[K comparable, V any] struct {
	k K
	v V
}

func newLRU[K comparable, V any](max int) *lru[K, V] {
	return &lru[K, V]{max: max, order: list.New(), items: map[K]*list.Element{}}
}

func (l *lru[K, V]) put(k K, v V) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.items[k]; ok {
		e.Value = entry[K, V]{k, v}
		l.order.MoveToFront(e)
		return
	}
	l.items[k] = l.order.PushFront(entry[K, V]{k, v})
	if l.order.Len() > l.max {
		last := l.order.Back()
		l.order.Remove(last)
		delete(l.items, last.Value.(entry[K, V]).k)
	}
}

func (l *lru[K, V]) get(k K) (V, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.items[k]
	if !ok {
		var zero V
		return zero, false
	}
	l.order.MoveToFront(e)
	return e.Value.(entry[K, V]).v, true
}

// zerologTo returns a zerolog logger that forwards rihma's warnings and
// errors to log. rihma promises its logger never receives tokens, keys,
// or message content; below warning it reports routine sync detail the
// bot's log has no use for.
func zerologTo(log *slog.Logger) zerolog.Logger {
	return zerolog.New(slogWriter{log.With("source", "rihma")}).Level(zerolog.WarnLevel)
}

// slogWriter receives zerolog's JSON lines and logs each through slog.
type slogWriter struct{ log *slog.Logger }

func (w slogWriter) Write(p []byte) (int, error) {
	var fields map[string]any
	if err := json.Unmarshal(p, &fields); err != nil {
		w.log.Warn("rihma logged a line that is not JSON", "bytes", len(p))
		return len(p), nil
	}
	level := slog.LevelInfo
	switch fields[zerolog.LevelFieldName] {
	case "trace", "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error", "fatal", "panic":
		level = slog.LevelError
	}
	msg, _ := fields[zerolog.MessageFieldName].(string)
	delete(fields, zerolog.LevelFieldName)
	delete(fields, zerolog.MessageFieldName)
	delete(fields, zerolog.TimestampFieldName)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	attrs := make([]any, 0, len(keys))
	for _, k := range keys {
		attrs = append(attrs, slog.Any(k, fields[k]))
	}
	w.log.Log(context.Background(), level, msg, attrs...)
	return len(p), nil
}
