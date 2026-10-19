// Package matrix carries an airut Bot to Matrix, on rihma, a cgo-free
// end-to-end-encrypted Matrix client built on mautrix.
//
// A message is a command when it starts with the bot's prefix, or with a
// mention of the bot followed by a command, with or without the prefix:
// "!roll 2d6", "Baphomet: roll 2d6", and "Baphomet: !roll 2d6" all call
// roll. Every other message, and every reaction, reaches the bot's
// OnMessage and OnReaction handlers. Encrypted rooms work as plain ones
// do.
//
// The adapter answers commands only while they are fresh. rihma resumes
// a sync where it stopped, so messages sent while the bot was down
// arrive when it returns; those that are commands older than
// Options.MaxCommandAge are dropped rather than answered late, and the
// rest reach the event handlers as usual, so a bot can still see what it
// missed.
package matrix

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"terva.sh/rihma"

	"terva.sh/airut"
	"terva.sh/airut/config"
)

// DefaultMaxCommandAge is how old a command may be and still be answered.
const DefaultMaxCommandAge = 10 * time.Minute

// Options are the adapter's settings that the configuration file does
// not hold.
type Options struct {
	// AcceptInvite decides whether the bot joins a room it is invited to.
	// When it is nil, the bot joins none, so nobody can pull it into a
	// room it was not meant for.
	AcceptInvite func(room, inviter string) bool
	// MaxCommandAge is how old a command may be and still be answered.
	// Zero means DefaultMaxCommandAge.
	MaxCommandAge time.Duration
	// Logger receives the adapter's logs. Nil means the Host's.
	Logger *slog.Logger
}

// Adapter is an airut.Adapter for Matrix.
type Adapter struct {
	cfg  config.Matrix
	opts Options
}

// New returns an adapter for the account cfg describes.
func New(cfg *config.Matrix, o Options) *Adapter {
	a := &Adapter{opts: o}
	if cfg != nil {
		a.cfg = *cfg
	}
	if a.opts.MaxCommandAge <= 0 {
		a.opts.MaxCommandAge = DefaultMaxCommandAge
	}
	return a
}

// Platform reports airut.Matrix.
func (a *Adapter) Platform() airut.Platform { return airut.Matrix }

// Run logs in, or restores the stored session, and serves the account
// until ctx ends. The state directory holds the session and rihma's
// crypto store, which cannot be rebuilt: lose it and the bot can no
// longer read encrypted history it could read before.
func (a *Adapter) Run(ctx context.Context, h airut.Host) error {
	log := a.opts.Logger
	if log == nil {
		log = h.Logger()
	}
	log = log.With("platform", string(airut.Matrix))
	user := id.UserID(a.cfg.UserID)
	local, _, err := user.Parse()
	if err != nil || a.cfg.Homeserver == "" || a.cfg.StateDir == "" {
		return errors.New("matrix: the configuration needs a homeserver, a user id, and a state directory")
	}
	if err := os.MkdirAll(a.cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("matrix: %w", err)
	}
	ro := rihma.Options{
		Homeserver: a.cfg.Homeserver,
		StateDir:   filepath.Join(a.cfg.StateDir, "rihma"),
		Sessions:   rihma.FileSessionStore{Path: filepath.Join(a.cfg.StateDir, "session.json")},
		DeviceName: a.cfg.DeviceName,
		Logger:     zerologTo(log),
	}
	if !a.cfg.Password.IsZero() {
		ro.Login = &mautrix.ReqLogin{
			Type:       mautrix.AuthTypePassword,
			Identifier: mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: local},
			Password:   a.cfg.Password.Value(),
		}
	}
	c, err := rihma.Open(ctx, ro)
	if err != nil {
		return fmt.Errorf("matrix: opening the client: %w", err)
	}
	if c.UserID != user {
		c.Close()
		return fmt.Errorf("matrix: the stored session is for another account than %s", user)
	}
	s := newSession(ctx, c, h, log, a.opts)
	if dn, err := c.GetOwnDisplayName(ctx); err == nil {
		s.me.display = dn.DisplayName
	}
	s.register()
	err = c.Sync(ctx)
	s.stop()
	c.Close()
	if ctx.Err() != nil {
		return nil
	}
	return fmt.Errorf("matrix: sync stopped: %w", err)
}

// session is one Run's connection. It is also the Conn the adapter
// announces.
type session struct {
	ctx  context.Context
	c    *rihma.Client
	h    airut.Host
	log  *slog.Logger
	opts Options
	me   names
	// commands holds the names of the bot's commands.
	commands map[string]bool

	// files holds the content of recent encrypted attachments, keyed by
	// their URL, because Open needs their keys. threads maps recent
	// messages to their thread root, so a reaction can name its thread.
	// rooms maps the messages the bot sent to the room each went to, so
	// an edit of a private reply goes to the private room.
	files   *lru[string, *event.MessageEventContent]
	threads *lru[id.EventID, id.EventID]
	rooms   *lru[id.EventID, id.RoomID]

	mu      sync.Mutex
	stopped bool
	ready   bool
	wg      sync.WaitGroup

	dmMu sync.Mutex
	dms  map[id.UserID]id.RoomID
}

// The sizes of the session's caches.
const (
	keptFiles   = 1024
	keptThreads = 4096
	keptSent    = 4096
)

