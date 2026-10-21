package airut

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MemoryScope says whom or where a memory is about.
type MemoryScope string

// The memory scopes. A user memory is one person's facts and
// preferences, a channel memory one room's, and a bot memory is shared
// by every room.
const (
	UserMemory    MemoryScope = "user"
	ChannelMemory MemoryScope = "channel"
	BotMemory     MemoryScope = "bot"
)

// MemorySource says who wrote a memory: a person with remember, the
// model through the remember tool, or the app's own code.
type MemorySource string

const (
	FromUser  MemorySource = "user"
	FromModel MemorySource = "model"
	FromAdmin MemorySource = "admin"
)

// MemoryStatus is a memory's state. Only an active memory is shown to
// the agent as it is; an archived one is recalled when a turn mentions
// what it is about, and a pending one waits for consent or approval and
// is never shown.
type MemoryStatus string

const (
	MemoryActive   MemoryStatus = "active"
	MemoryPending  MemoryStatus = "pending"
	MemoryArchived MemoryStatus = "archived"
)

// MemoryWrite says what happens when the model asks to remember
// something in a scope: nothing, a question to the caller first, or the
// write.
type MemoryWrite string

const (
	WriteOff  MemoryWrite = "off"
	WriteAsk  MemoryWrite = "ask"
	WriteAuto MemoryWrite = "auto"
)

// The caps on a memory, in bytes.
const (
	MaxMemoryText = 500
	MaxMemoryKey  = 64
)

// Memory is one thing the bot remembers. Its Text and Key are chat, or
// model text drawn from chat: show them as data, and never log them.
type Memory struct {
	ID    int64
	Scope MemoryScope
	// ScopeKey names whom or where, by ids only: the person as
	// "platform/user/id", the channel as "platform/channel/id", or empty
	// for the bot.
	ScopeKey string
	// Key is a short name for what the memory is about. A memory with
	// the key of another in its scope replaces it.
	Key      string
	Text     string
	Source   MemorySource
	Author   User
	Status   MemoryStatus
	Created  time.Time
	LastUsed time.Time
}

// MemoryQuery selects memories: those of the person, the channel, and
// the bot it names, in any of Statuses, or in every status when it
// names none.
type MemoryQuery struct {
	User     string // a user scope key, or empty for none
	Channel  string // a channel scope key, or empty for none
	Bot      bool
	Statuses []MemoryStatus
}

// ErrNoMemory is the error of a memory that does not exist.
var ErrNoMemory = errors.New("airut: no such memory")

// MemoryKeeper is an Agent that keeps long-term memory. With one, a
// running bot has the built-in remember, memories, and unremember
// commands, and remember is a tool Bot.Tools hands an agent turn. The
// core decides who may write, read, and remove a memory; the keeper
// stores it, and decides what is a duplicate and what is archived.
type MemoryKeeper interface {
	// SaveMemory stores m and returns what was stored. A memory with the
	// key of another in its scope replaces it, and one that nearly
	// repeats an active one refreshes that one instead, which is
	// returned. A pending memory it replaces must get a new id, so
	// consent or approval given to the old text never makes the new
	// text active.
	SaveMemory(ctx context.Context, m Memory) (Memory, error)
	// Memories returns the memories q selects, oldest first.
	Memories(ctx context.Context, q MemoryQuery) ([]Memory, error)
	// Memory returns one memory, or ErrNoMemory.
	Memory(ctx context.Context, id int64) (Memory, error)
	// SetMemoryStatus changes a memory's status, as when a pending one
	// is approved. A memory made active replaces the others with its
	// key, as a write would. It returns ErrNoMemory for one that does not
	// exist.
	SetMemoryStatus(ctx context.Context, id int64, s MemoryStatus) error
	// DeleteMemory deletes m only if it is still as Memory or Memories
	// returned it, with the same status, author, text, and Created, and
	// reports whether it did. The core decides who may remove a memory
	// from what it read, so a memory replaced or approved since must be
	// left alone.
	DeleteMemory(ctx context.Context, m Memory) (bool, error)
}

// UserScope is the scope key of u's memories.
func UserScope(u User) string { return string(u.Platform) + "/user/" + u.ID }

// ChannelScope is the scope key of ch's memories.
func ChannelScope(ch Channel) string { return string(ch.Platform) + "/channel/" + ch.ID }

// memoryWrite is the mode of the model's writes in scope s.
func (b *Bot) memoryWrite(s MemoryScope) MemoryWrite {
	if w, ok := b.cfg.MemoryWrites[s]; ok {
		return w
	}
	return map[MemoryScope]MemoryWrite{UserMemory: WriteAsk, ChannelMemory: WriteAuto, BotMemory: WriteOff}[s]
}

