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
	// When it and CheckInvite are both nil, the bot joins none, so nobody
	// can pull it into a room it was not meant for.
	AcceptInvite func(Invite) bool
	// CheckInvite decides as AcceptInvite does, for a decision that needs
	// a context or can fail, such as asking airut.Bot.IsOwner whether the
	// inviter owns the bot. An error refuses the invite, so a check that
	// could not finish never lets the bot in. ctx ends when Run does. It
	// runs off the sync loop, one call for each invite, and must be safe
	// for concurrent use.
	//
	// Set one of AcceptInvite and CheckInvite: Run refuses to start with
	// both, rather than ignore either.
	//
	// The Bot is usually built before its adapters, so CheckInvite can
	// capture it:
	//
	//	b := airut.New(cfg)
	//	m := matrix.New(&mcfg, matrix.Options{CheckInvite: func(ctx context.Context, inv matrix.Invite) (bool, error) {
	//		return b.IsOwner(ctx, airut.User{Platform: airut.Matrix, ID: inv.Inviter})
	//	}})
	//	err := b.Run(ctx, m)
	CheckInvite func(ctx context.Context, inv Invite) (bool, error)
	// MaxCommandAge is how old a command may be and still be answered.
	// Zero means DefaultMaxCommandAge.
	MaxCommandAge time.Duration
	// ReplyDepth is how far up a reply chain the adapter resolves for a
	// command or a message that addresses the bot. Zero means
	// airut.DefaultReplyDepth, and a negative value resolves none.
	ReplyDepth int
	// Logger receives the adapter's logs. Nil means the Host's.
	Logger *slog.Logger
}

// Invite is an invitation to a room, for Options.AcceptInvite and
// Options.CheckInvite.
type Invite struct {
	Room, Inviter string
	// Direct reports that the invite says it opens a direct chat, as a
	// client marks one. It is the inviter's claim only. Once the bot has
	// joined, an event's Direct reports whether the room is in fact
	// private to the bot and the inviter. The bot records a direct room
	// it joins in its m.direct, as clients do.
	Direct bool
}

// errBothInviteChecks is Run's error for Options that set both invite
// callbacks.
var errBothInviteChecks = errors.New("matrix: Options sets both AcceptInvite and CheckInvite; set one")

// Adapter is an airut.Adapter for Matrix.
type Adapter struct {
	cfg  config.Matrix
	opts Options

	mu     sync.Mutex
	client *rihma.Client // the running Run's, or nil
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
	if a.opts.AcceptInvite != nil && a.opts.CheckInvite != nil {
		return errBothInviteChecks
	}
	log := a.opts.Logger
	if log == nil {
		log = h.Logger()
	}
	log = log.With("platform", string(airut.Matrix))
	var sib *siblings
	var onIdentity func(rihma.IdentityChange)
	if a.cfg.Siblings != nil {
		sib = newSiblings(a.cfg.Siblings, id.UserID(a.cfg.UserID), a.cfg.StateDir, log)
		onIdentity = sib.identityChanged
	}
	c, err := open(ctx, &a.cfg, log, onIdentity)
	if err != nil {
		return err
	}
	checkCrossSigning(ctx, c, a.cfg.RecoveryKey, log)
	a.mu.Lock()
	a.client = c
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.client = nil
		a.mu.Unlock()
	}()
	s := newSession(ctx, c, h, log, a.opts)
	if sib != nil {
		sib.ids, sib.rm = c, coordRoom{s: s, room: sib.room}
		s.sib = sib
	}
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

// open opens the account cfg describes, from its state directory, or by
// logging in when none is stored yet.
func open(ctx context.Context, cfg *config.Matrix, log *slog.Logger, onIdentity func(rihma.IdentityChange)) (*rihma.Client, error) {
	user := id.UserID(cfg.UserID)
	local, _, err := user.Parse()
	if err != nil || cfg.Homeserver == "" || cfg.StateDir == "" {
		return nil, errors.New("matrix: the configuration needs a homeserver, a user id, and a state directory")
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("matrix: %w", err)
	}
	ro := rihma.Options{
		Homeserver: cfg.Homeserver,
		StateDir:   filepath.Join(cfg.StateDir, "rihma"),
		Sessions:   rihma.FileSessionStore{Path: filepath.Join(cfg.StateDir, "session.json")},
		DeviceName: cfg.DeviceName,
		Logger:     zerologTo(log),

		OnIdentityChange: onIdentity,
	}
	if !cfg.Password.IsZero() {
		ro.Login = &mautrix.ReqLogin{
			Type:       mautrix.AuthTypePassword,
			Identifier: mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: local},
			Password:   cfg.Password.Value(),
		}
	}
	c, err := rihma.Open(ctx, ro)
	if err != nil {
		return nil, fmt.Errorf("matrix: opening the client: %w", err)
	}
	if c.UserID != user {
		c.Close()
		return nil, fmt.Errorf("matrix: the stored session is for another account than %s", user)
	}
	return c, nil
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
	// replyDepth is how far up a reply chain the adapter resolves.
	replyDepth int
	// bundles are the bundles waiting for replies.
	bundles bundles
	// sib runs sibling cross-signing, or is nil without [matrix.siblings].
	sib *siblings

	mu      sync.Mutex
	stopped bool
	ready   bool
	wg      sync.WaitGroup

	dmMu sync.Mutex
	dms  map[id.UserID]id.RoomID

	// listed holds the rooms the account's m.direct names, read when a
	// private-looking room is first checked and again once the copy is
	// directReload old, so a room another client adds or removes is seen
	// within that time.
	listedMu   sync.Mutex
	listed     map[id.RoomID]bool
	listedLoad time.Time
}

