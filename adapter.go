package airut

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
)

// Platform names the chat platform an adapter carries, such as Discord
// or Matrix. Adapters outside this module may name their own.
type Platform string

// The platforms airut ships adapters for.
const (
	Discord Platform = "discord"
	Matrix  Platform = "matrix"
)

// User is a person or bot account on a platform.
type User struct {
	Platform Platform
	ID       string
	Name     string
}

// Channel is a place messages go: a Discord channel or thread, or a
// Matrix room.
type Channel struct {
	Platform Platform
	ID       string
	Name     string
}

// Role is a group of users that a platform can grant permissions to.
type Role struct {
	Platform Platform
	ID       string
	Name     string
}

// Attachment is a file that arrived with an invocation.
type Attachment struct {
	Name        string
	ContentType string
	Size        int64
	URL         string
}

// File is a file a reply carries.
type File struct {
	Name        string
	ContentType string
	Data        []byte
}

// Message is what a reply or an edit sends.
type Message struct {
	Text  string
	Files []File
	// Private asks for a reply only the caller sees: an ephemeral reply
	// on Discord, a direct message on Matrix.
	Private bool
	// ReplyTo makes the message a reply to another one. A Ctx's replies
	// answer the message that triggered it unless this is set.
	ReplyTo MessageRef
	// Key makes sending idempotent. A second message with the same Key,
	// from the same bot, posts nothing and returns the first one's ref,
	// for as long as the platform remembers the key: about a day on
	// Matrix, and a few minutes on Discord, which takes a key only for a
	// message posted to a channel, not for an answer to a command. A bot
	// that records what it sent uses it to resend safely after a restart
	// that fell between a send and its record. The key is the bot's to
	// make unique, such as the id of the event the message answers and a
	// step name.
	Key string
}

// MessageRef names a message an adapter sent or received, so it can be
// edited or reacted to. Its ID means something only to the adapter that
// issued it.
type MessageRef struct {
	ID string
}

// Ask is a question with a fixed set of answers, put to one user.
type Ask struct {
	Question string
	Choices  []string
	// Who may answer. The core sets it to the caller.
	Who User
	// Timeout bounds the wait. The core fills in a default when it is
	// zero.
	Timeout time.Duration
}

// ErrAskTimeout is returned by an Ask nobody answered in time.
var ErrAskTimeout = errors.New("airut: the question was not answered in time")

// Responder carries replies for one invocation back to the platform. An
// adapter implements it, and degrades what its platform lacks: an Ask is
// buttons on Discord and a reaction widget on Matrix. Its methods must be
// safe to call from more than one goroutine, because an Agent may run the
// tools of a turn on goroutines other than the handler's.
type Responder interface {
	Reply(ctx context.Context, m Message) (MessageRef, error)
	Edit(ctx context.Context, ref MessageRef, m Message) error
	React(ctx context.Context, ref MessageRef, emoji string) error
	// Ask blocks until a.Who answers with one of a.Choices, or returns
	// ErrAskTimeout.
	Ask(ctx context.Context, a Ask) (string, error)
	Typing(ctx context.Context) error
	// Thread opens a thread named name from the invocation and returns
	// a Responder that replies inside it, and the thread's id: what a
	// message posted in the thread later carries as its Thread.
	Thread(ctx context.Context, name string) (Responder, string, error)
	// Resolve turns a text argument, such as a mention, into a User,
	// Channel, or Role, according to kind.
	Resolve(ctx context.Context, kind Kind, token string) (any, error)
}

// Invocation is one command call, as an adapter delivers it to Host.Dispatch.
type Invocation struct {
	// ID identifies the invocation in logs. It never holds chat text.
	ID      string
	Command string
	// Values holds structured arguments, keyed by parameter name, from a
	// platform that types them, such as a Discord application command.
	// Each value is a string, bool, int64, float64, User, Channel, Role,
	// or Attachment. When Values is nil, Text is parsed instead.
	Values map[string]any
	// Text is everything after the command name, from a text trigger.
	Text string
	// Files are the attachments that arrived with a text invocation, in
	// order. They bind to the command's attachment parameters in order.
	Files   []Attachment
	Caller  User
	Roles   []Role
	Channel Channel
	// Thread identifies the thread the invocation arrived in, if any.
	Thread string
	// Message is the message that triggered the invocation, if there was
	// one.
	Message MessageRef
	// ReplyTo is the message the triggering message replies to, if it
	// replies to one.
	ReplyTo MessageRef
	Respond Responder
	// Native is the platform's own event, for the escape hatch.
	Native any
}

// Host is what a running Bot gives each adapter.
type Host interface {
	// Specs describes every registered command, for an adapter to
	// register them with its platform.
	Specs() []Spec
	// Prefix is the text trigger, such as "!".
	Prefix() string
	// Dispatch runs one invocation to completion. Adapters call it from
	// a goroutine of their own for each invocation.
	Dispatch(ctx context.Context, inv Invocation)
	// Deliver runs the handlers for one event to completion, with r
	// answering in the event's channel. Adapters call it from a goroutine
	// of their own for each message that is not a command and each
	// reaction, never for the bot's own.
	Deliver(ctx context.Context, ev Event, r Responder)
	// Ready announces the adapter's Conn once it is connected, and again
	// after a reconnect. ctx is the one Run was given. It returns at
	// once; the bot's OnReady handlers run on goroutines of their own.
	Ready(ctx context.Context, c Conn)
	Logger() *slog.Logger
}

// Adapter connects a Bot to one platform.
type Adapter interface {
	Platform() Platform
	// Run serves the platform until ctx ends, and returns nil then.
	Run(ctx context.Context, h Host) error
}

// SplitCommand splits a text trigger into a command name and the rest of
// the line. It reports false when line does not start with prefix
// followed by a command name. Adapters strip a mention of the bot before
// calling it, and pass the prefix as "" when the mention was the trigger.
func SplitCommand(line, prefix string) (name, rest string, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, prefix) {
		return "", "", false
	}
	line = strings.TrimLeft(line[len(prefix):], " \t")
	end := strings.IndexAny(line, " \t\n")
	if end < 0 {
		end = len(line)
	}
	name, rest = line[:end], strings.TrimSpace(line[end:])
	if !validName(name) {
		return "", "", false
	}
	return name, rest, true
}
