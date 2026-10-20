// Package discord carries an airut Bot to Discord, on discordgo.
//
// A bot's commands are Discord application commands, the slash commands
// a person picks from Discord's menu, with each parameter a typed option.
// The adapter registers them each time it connects, replacing whatever
// the application had registered before: in each guild the configuration
// names, which suits development, or globally.
//
// The adapter acknowledges every command at once, so a handler or an
// agent turn may take as long as it needs; the person sees Discord's
// "thinking" placeholder until the first public reply fills it. Later
// replies follow it up, and private ones are ephemeral; a private reply
// that comes first turns the placeholder into a note that the command
// answered privately, because Discord would otherwise show that reply
// in the placeholder's place, to everyone. Text past Discord's 2000
// characters continues in further messages, including when an edit
// grows it. Discord accepts answers to a command for fifteen minutes;
// after that, public replies go to the channel and private ones to a
// direct message. No message the bot sends pings anyone, whatever its
// text says.
//
// An Ask is a message with a button for each choice. Only the person
// asked can answer; anyone else who presses is told so privately. Once
// answered or expired, the message shows the outcome without its
// buttons, and a press on a question from before a restart closes it.
//
// Messages and reactions other than the bot's own reach its OnMessage
// and OnReaction handlers. Discord sends the text of other people's
// messages only to a bot with its privileged message-content intent,
// which Options.MessageContent asks for. The adapter's Conn posts with
// Discord's nonce when a message has a Key, downloads attachments only
// from Discord's content servers, pages history by message id, and
// checks membership of a channel's guild.
package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"terva.sh/airut"
	"terva.sh/airut/config"
)

// Options are the adapter's settings that the configuration file does
// not hold.
type Options struct {
	// MessageContent asks Discord for the text of every message the bot
	// can see, which an OnMessage handler needs to read chat. Discord
	// calls it a privileged intent: enable it for the application in
	// Discord's developer portal first, or Discord refuses the
	// connection. Without it, a MessageEvent's Text is empty except in
	// direct messages and in messages that mention the bot.
	MessageContent bool
	// Logger receives the adapter's logs. Nil means the Host's.
	Logger *slog.Logger
}

// Adapter is an airut.Adapter for Discord.
type Adapter struct {
	cfg  config.Discord
	opts Options
}

// New returns an adapter for the bot cfg describes.
func New(cfg *config.Discord, o Options) *Adapter {
	a := &Adapter{opts: o}
	if cfg != nil {
		a.cfg = *cfg
	}
	return a
}

// Platform reports airut.Discord.
func (a *Adapter) Platform() airut.Platform { return airut.Discord }

// intents are the gateway events the adapter asks for: messages and
// reactions in guilds and direct messages, and the guild events that
// keep discordgo's cache of channels and roles, which names them.
// Interactions arrive whatever the intents.
func (o Options) intents() discordgo.Intent {
	i := discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsGuildMessageReactions |
		discordgo.IntentsDirectMessages | discordgo.IntentsDirectMessageReactions
	if o.MessageContent {
		i |= discordgo.IntentsMessageContent
	}
	return i
}

