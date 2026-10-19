// Package sessions persists airut's agent conversations in SQLite, one
// database per bot.
//
// Each session, named by the core's session key, is a log of the writes
// terva's engine made through core.TranscriptStore. Reload replays the
// log into terva's own core.MemoryTranscriptStore and returns its
// Transcript, so terva decides what a resume sees: the usage totals,
// compactions, excluded images, and repaired tool calls.
//
// Sessions idle longer than the retention are deleted. The database holds
// conversation text, so it is state to protect and to back up, like a
// Matrix crypto store; nothing in it is ever logged.
package sessions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"terva.sh/terva/packages/core"
	"terva.sh/terva/packages/provider"

	"terva.sh/airut/agent/sessions/dbq"
)

// ErrForgotten is returned by a write to a session that was forgotten or
// expired after it was opened. Open it again to start afresh.
var ErrForgotten = errors.New("sessions: the session was forgotten")

// DefaultRetention is how long an idle session is kept when Options sets
// no retention.
const DefaultRetention = 30 * 24 * time.Hour

// sweepEvery is how often an open DB deletes expired sessions.
const sweepEvery = time.Hour

// Options configure Open. The zero value is usable.
type Options struct {
	// Retention is how long a session is kept after its last write.
	// Zero means DefaultRetention.
	Retention time.Duration
	// Logger receives the sweep's counts and errors. Nil discards them.
	Logger *slog.Logger
	// Now is the clock, for tests. Nil means time.Now.
	Now func() time.Time
}