// directReload is how old the cached m.direct may be before a check of a
// private-looking room reads it again. A test shortens it.
var directReload = time.Minute

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
	s.replyDepth = airut.DefaultReplyDepth
	if o.ReplyDepth != 0 {
		s.replyDepth = max(o.ReplyDepth, 0)
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
		if s.sib != nil {
			s.spawn(func() { s.sib.start(s.ctx) })
		}
		s.h.Ready(s.ctx, s)
	}
	return true
}

func (s *session) onMessage(ctx context.Context, evt *event.Event) {
	if skip(evt, s.c.UserID) {
		return
	}
	if s.sib != nil && evt.RoomID == s.sib.room {
		// The coordination room carries proofs and nothing else: no
		// message there is a command or reaches the bot's handlers.
		raw := evt.Content.VeryRaw
		s.spawn(func() { s.sib.handle(s.ctx, evt.Sender, raw) })
		return
	}
	in, ok := s.parse(evt)
	if !ok || in.reply != "" && s.answerBundle(ctx, evt, in) {
		return
	}
	resp := s.responder(evt.RoomID, in.thread, evt.ID, evt.Sender)
	direct := s.isDirect(ctx, evt.RoomID, evt.Sender)
	if name, rest, ok := in.command(s.h.Prefix(), s.me, s.known, direct); ok {
		if age := time.Since(time.UnixMilli(evt.Timestamp)); age > s.opts.MaxCommandAge {
			s.log.Info("dropped a stale command", "event", string(evt.ID), "age", age.Round(time.Second).String())
			return
		}
		inv := in.invocation(s.user(ctx, evt.RoomID, evt.Sender), name, rest)
		inv.Direct, inv.Respond = direct, resp
		s.spawn(func() {
			inv.ReplyChain = s.chain(s.ctx, evt.RoomID, in)
			s.h.Dispatch(s.ctx, inv)
		})
		return
	}
	ev := in.messageEvent(s.user(ctx, evt.RoomID, evt.Sender), s.me, direct)
	s.spawn(func() {
		// Off the sync loop: each link is a request.
		if ev.Mentioned {
			ev.ReplyChain = s.chain(s.ctx, evt.RoomID, in)
		}
		s.h.Deliver(s.ctx, ev, resp)
	})
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
		Direct:  s.isDirect(ctx, evt.RoomID, evt.Sender),
		Target:  airut.MessageRef{ID: string(rel.EventID)},
		Emoji:   rel.Key, // the core normalizes it
		Native:  evt,
	}
	resp := s.responder(evt.RoomID, thread, rel.EventID, evt.Sender)
	s.spawn(func() { s.h.Deliver(s.ctx, ev, resp) })
}

func (s *session) onMember(_ context.Context, evt *event.Event) {
	if evt.GetStateKey() != string(s.c.UserID) {
		return
	}
	if s.sib != nil && evt.RoomID == s.sib.room && evt.Content.AsMember().Membership == event.MembershipJoin {
		s.spawn(func() { s.sib.joined(s.ctx) })
		return
	}
	if evt.Content.AsMember().Membership != event.MembershipInvite {
		return
	}
	room, inviter := evt.RoomID, evt.Sender
	inv := Invite{Room: string(room), Inviter: string(inviter), Direct: evt.Content.AsMember().IsDirect}
	// AcceptInvite decides on the sync loop, as it always has; CheckInvite
	// may wait on I/O, so it decides off it.
	if s.opts.CheckInvite == nil && !s.admit(s.ctx, inv) {
		return
	}
	s.spawn(func() {
		if s.opts.CheckInvite != nil && !s.admit(s.ctx, inv) {
			return
		}
		if _, err := s.c.JoinRoomByID(s.ctx, room); err != nil {
			s.log.Warn("could not join a room it was invited to", "room", string(room), "error_kind", errKind(err))
			return
		}
		if inv.Direct {
			s.dmMu.Lock()
			defer s.dmMu.Unlock()
			if err := s.recordDirect(s.ctx, inviter, room); err != nil {
				s.log.Warn("could not record a direct room", "room", string(room), "error_kind", errKind(err))
			}
		}
	})
}

// admit reports whether the bot joins the room inv invites it to, by
// Options.CheckInvite or Options.AcceptInvite, and logs the decision by
// the room's and inviter's ids. With neither set it refuses, and an error
// from CheckInvite refuses too.
func (s *session) admit(ctx context.Context, inv Invite) bool {
	log := s.log.With("room", inv.Room, "inviter", inv.Inviter, "direct", inv.Direct)
	ok := false
	switch {
	case s.opts.CheckInvite != nil:
		var err error
		if ok, err = s.opts.CheckInvite(ctx, inv); err != nil {
			if ctx.Err() == nil {
				log.Warn("refused an invite its check could not decide", "error_kind", errKind(err))
			}
			return false
		}
	case s.opts.AcceptInvite != nil:
		ok = s.opts.AcceptInvite(inv)
	}
	if !ok {
		log.Info("ignored an invite")
		return false
	}
	log.Info("accepted an invite")
	return true
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