// approver reports whether the caller of c may approve memories.
func (b *Bot) approver(c *Ctx) bool {
	return b.cfg.MemoryApprover != nil && b.cfg.MemoryApprover(c)
}

type rememberArgs struct {
	Channel bool   `airut:",optional,named" desc:"Remember it for this channel, not for you"`
	Bot     bool   `airut:",optional,named" desc:"Remember it for the whole bot"`
	About   *User  `airut:",optional,named" desc:"The person it is about, when it is not you"`
	Key     string `airut:",optional,named" desc:"A short name for what it is about"`
	Text    string `airut:",optional,rest" desc:"What to remember"`
}

// rememberCommand is the built-in remember, which is a tool too.
func rememberCommand(b *Bot, k MemoryKeeper) *Command {
	c, err := newCommand(b, "remember", "Make the bot remember something about you, this channel, or the bot", func(c *Ctx, a rememberArgs) error {
		return b.remember(c, k, a)
	})
	if err != nil {
		panic("airut: the built-in remember command: " + err.Error())
	}
	c.tool = true
	return c
}

func (b *Bot) remember(c *Ctx, k MemoryKeeper, a rememberArgs) error {
	text, key := strings.TrimSpace(a.Text), strings.TrimSpace(a.Key)
	switch {
	case text == "":
		return Errorf("Say what to remember, as: remember TEXT")
	case len(text) > MaxMemoryText || !utf8.ValidString(text):
		return Errorf("That is too long to remember: keep it under %d bytes.", MaxMemoryText)
	case len(key) > MaxMemoryKey || !utf8.ValidString(key):
		return Errorf("That key is too long: keep it under %d bytes.", MaxMemoryKey)
	case a.Channel && a.Bot, a.About != nil && (a.Channel || a.Bot):
		return Errorf("Remember it for one of you, someone, this channel, or the bot.")
	}
	m := Memory{Key: key, Text: text, Source: FromUser, Author: c.inv.Caller, Status: MemoryActive}
	if c.FromAgent() {
		m.Source = FromModel
	}
	switch {
	case a.Bot:
		m.Scope = BotMemory
	case a.Channel:
		m.Scope, m.ScopeKey = ChannelMemory, ChannelScope(c.inv.Channel)
	default:
		m.Scope, m.ScopeKey = UserMemory, UserScope(c.inv.Caller)
		if a.About != nil && UserScope(*a.About) != m.ScopeKey {
			// Nobody may consent for someone else, so it waits for them.
			m.ScopeKey, m.Status = UserScope(*a.About), MemoryPending
		}
	}
	if m.Source == FromModel {
		switch b.memoryWrite(m.Scope) {
		case WriteOff:
			return Errorf("Remembering things in that scope is turned off.")
		case WriteAsk:
			if m.Status == MemoryActive {
				ok, err := c.confirmMemory(m)
				if err != nil || !ok {
					return errors.Join(Errorf("The caller did not agree to remember that."), err)
				}
			}
		}
	}
	if m.Scope == BotMemory && !b.approver(c) {
		m.Status = MemoryPending
	}
	saved, err := k.SaveMemory(c.ctx, m)
	if err != nil {
		return err
	}
	b.metrics.Add("airut_memory_writes_total", 1, "scope", string(saved.Scope), "source", string(saved.Source), "status", string(saved.Status))
	b.cfg.Logger.Debug("memory saved", "invocation", c.inv.ID, "memory", saved.ID, "scope", string(saved.Scope), "status", string(saved.Status))
	if saved.Status == MemoryPending && saved.Scope == BotMemory && b.cfg.OnPendingMemory != nil {
		b.cfg.OnPendingMemory(c, saved)
	}
	_, err = c.Reply(rememberedReply(saved))
	return err
}

func rememberedReply(m Memory) string {
	id := fmt.Sprintf(" (memory %d)", m.ID)
	switch {
	case m.Status != MemoryPending:
		return "Remembered" + id + "."
	case m.Scope == BotMemory:
		return "Saved for approval" + id + "."
	}
	return "Saved until they agree to it" + id + "."
}

// confirmMemory asks the caller whether to remember m.
func (c *Ctx) confirmMemory(m Memory) (bool, error) {
	where := map[MemoryScope]string{UserMemory: "about you", ChannelMemory: "for this channel"}[m.Scope]
	got, err := c.Ask(fmt.Sprintf("May I remember this %s? %q", where, m.Text), "Remember", "Don't")
	return got == "Remember", err
}