// DB is one bot's session database.
type DB struct {
	db        *sql.DB
	q         *dbq.Queries
	retention time.Duration
	log       *slog.Logger
	now       func() time.Time

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// Open opens or creates the database at path, brings its schema up to
// date, deletes the sessions that have expired, and keeps deleting them
// hourly until Close.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	if opts.Retention < 0 {
		return nil, fmt.Errorf("sessions: retention %s is negative", opts.Retention)
	}
	if opts.Retention == 0 {
		opts.Retention = DefaultRetention
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	pool, err := openPool(path)
	if err != nil {
		return nil, err
	}
	// A bot's writes are small and serialized per session; one connection
	// keeps SQLite's locking out of the picture.
	pool.SetMaxOpenConns(1)
	if err := migrate(ctx, pool, opts.Now()); err != nil {
		pool.Close()
		return nil, err
	}
	d := &DB{
		db:        pool,
		q:         dbq.New(pool),
		retention: opts.Retention,
		log:       opts.Logger,
		now:       opts.Now,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	if _, err := d.Sweep(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	go d.sweepLoop()
	return d, nil
}

// Close stops the sweep and closes the database. Sessions opened from d
// fail every write afterwards.
func (d *DB) Close() error {
	var err error
	d.closeOnce.Do(func() {
		close(d.stop)
		<-d.done
		err = d.db.Close()
	})
	return err
}

func (d *DB) sweepLoop() {
	defer close(d.done)
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		select {
		case <-d.stop:
			return
		case <-t.C:
			if _, err := d.Sweep(context.Background()); err != nil {
				d.log.Warn("sessions: sweep failed", "err", err)
			}
		}
	}
}

// Sweep deletes every session idle longer than the retention, with its
// log, and reports how many it deleted.
func (d *DB) Sweep(ctx context.Context) (int64, error) {
	n, err := d.q.DeleteIdleSessions(ctx, d.now().Add(-d.retention).UnixMilli())
	if err != nil {
		return 0, err
	}
	if n > 0 {
		d.log.Info("sessions: expired idle sessions", "deleted", n)
	}
	return n, nil
}

// Forget deletes the session named key and its log. A handle opened on it
// earlier fails its next write with ErrForgotten. Forgetting a session
// that does not exist is not an error.
func (d *DB) Forget(ctx context.Context, key string) error {
	_, err := d.q.DeleteSession(ctx, key)
	return err
}

// Session opens the session named key, creating it if there is none, and
// counts the open as activity.
func (d *DB) Session(ctx context.Context, key string) (*Session, error) {
	now := d.now().UnixMilli()
	id, err := d.q.OpenSession(ctx, dbq.OpenSessionParams{Key: key, CreatedAt: now, LastActive: now})
	if err != nil {
		return nil, err
	}
	return &Session{d: d, id: id}, nil
}

// Session is one conversation's store. It implements core.TranscriptStore
// for the agent that owns it, and Reload for the host that resumes it.
type Session struct {
	d  *DB
	id int64
	// mu orders this handle's writes. The engine serializes its own, but a
	// host may Reload while it writes.
	mu sync.Mutex
}

var _ core.TranscriptStore = (*Session)(nil)

// The entry kinds, one per TranscriptStore method.
const (
	kindMessage        = "message"
	kindCompaction     = "compaction"
	kindUsage          = "usage"
	kindToolGroup      = "tool_group"
	kindImageExclusion = "image_exclusion"
)

// compactionBody is a compaction entry: the replacement transcript and
// the compaction's own record.
type compactionBody struct {
	Messages []wireMessage      `json:"messages"`
	Result   core.CompactResult `json:"result"`
}

// AppendMessage records a message added to the transcript.
func (s *Session) AppendMessage(m provider.Message) error {
	w, err := encodeMessage(m)
	if err != nil {
		return err
	}
	return s.append(kindMessage, w)
}

// AppendCompaction records that compaction replaced the transcript.
func (s *Session) AppendCompaction(messages []provider.Message, res core.CompactResult) error {
	ws, err := encodeMessages(messages)
	if err != nil {
		return err
	}
	return s.append(kindCompaction, compactionBody{Messages: ws, Result: res})
}

// AppendUsage records one request's usage.
func (s *Session) AppendUsage(r core.UsageRecord) error {
	return s.append(kindUsage, r)
}

// AppendToolGroupActivation records that the model activated a lazy tool
// group.
func (s *Session) AppendToolGroupActivation(group string) error {
	return s.append(kindToolGroup, group)
}

// AppendImageExclusion records that the provider rejected an image.
func (s *Session) AppendImageExclusion(sha256Hex string) error {
	return s.append(kindImageExclusion, sha256Hex)
}

// append adds one entry to the log and marks the session active, in one
// transaction, so the log never holds an entry for a session the sweep
// thought idle.
func (s *Session) append(kind string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("sessions: encoding a %s entry: %w", kind, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ctx := context.Background()
	tx, err := s.d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.d.q.WithTx(tx)
	n, err := q.TouchSession(ctx, dbq.TouchSessionParams{LastActive: s.d.now().UnixMilli(), ID: s.id})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrForgotten
	}
	seq, err := q.NextSeq(ctx, s.id)
	if err != nil {
		return err
	}
	if err := q.InsertEntry(ctx, dbq.InsertEntryParams{SessionID: s.id, Seq: seq, Kind: kind, Body: body}); err != nil {
		return err
	}
	return tx.Commit()
}

// Reload replays the session's log through terva's memory store and
// returns the transcript a resume starts from. A forgotten session
// reloads empty.
func (s *Session) Reload() (core.Transcript, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.d.q.ListEntries(context.Background(), s.id)
	if err != nil {
		return core.Transcript{}, err
	}
	mem := core.NewMemoryTranscriptStore()
	for i, e := range entries {
		if err := replay(mem, e.Kind, e.Body); err != nil {
			return core.Transcript{}, fmt.Errorf("sessions: log entry %d (%s): %w", i+1, e.Kind, err)
		}
	}
	return mem.Transcript(), nil
}

func replay(mem *core.MemoryTranscriptStore, kind string, body []byte) error {
	switch kind {
	case kindMessage:
		var w wireMessage
		if err := json.Unmarshal(body, &w); err != nil {
			return err
		}
		m, err := decodeMessage(w)
		if err != nil {
			return err
		}
		return mem.AppendMessage(m)
	case kindCompaction:
		var c compactionBody
		if err := json.Unmarshal(body, &c); err != nil {
			return err
		}
		ms, err := decodeMessages(c.Messages)
		if err != nil {
			return err
		}
		return mem.AppendCompaction(ms, c.Result)
	case kindUsage:
		var r core.UsageRecord
		if err := json.Unmarshal(body, &r); err != nil {
			return err
		}
		return mem.AppendUsage(r)
	case kindToolGroup:
		var g string
		if err := json.Unmarshal(body, &g); err != nil {
			return err
		}
		return mem.AppendToolGroupActivation(g)
	case kindImageExclusion:
		var h string
		if err := json.Unmarshal(body, &h); err != nil {
			return err
		}
		return mem.AppendImageExclusion(h)
	default:
		return errors.New("unknown entry kind")
	}
}
