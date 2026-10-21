package sessions

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"

	"terva.sh/airut/agent/sessions/dbq"
)

// ErrNoMemory is the error of a memory that does not exist.
var ErrNoMemory = errors.New("sessions: no such memory")

// Memory is one stored memory. Its Key and Text are chat, or model text
// drawn from chat, and are never logged.
type Memory struct {
	ID         int64
	Scope      string // user, channel, or bot
	ScopeKey   string
	Key        string
	Text       string
	Source     string // user, model, or admin
	Author     string
	AuthorName string
	Status     string // active, pending, or archived
	Created    time.Time
	LastUsed   time.Time
}

// The statuses.
const (
	Active   = "active"
	Pending  = "pending"
	Archived = "archived"
)

// archivedPerActive is how many archived memories a scope keeps for each
// active one it may hold. The oldest beyond it are deleted, so a busy
// channel's archive stays bounded.
const archivedPerActive = 4

// nearDuplicate is the share of words two texts must have in common, of
// the words either has, to count as one memory.
const nearDuplicate = 0.8

// SaveMemory stores m in its scope, which holds at most limit active
// memories, and returns what was stored:
//
//   - A memory with the key of another in the scope replaces it: an
//     active one replaces an active or archived one, keeping its id, and
//     a pending one only another pending one, under a new id.
//   - One whose words nearly repeat an active memory's, or for a pending
//     one a pending memory's too, refreshes that memory instead, which is
//     returned.
//   - Past limit, the least recently used active memories are archived,
//     and past archivedPerActive times limit the oldest archived ones are
//     deleted.
//
// A limit of zero or less caps nothing.
func (d *DB) SaveMemory(ctx context.Context, m Memory, limit int) (Memory, error) {
	now := d.now()
	var out Memory
	err := d.tx(ctx, func(q *dbq.Queries) error {
		rows, err := q.ScopeMemories(ctx, dbq.ScopeMemoriesParams{Scope: m.Scope, ScopeKey: m.ScopeKey})
		if err != nil {
			return err
		}
		matches := func(r dbq.Memory) bool {
			if m.Status == Pending {
				return r.Status == Pending
			}
			return r.Status != Pending
		}
		words := wordSet(m.Text)
		if m.Key != "" {
			for _, r := range rows {
				if r.Key == m.Key && matches(r) && r.Status == Pending {
					if similar(words, wordSet(r.Text)) {
						// The same proposal again: what was asked about
						// stands, so it keeps its id and its text.
						break
					}
					// A pending memory someone was asked about, or an
					// approver was told of, is replaced under a new id, so
					// an answer about the old text never makes the new one
					// active. The near-duplicate check below cannot match
					// the deleted row, which is not similar to m.
					if err := q.DeleteMemory(ctx, r.ID); err != nil {
						return err
					}
					break
				}
				if r.Key == m.Key && matches(r) {
					r, err := q.ReplaceMemory(ctx, dbq.ReplaceMemoryParams{
						Text: m.Text, Source: m.Source, Author: m.Author, AuthorName: m.AuthorName,
						Status: m.Status, Created: now.UnixMilli(), LastUsed: now.UnixMilli(), ID: r.ID,
					})
					if err != nil {
						return err
					}
					out = memory(r)
					return d.cap(ctx, q, m.Scope, m.ScopeKey, limit)
				}
			}
		}
		for _, r := range rows {
			if (r.Status == Active || m.Status == Pending && r.Status == Pending) && similar(words, wordSet(r.Text)) {
				if err := q.TouchMemory(ctx, dbq.TouchMemoryParams{LastUsed: now.UnixMilli(), ID: r.ID}); err != nil {
					return err
				}
				r.LastUsed = now.UnixMilli()
				out = memory(r)
				return nil
			}
		}
		r, err := q.InsertMemory(ctx, dbq.InsertMemoryParams{
			Scope: m.Scope, ScopeKey: m.ScopeKey, Key: m.Key, Text: m.Text, Source: m.Source,
			Author: m.Author, AuthorName: m.AuthorName, Status: m.Status,
			Created: now.UnixMilli(), LastUsed: now.UnixMilli(),
		})
		if err != nil {
			return err
		}
		out = memory(r)
		return d.cap(ctx, q, m.Scope, m.ScopeKey, limit)
	})
	return out, err
}