// Run connects to Discord's gateway, registers the bot's commands, and
// serves them until ctx ends. It returns an error if the commands cannot
// be registered, because a bot whose commands Discord does not know can
// never be called.
func (a *Adapter) Run(ctx context.Context, h airut.Host) error {
	if a.cfg.Token.IsZero() {
		return errors.New("discord: the configuration needs a bot token")
	}
	log := a.opts.Logger
	if log == nil {
		log = h.Logger()
	}
	log = log.With("platform", string(airut.Discord))
	dg, err := discordgo.New("Bot " + a.cfg.Token.Value())
	if err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	dg.Identify.Intents = a.opts.intents()
	dg.LogLevel = -1 // discordgo's own log lines can carry message text
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	s := newSession(ctx, dg, h, log, a.cfg.Guilds, func(err error) { cancel(err) })
	s.register()
	if err := dg.Open(); err != nil {
		return fmt.Errorf("discord: connecting to the gateway: %w", errKindOnly(err))
	}
	<-ctx.Done()
	dg.Close()
	s.stop()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// session is one Run's connection.
type session struct {
	ctx    context.Context
	dg     *discordgo.Session
	h      airut.Host
	log    *slog.Logger
	guilds []string
	fail   func(error)
	// now is the clock, which tests replace.
	now func() time.Time

	asks asks

	mu sync.Mutex
	// me is the bot's own user id.
	me      string
	stopped bool
	wg      sync.WaitGroup
}

func newSession(ctx context.Context, dg *discordgo.Session, h airut.Host, log *slog.Logger, guilds []string, fail func(error)) *session {
	return &session{ctx: ctx, dg: dg, h: h, log: log, guilds: guilds, fail: fail, now: time.Now}
}

func (s *session) register() {
	s.dg.AddHandler(s.onReady)
	s.dg.AddHandler(s.onResumed)
	s.dg.AddHandler(s.onInteraction)
	s.dg.AddHandler(s.onMessage)
	s.dg.AddHandler(s.onReaction)
}

// spawn runs f on a goroutine Run waits for. discordgo calls handlers on
// goroutines of its own, which may still arrive after Run stopped, so a
// spawn after stop is dropped.
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

// onReady registers the commands each time the gateway session starts,
// and announces the session as the bot's Conn. Registering is a
// replacement, so doing it again after a reconnect changes nothing.
func (s *session) onReady(_ *discordgo.Session, r *discordgo.Ready) {
	appID := ""
	if r.User != nil {
		s.mu.Lock()
		s.me = r.User.ID
		s.mu.Unlock()
		appID = r.User.ID
	}
	if r.Application != nil && r.Application.ID != "" {
		appID = r.Application.ID
	}
	s.h.Ready(s.ctx, s)
	s.spawn(func() {
		cmds, err := commands(s.h.Specs())
		if err == nil {
			err = register(s.ctx, s.dg, appID, s.guilds, cmds)
		}
		if err != nil {
			s.log.Error("could not register the commands", "error_kind", errKind(err))
			s.fail(err)
			return
		}
		s.log.Info("registered the commands", "commands", len(cmds), "guilds", len(s.guilds))
	})
}

// onResumed announces the Conn again after the gateway resumed a session
// it had lost, as after any reconnect.
func (s *session) onResumed(*discordgo.Session, *discordgo.Resumed) { s.h.Ready(s.ctx, s) }

// onInteraction answers an application command. It defers the response
// before anything else, so the handler, however slow, never misses the
// three seconds Discord allows for the first answer.
func (s *session) onInteraction(_ *discordgo.Session, ic *discordgo.InteractionCreate) {
	if ic.Interaction == nil {
		return
	}
	if ic.Type == discordgo.InteractionMessageComponent {
		s.spawn(func() { s.onComponent(ic) })
		return
	}
	if ic.Type != discordgo.InteractionApplicationCommand {
		return
	}
	s.spawn(func() {
		r := s.interactionResponder(ic.Interaction)
		if err := r.deferResponse(s.ctx); err != nil {
			s.log.Warn("could not acknowledge a command", "interaction", ic.ID, "error_kind", errKind(err))
			return
		}
		inv := s.invocation(ic, r)
		s.h.Dispatch(s.ctx, inv)
		r.finish(s.ctx)
	})
}

// invocation describes an application command interaction to the core.
func (s *session) invocation(ic *discordgo.InteractionCreate, r airut.Responder) airut.Invocation {
	data := ic.ApplicationCommandData()
	inv := airut.Invocation{
		ID:      ic.ID,
		Command: data.Name,
		Values:  map[string]any{},
		Channel: s.channel(ic.ChannelID),
		Respond: r,
		Native:  ic,
	}
	if ch, err := s.dg.State.Channel(ic.ChannelID); err == nil && ch.IsThread() {
		inv.Thread = ch.ID
	}
	switch {
	case ic.Member != nil && ic.Member.User != nil:
		inv.Caller = user(ic.Member.User, ic.Member)
		for _, id := range ic.Member.Roles {
			inv.Roles = append(inv.Roles, s.role(ic.GuildID, id))
		}
	case ic.User != nil:
		inv.Caller = user(ic.User, nil)
	}
	res := data.Resolved
	for _, o := range data.Options {
		if v, ok := value(o, res); ok {
			inv.Values[o.Name] = v
		}
	}
	return inv
}

// value reads one option as the type the core expects of its kind, with
// users, channels, roles, and attachments taken from the interaction's
// resolved data.
func value(o *discordgo.ApplicationCommandInteractionDataOption, res *discordgo.ApplicationCommandInteractionDataResolved) (any, bool) {
	id, _ := o.Value.(string)
	switch o.Type {
	case discordgo.ApplicationCommandOptionString:
		return o.StringValue(), true
	case discordgo.ApplicationCommandOptionInteger:
		// Discord bounds an integer to ±2^53, which a float64 holds
		// exactly.
		return o.IntValue(), true
	case discordgo.ApplicationCommandOptionNumber:
		return o.FloatValue(), true
	case discordgo.ApplicationCommandOptionBoolean:
		return o.BoolValue(), true
	case discordgo.ApplicationCommandOptionUser:
		u := airut.User{Platform: airut.Discord, ID: id}
		if res != nil && res.Users[id] != nil {
			u = user(res.Users[id], res.Members[id])
		}
		return u, true
	case discordgo.ApplicationCommandOptionChannel:
		c := airut.Channel{Platform: airut.Discord, ID: id}
		if res != nil && res.Channels[id] != nil {
			c.Name = res.Channels[id].Name
		}
		return c, true
	case discordgo.ApplicationCommandOptionRole:
		r := airut.Role{Platform: airut.Discord, ID: id}
		if res != nil && res.Roles[id] != nil {
			r.Name = res.Roles[id].Name
		}
		return r, true
	case discordgo.ApplicationCommandOptionAttachment:
		if res == nil || res.Attachments[id] == nil {
			return nil, false
		}
		a := res.Attachments[id]
		return airut.Attachment{Name: a.Filename, ContentType: a.ContentType, Size: int64(a.Size), URL: a.URL}, true
	}
	return nil, false
}

// user describes a Discord user, by the name they show in the guild when
// m is their membership there.
func user(u *discordgo.User, m *discordgo.Member) airut.User {
	name := u.GlobalName
	if name == "" {
		name = u.Username
	}
	if m != nil && m.Nick != "" {
		name = m.Nick
	}
	return airut.User{Platform: airut.Discord, ID: u.ID, Name: name}
}

// channel describes a channel by id, named from discordgo's cache when it
// is there.
func (s *session) channel(id string) airut.Channel {
	c := airut.Channel{Platform: airut.Discord, ID: id}
	if ch, err := s.dg.State.Channel(id); err == nil {
		c.Name = ch.Name
	}
	return c
}

// role describes a role by id, named from discordgo's cache when it is
// there.
func (s *session) role(guild, id string) airut.Role {
	r := airut.Role{Platform: airut.Discord, ID: id}
	if ro, err := s.dg.State.Role(guild, id); err == nil {
		r.Name = ro.Name
	}
	return r
}

// From returns the discordgo session and the interaction behind c, the
// escape hatch to what airut's portable surface does not carry. ok is
// false when c did not come from this package's adapter; ic is nil when
// c came from it but not from an interaction.
func From(c *airut.Ctx) (dg *discordgo.Session, ic *discordgo.InteractionCreate, ok bool) {
	r, ok := c.Responder().(*responder)
	if !ok {
		return nil, nil, false
	}
	ic, _ = c.Native().(*discordgo.InteractionCreate)
	return r.s.dg, ic, true
}

// errKind names what kind of error err is, for a log line: Discord's
// error code when it sent one. It never carries the error's text, which
// can quote a message.
func errKind(err error) string {
	var re *discordgo.RESTError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &re) && re.Message != nil && re.Message.Code != 0:
		return "discord_" + strconv.Itoa(re.Message.Code)
	case errors.As(err, &re) && re.Response != nil:
		return "http_" + strconv.Itoa(re.Response.StatusCode)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	}
	return "other"
}

// errKindOnly replaces err with its kind, for an error Run returns, which
// the bot may log.
func errKindOnly(err error) error {
	return errors.New(errKind(err))
}