func newSession(ctx context.Context, c *rihma.Client, h airut.Host, log *slog.Logger, o Options) *session {
	local, _, _ := c.UserID.Parse()
	s := &session{
		ctx: ctx, c: c, h: h, log: log, opts: o,
		me:      names{id: string(c.UserID), local: local},
		files:   newLRU[string, *event.MessageEventContent](keptFiles),
		threads: newLRU[id.EventID, id.EventID](keptThreads),
		rooms:   newLRU[id.EventID, id.RoomID](keptSent),
		dms:     map[id.UserID]id.RoomID{},
	}
	s.commands = map[string]bool{}
	for _, spec := range h.Specs() {
		s.commands[spec.Name] = true
	}
	return s
}

func (s *session) known(name string) bool { return s.commands[name] }

func (s *session) register() {
	sy := s.c.Handlers()
	sy.OnSync(s.onSync)
	sy.OnEventType(event.EventMessage, s.onMessage)
	sy.OnEventType(event.EventReaction, s.onReaction)
	sy.OnEventType(event.StateMember, s.onMember)
}

// spawn runs f on a goroutine Run waits for. rihma can dispatch an event
// whose key arrived late from a goroutine of its own, even after Sync
// returned, so a spawn after stop is dropped.
func (s *session) spawn(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

func (s *session) stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.wg.Wait()
}

// onSync announces the Conn once a sync has been processed in full, so
// the state store holds the rooms' members by the time OnReady handlers
// run. A sync that resumes from a stored token follows one that was; on
// a first connect, the sync after the initial one does.
func (s *session) onSync(_ context.Context, _ *mautrix.RespSync, since string) bool {
	s.mu.Lock()
	announce := since != "" && !s.ready
	s.ready = s.ready || announce
	s.mu.Unlock()
	if announce {
		s.h.Ready(s.ctx, s)
	}
	return true
}

func (s *session) onMessage(ctx context.Context, evt *event.Event) {
	if skip(evt, s.c.UserID) {
		return
	}
	in, ok := s.parse(evt)
	if !ok {
		return
	}
	resp := s.responder(evt.RoomID, in.thread, evt.ID, evt.Sender)
	if name, rest, ok := in.command(s.h.Prefix(), s.me, s.known); ok {
		if age := time.Since(time.UnixMilli(evt.Timestamp)); age > s.opts.MaxCommandAge {
			s.log.Info("dropped a stale command", "event", string(evt.ID), "age", age.Round(time.Second).String())
			return
		}
		inv := in.invocation(s.user(ctx, evt.RoomID, evt.Sender), name, rest)
		inv.Respond = resp
		s.spawn(func() { s.h.Dispatch(s.ctx, inv) })
		return
	}
	ev := in.messageEvent(s.user(ctx, evt.RoomID, evt.Sender), s.me)
	s.spawn(func() { s.h.Deliver(s.ctx, ev, resp) })
}

func (s *session) onReaction(ctx context.Context, evt *event.Event) {
	if skip(evt, s.c.UserID) || s.c.Asks().HandleReaction(evt) {
		return
	}
	rel := evt.Content.AsReaction().RelatesTo
	if rel.Type != event.RelAnnotation || rel.EventID == "" || rel.Key == "" {
		return
	}
	thread, _ := s.threads.get(rel.EventID)
	ev := airut.ReactionEvent{
		ID:      string(evt.ID),
		Sender:  s.user(ctx, evt.RoomID, evt.Sender),
		Channel: channel(evt.RoomID),
		Thread:  string(thread),
		Target:  airut.MessageRef{ID: string(rel.EventID)},
		Emoji:   normalizeEmoji(rel.Key),
		Native:  evt,
	}
	resp := s.responder(evt.RoomID, thread, rel.EventID, evt.Sender)
	s.spawn(func() { s.h.Deliver(s.ctx, ev, resp) })
}

func (s *session) onMember(_ context.Context, evt *event.Event) {
	if evt.GetStateKey() != string(s.c.UserID) || evt.Content.AsMember().Membership != event.MembershipInvite {
		return
	}
	room, inviter := evt.RoomID, evt.Sender
	if s.opts.AcceptInvite == nil || !s.opts.AcceptInvite(string(room), string(inviter)) {
		s.log.Info("ignored an invite", "room", string(room))
		return
	}
	s.spawn(func() {
		if _, err := s.c.JoinRoomByID(s.ctx, room); err != nil {
			s.log.Warn("could not join a room it was invited to", "room", string(room), "error_kind", errKind(err))
		}
	})
}

// skip reports events the bot never acts on: its own, and those from
// rooms it has left.
func skip(evt *event.Event, self id.UserID) bool {
	return evt.Sender == self || evt.Mautrix.EventSource&event.SourceLeave != 0
}

// user is a Matrix user as airut names one, with the display name the
// room knows when the state store has it.
func (s *session) user(ctx context.Context, room id.RoomID, u id.UserID) airut.User {
	out := airut.User{Platform: airut.Matrix, ID: string(u)}
	if m, err := s.c.StateStore.TryGetMember(ctx, room, u); err == nil && m != nil {
		out.Name = m.Displayname
	}
	if out.Name == "" {
		out.Name, _, _ = u.Parse()
	}
	return out
}

func channel(room id.RoomID) airut.Channel {
	return airut.Channel{Platform: airut.Matrix, ID: string(room)}
}

// From returns the rihma client and the Matrix event behind c, for what
// the portable surface does not cover. ok is false when c did not come
// from this adapter. The event is nil for an invocation that came from
// an agent turn rather than a message.
func From(c *airut.Ctx) (client *rihma.Client, evt *event.Event, ok bool) {
	r, ok := c.Responder().(*responder)
	if !ok {
		return nil, nil, false
	}
	evt, _ = c.Native().(*event.Event)
	return r.s.c, evt, true
}