// askPendingMemories asks the caller about memories others saved about
// them, at most max of them, and makes each one active or deletes it.
// One left unanswered stays pending, to be asked again.
func (b *Bot) askPendingMemories(c *Ctx, k MemoryKeeper, max int) error {
	ms, err := k.Memories(c.ctx, MemoryQuery{User: UserScope(c.inv.Caller), Statuses: []MemoryStatus{MemoryPending}})
	if err != nil {
		return err
	}
	for i, m := range ms {
		if i == max {
			break
		}
		got, err := c.Ask(fmt.Sprintf("%s asked me to remember this about you: %q. May I?", name(m.Author), m.Text), "Remember", "Don't")
		if err != nil {
			return err
		}
		if got == "Remember" {
			err = k.SetMemoryStatus(c.ctx, m.ID, MemoryActive)
		} else {
			_, err = k.DeleteMemory(c.ctx, m)
		}
		if err != nil && !errors.Is(err, ErrNoMemory) {
			return err
		}
		b.metrics.Add("airut_memory_consents_total", 1, "answer", map[bool]string{true: "yes", false: "no"}[got == "Remember"])
	}
	return nil
}

// memoriesCommand is the built-in memories.
func memoriesCommand(b *Bot, k MemoryKeeper) *Command {
	c, err := newCommand(b, "memories", "List what the bot remembers about you, this channel, and itself", func(c *Ctx) error {
		if err := b.askPendingMemories(c, k, maxPendingAsks); err != nil && !errors.Is(err, ErrAskTimeout) {
			return err
		}
		ms, err := k.Memories(c.ctx, MemoryQuery{User: UserScope(c.inv.Caller), Channel: ChannelScope(c.inv.Channel), Bot: true,
			Statuses: []MemoryStatus{MemoryActive, MemoryPending}})
		if err != nil {
			return err
		}
		_, err = c.ReplyPrivate(listMemories(ms))
		return err
	})
	if err != nil {
		panic("airut: the built-in memories command: " + err.Error())
	}
	return c
}

// maxPendingAsks is how many pending memories one invocation asks about.
const maxPendingAsks = 2

func listMemories(ms []Memory) string {
	if len(ms) == 0 {
		return "I remember nothing about you, this channel, or myself."
	}
	heads := map[MemoryScope]string{UserMemory: "About you:", ChannelMemory: "About this channel:", BotMemory: "For everyone:"}
	var b strings.Builder
	for _, s := range []MemoryScope{UserMemory, ChannelMemory, BotMemory} {
		first := true
		for _, m := range ms {
			if m.Scope != s {
				continue
			}
			if first {
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(heads[s])
				first = false
			}
			fmt.Fprintf(&b, "\n%d. %s (noted by %s on %s", m.ID, m.Text, name(m.Author), m.Created.UTC().Format(time.DateOnly))
			if m.Status == MemoryPending {
				b.WriteString(", waiting for approval")
			}
			b.WriteString(")")
		}
	}
	b.WriteString("\nRemove one with: unremember ID")
	return b.String()
}

type unrememberArgs struct {
	ID int64 `desc:"The memory's number, as memories lists it"`
}

// unrememberCommand is the built-in unremember.
func unrememberCommand(b *Bot, k MemoryKeeper) *Command {
	c, err := newCommand(b, "unremember", "Make the bot forget one memory", func(c *Ctx, a unrememberArgs) error {
		m, err := k.Memory(c.ctx, a.ID)
		if errors.Is(err, ErrNoMemory) || err == nil && !b.mayRemove(c, m) {
			// One the caller may not remove reads as unknown, so an id
			// tells nobody what it holds.
			return Errorf("There is no memory %d you can remove.", a.ID)
		}
		if err != nil {
			return err
		}
		switch deleted, err := k.DeleteMemory(c.ctx, m); {
		case err != nil:
			return err
		case !deleted:
			// It changed after the check, as when it was replaced or
			// approved, so the check no longer holds.
			return Errorf("There is no memory %d you can remove.", a.ID)
		}
		b.cfg.Logger.Debug("memory removed", "invocation", c.inv.ID, "memory", m.ID, "scope", string(m.Scope))
		_, err = c.ReplyPrivate(fmt.Sprintf("Forgot memory %d.", m.ID))
		return err
	})
	if err != nil {
		panic("airut: the built-in unremember command: " + err.Error())
	}
	return c
}

// mayRemove reports whether the caller of c may remove m: a person their
// own memories, an author a pending memory they wrote and a channel
// memory, and an approver any channel or bot memory.
func (b *Bot) mayRemove(c *Ctx, m Memory) bool {
	caller := UserScope(c.inv.Caller)
	author := UserScope(m.Author) == caller
	switch m.Scope {
	case UserMemory:
		return m.ScopeKey == caller || author && m.Status == MemoryPending
	case ChannelMemory:
		return author || b.approver(c)
	case BotMemory:
		return b.approver(c) || author && m.Status == MemoryPending
	}
	return false
}