// cap archives the least recently used of a scope's active memories past
// limit, and deletes its oldest archived ones past archivedPerActive times
// limit.
func (d *DB) cap(ctx context.Context, q *dbq.Queries, scope, key string, limit int) error {
	if limit <= 0 {
		return nil
	}
	rows, err := q.ScopeMemories(ctx, dbq.ScopeMemoriesParams{Scope: scope, ScopeKey: key})
	if err != nil {
		return err
	}
	var active, archived []dbq.Memory
	for _, r := range rows {
		switch r.Status {
		case Active:
			active = append(active, r)
		case Archived:
			archived = append(archived, r)
		}
	}
	sort.SliceStable(active, func(i, j int) bool { return active[i].LastUsed < active[j].LastUsed })
	for _, r := range active[:max(0, len(active)-limit)] {
		if _, err := q.SetMemoryStatus(ctx, dbq.SetMemoryStatusParams{Status: Archived, ID: r.ID}); err != nil {
			return err
		}
		archived = append(archived, r)
	}
	sort.SliceStable(archived, func(i, j int) bool { return archived[i].LastUsed < archived[j].LastUsed })
	for _, r := range archived[:max(0, len(archived)-archivedPerActive*limit)] {
		if err := q.DeleteMemory(ctx, r.ID); err != nil {
			return err
		}
	}
	return nil
}

// Memories returns the memories of one scope, oldest first.
func (d *DB) Memories(ctx context.Context, scope, key string) ([]Memory, error) {
	rows, err := d.q.ScopeMemories(ctx, dbq.ScopeMemoriesParams{Scope: scope, ScopeKey: key})
	if err != nil {
		return nil, err
	}
	out := make([]Memory, len(rows))
	for i, r := range rows {
		out[i] = memory(r)
	}
	return out, nil
}

// Memory returns one memory, or ErrNoMemory.
func (d *DB) Memory(ctx context.Context, id int64) (Memory, error) {
	r, err := d.q.GetMemory(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Memory{}, ErrNoMemory
	}
	return memory(r), err
}

// SetMemoryStatus changes a memory's status, and caps its scope at limit
// active memories as SaveMemory does. A memory made active replaces the
// active or archived ones with its key, as a write would. It returns
// ErrNoMemory for one that does not exist.
func (d *DB) SetMemoryStatus(ctx context.Context, id int64, status string, limit int) error {
	return d.tx(ctx, func(q *dbq.Queries) error {
		r, err := q.GetMemory(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoMemory
		}
		if err != nil {
			return err
		}
		if status == Active && r.Key != "" {
			rows, err := q.ScopeMemories(ctx, dbq.ScopeMemoriesParams{Scope: r.Scope, ScopeKey: r.ScopeKey})
			if err != nil {
				return err
			}
			for _, o := range rows {
				if o.ID != id && o.Key == r.Key && o.Status != Pending {
					if err := q.DeleteMemory(ctx, o.ID); err != nil {
						return err
					}
				}
			}
		}
		if _, err := q.SetMemoryStatus(ctx, dbq.SetMemoryStatusParams{Status: status, ID: id}); err != nil {
			return err
		}
		if err := q.TouchMemory(ctx, dbq.TouchMemoryParams{LastUsed: d.now().UnixMilli(), ID: id}); err != nil {
			return err
		}
		return d.cap(ctx, q, r.Scope, r.ScopeKey, limit)
	})
}

// TouchMemories records that the memories were used now.
func (d *DB) TouchMemories(ctx context.Context, ids ...int64) error {
	return d.tx(ctx, func(q *dbq.Queries) error {
		for _, id := range ids {
			if err := q.TouchMemory(ctx, dbq.TouchMemoryParams{LastUsed: d.now().UnixMilli(), ID: id}); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteMemory deletes m if it is still as it was read: the same status,
// author, text, and time of writing, which a replacement changes. It
// reports whether it deleted it, so a memory replaced or approved since
// it was read, or already gone, is left alone.
func (d *DB) DeleteMemory(ctx context.Context, m Memory) (bool, error) {
	n, err := d.q.DeleteMemoryAsRead(ctx, dbq.DeleteMemoryAsReadParams{
		ID: m.ID, Status: m.Status, Author: m.Author, Text: m.Text, Created: m.Created.UnixMilli(),
	})
	return n > 0, err
}

func (d *DB) tx(ctx context.Context, f func(*dbq.Queries) error) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(d.q.WithTx(tx)); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func memory(r dbq.Memory) Memory {
	return Memory{
		ID: r.ID, Scope: r.Scope, ScopeKey: r.ScopeKey, Key: r.Key, Text: r.Text, Source: r.Source,
		Author: r.Author, AuthorName: r.AuthorName, Status: r.Status,
		Created: time.UnixMilli(r.Created), LastUsed: time.UnixMilli(r.LastUsed),
	}
}

// Words returns the distinct words of s, lowercased, as the near-duplicate
// check and recall compare them.
func Words(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

func wordSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, w := range Words(s) {
		set[w] = true
	}
	return set
}

// similar reports whether two word sets share at least nearDuplicate of
// the words either has. Two empty sets are similar.
func similar(a, b map[string]bool) bool {
	both := 0
	for w := range a {
		if b[w] {
			both++
		}
	}
	either := len(a) + len(b) - both
	return either == 0 || float64(both)/float64(either) >= nearDuplicate
}
