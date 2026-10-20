# airut architecture

This is the design airut starts from. Where the code and this document
disagree, the code and its tickets win, and this document is corrected.

## The idea in one paragraph

A bot is a set of typed commands. A command runs as plain code and
replies through a portable surface that both Discord and Matrix carry.
A command handler may call `c.Agent(...)` to run an agent turn, and the
author decides everything that turn can reach: which tools it may call,
what persona frames it, which conversation it remembers, and how much it
may spend. A command can also be registered as a tool, so one typed
definition serves as a slash command, a text command, and an agent tool.

## Layers

```
            ┌────────────────────────────────────────────┐
 bot code   │ commands, handlers, personas, tool choices │
            └──────────────┬─────────────────────────────┘
                           │
            ┌──────────────▼─────────────┐   ┌────────────────────────┐
 airut      │ terva.sh/airut (core)      │──▶│ terva.sh/airut/agent   │
            │ commands, Ctx, routing,    │   │ Turn, tools, gate,     │
            │ portable replies           │   │ sessions, budgets      │
            └───────┬───────────┬────────┘   └───────────┬────────────┘
                    │           │                        │
            ┌───────▼──┐  ┌─────▼────┐            ┌──────▼──────┐
 platforms  │ discord  │  │ matrix   │   engine   │ terva core  │
            │discordgo │  │ rihma    │            │ + provider  │
            └──────────┘  └──────────┘            └─────────────┘
```

The arrows are imports. The core imports nothing below it. Each adapter
imports the core and its own platform library. `agent` imports the core
and terva. An import-graph test enforces this, in the style of rihma's
`TestRootPackageKnowsNothingOfTerva`.

The core reaches the agent through an interface it declares, not through
an import. A bot that wires no agent gets an error from `c.Agent`, and
a bot with no agent commands never links terva.

## Commands

A command is a name, a one-line description, and a handler whose second
parameter is an argument struct. The struct's fields and tags are the
argument schema, and the framework derives three things from them:

- a Discord application command with typed options;
- a text grammar for Matrix, triggered by a configurable prefix such as
  `!` or by a mention of the bot;
- a JSON Schema, when the command is registered with `.AsTool()`, so the
  agent can call it as a tool.

Every platform gets the same definition. The core describes a command as
a platform-neutral `Spec`, whose parameter kinds match Discord's option
types one for one, and enforces Discord's limits when the command is
registered: the name pattern, description lengths, at most 25
parameters, and required parameters before optional ones. A definition
that one platform would refuse therefore fails at startup on all of
them. `Command` panics there, naming the command and the field. The
Discord adapter maps a `Spec` to an application command.

A command called by the agent runs with the same `Ctx` type, marked as an
agent call, so a handler can tell a person's request from the model's.
What it replies is returned to the model as the tool's result and is not
posted. The model decides what the person sees.

## The portable surface and the escape hatch

What both platforms carry is portable: replies, edits, reactions,
attachments, threads, typing, and asks (a question with a fixed set of
answers). Where a platform lacks something, airut degrades it:

| Feature | Discord | Matrix |
|---|---|---|
| Command invocation | application command | prefix or mention |
| Ask | buttons | reaction widget |
| Ephemeral reply | ephemeral | a DM to the caller |

Anything outside the portable surface goes through the escape hatch.
The core cannot name a platform's types, so the platform packages
provide it as functions, `discord.From(c)` and `matrix.From(c)`. Each
returns the native client and event, with a boolean saying whether this
`Ctx` came from that platform. They build on `c.Native()` and
`c.Responder()`.

## Events

Not everything a bot does starts with a command. `bot.OnMessage`
registers a handler for every message that is not a command, with its
text, attachments, and the message it replies to. `bot.OnReaction`
registers one for reactions, with the message they target. Adapters
deliver both through `Host.Deliver`, never for the bot's own messages
or reactions.

A handler gets an ordinary `Ctx`: its caller is the event's sender, and
`Reply`, `React`, `SessionKey`, and `Agent` work as for a command, the
gate included. Handlers run in registration order. One that fails is
logged by kind and never answered in the chat, so a handler speaks only
when it chooses to.

A message id is the platform's own and survives a restart. A bot that
keeps its own record of what it posted, such as reviews awaiting a
moderator, can match a reaction to one of them days later. `Ctx.ReplyTo`
names the message a command or a message replies to, and
`Message.ReplyTo` makes a reply answer a message other than the
trigger.

## The connection

Some work belongs to no one invocation: posting a review to a
moderators' channel, reading an image's bytes, walking a channel's
history, checking whether someone is in a channel, or showing a long
job's progress as a reaction. Each adapter
announces a `Conn` through `Host.Ready` once it is connected:

- `Send` posts to any channel, and `Message.ReplyTo` makes it a reply;
- `Open` reads an attachment through the platform's own authenticated
  download, and the caller bounds what it reads;
- `History` pages back through a channel, newest first, with a cursor
  that survives a restart and an empty one at the start of what the bot
  may read;
- `Member` reports whether a user is joined to a channel;
- `React` adds the bot's reaction to a message and returns a ref for
  the reaction, and `Unreact` retracts one. Together they let a bot swap
  a status reaction as work moves on. `Unreact` refuses anything that
  is not the bot's own reaction, with `ErrNotOwnReaction`, so it cannot
  remove a message or someone else's reaction. Reacting again with the
  same emoji returns the standing reaction's ref, so a bot that
  restarted before recording a ref can recover it.

`bot.OnReady` runs each time an adapter announces its `Conn`, on a
goroutine of its own that `Run` waits for, which is where a bot resumes
long work after a restart.

Resumed work can repeat a send whose record a restart cut off.
`Message.Key` makes a send idempotent: a second message with the same
key, from the same bot, posts nothing and returns the first one's ref.
The bot makes the key unique, typically from the id of the event the
message answers and the step it belongs to. On Matrix each part's
transaction id derives from the key, and the homeserver remembers a
transaction for about a day, from the same device. A file is a part
too, so a keyed message with files posts each file once, but a resend
uploads the file again and leaves that copy unreferenced. On Discord each part's nonce derives from the key, and Discord
enforces a nonce for a few minutes; an answer to a command takes no
key, because Discord delivers each command once. `Ctx.Conn` and
`Bot.Conn` return the Conn from a handler or from anywhere else.

## The Matrix adapter

`matrix.New(cfg.Matrix, matrix.Options{...})` runs on rihma with its bot
sync policy, which discards the backlog on a first connect and never
shows the bot its own messages. The state directory holds the session
and rihma's crypto store; lose it and the bot can no longer read
encrypted history it could read before.

- **Triggers.** A message is a command when it starts with the prefix,
  as in `!roll 2d6`, or with a mention of the bot. A mention is the
  bot's user id, display name, or localpart, followed by a colon, a
  comma, or a space, which is what a client writes for a pill. After a
  mention, `!roll` calls roll, and so does `roll` alone, but only when
  roll is a command. So "Baphomet: what is this?" reaches the message
  handlers as chat, rather than calling a command named what. That
  chat arrives with `Mentioned` set and the mention stripped into
  `Request`, so a bot can answer what was said to it. A pill elsewhere
  in a message, carried in `m.mentions`, sets `Mentioned` too; an
  `@room` alone does not. Notices
  and edits are never commands, and bare text in a direct room is not a
  trigger.
- **Downtime.** rihma resumes a sync where it stopped, so messages sent
  while the bot was down arrive when it returns. A command older than
  `Options.MaxCommandAge`, ten minutes by default, is dropped rather
  than answered late. Every other message and reaction still reaches the
  handlers, so a bot sees what it missed. A message carries `Sent`, the
  homeserver's time for it, so a handler that answers chat can leave an
  old one unanswered.
- **Invites.** The bot joins a room only when `Options.AcceptInvite`
  says yes. Without that function it joins none, so nobody can pull it
  into a room it was not meant for.
- **Replies.**
  - Replies render Markdown, but any HTML in the text is shown as text.
  - Every message carries empty intentional mentions, so a name or
    `@room` in an agent's answer pings nobody.
  - Long text is split so that each event fits Matrix's limit once it
    is serialized and encrypted.
  - A private reply goes to a direct room with the caller. A room
    recorded in the account's `m.direct` is reused only while it is
    encrypted, joinable only by invitation, and holds nobody but the bot
    and the caller. Otherwise a new room is created. An edit of, or a
    reaction to, a message the bot sent goes to the room it went to,
    the private room included.
  - Matrix threads have no names, so `Ctx.Thread` roots a thread at the
    triggering message, or keeps the thread it was sent in, and ignores
    the name. The returned Ctx's `ThreadID` is that root, which is what a
    later message in the thread carries, so a `PerThread` turn started
    from either remembers one session.
- **The Conn.**
  - `Open` downloads through the authenticated media endpoints and
    decrypts an encrypted file. It needs that file's key, which the
    adapter keeps for the most recent encrypted attachments it has seen,
    live or through `History`.
  - `History` decrypts what it can. It leaves out commands, edits,
    redacted messages, and the bot's own, so a page can hold fewer
    messages than asked for.
  - `Member` reads the state store, which knows the members of the rooms
    the bot is in.
  - `React` sends an `m.reaction`, which mautrix never encrypts. The
    homeserver refuses a second identical reaction from the same
    sender, so on that refusal `React` finds the bot's standing one
    through the relations API.
  - `Unreact` fetches the event and redacts it only when it is an
    `m.reaction` the bot sent. The redaction stays on record.
  - The adapter announces the `Conn` once a sync has been fully
    processed. On a first connect that is the second sync, so `OnReady`
    can wait up to the sync timeout of thirty seconds.

The live tests run against a throwaway Synapse in a container, started
by `just synapse`, and `just e2e` runs them. `just ci` builds them but
does not run them.

## The Discord adapter

`discord.New(cfg.Discord, discord.Options{...})` runs on discordgo. It
connects to the gateway with the bot token and asks only for the events
it uses: guilds, messages, and reactions, in guilds and direct messages.

- **Commands.** Commands are application commands, the slash commands a
  person picks from Discord's menu, and their typed options arrive as
  `Values`, users, channels, roles, and attachments included. Each time
  the gateway session starts, the adapter replaces the application's
  commands with the bot's in one bulk overwrite: in each guild the
  configuration names, where changes show at once, or globally. A
  restart therefore neither registers a command twice nor leaves a
  removed one behind. Text commands with a prefix are not carried,
  because reading other people's messages needs a privileged intent.
- **Answering in time.** Discord wants an answer to a command within
  three seconds. The adapter defers every command before its handler
  runs, which shows the person a "thinking" placeholder, so a handler
  or an agent turn may take as long as it needs.
- **Replies.**
  - The first public reply fills the placeholder, later ones follow it
    up, and a private reply is an ephemeral follow-up only the caller
    sees.
  - Discord shows the first follow-up after a deferral in the
    placeholder's place, with the placeholder's visibility, so a
    private reply that comes first would be public. The adapter turns
    the placeholder into "Answered privately." before such a reply. A
    placeholder no public reply filled is deleted when the command
    finishes.
  - Text is split at Discord's 2000 characters. An edit that grows a
    message past the limit, as a streamed answer does, continues it in
    further messages, and one that shrinks it removes them.
  - Discord accepts answers to a command for fifteen minutes. After
    that, public replies go to the channel and private ones to a direct
    message, never to the channel.
  - Every message carries an empty `allowed_mentions`, so nothing in
    a reply pings a person, a role, or `@everyone`.
- **Asks.** An ask is a message with a button for each choice, at most
  25. Only the person asked can answer; anyone else who presses is told
  privately that it is not theirs. Answered or expired, the message
  shows the outcome and loses its buttons, and a press on an ask from
  before a restart closes it.
- **Events.** Messages and reactions other than the bot's own reach the
  handlers, with `Mentioned` set when a message mentions the bot.
  Discord sends the text of other people's messages only to a bot with
  the message content intent, which is privileged:
  `Options.MessageContent` asks for it, once it is enabled for the
  application in Discord's developer portal.
- **The Conn.**
  - `Send` derives each part's nonce from `Message.Key`, which Discord
    enforces for a few minutes.
  - `Open` downloads only from Discord's content servers, whose links
    are signed, and never sends the bot's token.
  - `History` pages back by message id and leaves out the bot's own
    messages and system messages such as a member joining.
  - `Member` checks membership of the channel's guild, or a direct
    message's recipients. It says nothing of whether a member may read
    the channel.
  - `React` returns the message id and the emoji as the reaction's ref,
    because Discord gives a reaction no id of its own. `Unreact` calls
    the endpoint that removes only the bot's own reaction, and refuses a
    ref whose parts are not an id and an emoji before any request.
- **Logs.** discordgo's own logging is off, because its errors can
  quote gateway payloads, which hold message text. The adapter logs
  ids and error kinds.

The tests run discordgo against a fake transport that answers Discord's
REST API and models its rules, such as the first follow-up after a
deferral, and feed gateway events to the adapter directly. No test
reaches Discord.

## Agent turns

`c.Agent(airut.Turn{...})` passes the core's gate (below) and then runs
on `terva.sh/airut/agent`, which builds an agent on terva's engine for
each turn, `core.New(client, model, ...)`, with:

- `core.WithTools`: only the tools the turn names. A command registered
  with `AsTool` is one; its replies go to the model as the tool's result,
  not to the chat. A failed tool shows the model only the text a person
  would see.
- `core.WithGate`: a gate that asks the caller in the chat before each
  call to a tool registered with `RequireApproval`, and tells the model
  the call was refused on a no or when no answer comes in time.
- `core.WithAssembler`: the system prompt of the turn's persona, from
  the personas the bot gives `agent.New`.
- `core.WithTranscriptStore`: the session for the turn's session key,
  resumed before the turn. Turns in one session run one at a time.
- `core.WithCatalog`: the model catalog `agent.Providers` builds from
  the bot's configuration.
- A turn filter that holds the budget: before each model request it
  stops the turn once the turn has made `Budget.Turns` requests or used
  `Budget.Tokens` tokens, and the chat is told why. The answer so far is
  kept, because a stop never cuts a response off mid-stream.

Output streams back to the chat as typing and message edits. The agent
package imports only the terva packages terva marks stable, which a test
checks.

The agent never receives a shell, a file system, the bot's
configuration, or its credentials. Chat text is untrusted input, so every
tool a turn can reach has to be safe to trigger for anyone who can reach
that command.

## Gating

An agent command is a spend vector, and anyone who can reach it can
trigger it. So before a turn reaches the agent, the core checks
`Config.Gate`, on every platform alike:

- **Access:** the caller's user id, or one of their role ids, must be
  listed for their platform, or the caller must be a member of one of
  its `Channels`, as `MemberOf` checks. The lists are read first, so a
  listed caller costs no membership lookup. A membership check that
  fails is logged by kind, and refuses unless another channel admits. A platform with no list allows
  nobody, and the zero `Gate` refuses every turn. In the configuration
  file, Matrix names its channels as `rooms`, by room id.
- **Rate limit:** at most `PerUser` turns per caller and `PerChannel`
  per channel in a sliding `Window`. The counts are held in memory, and a
  refused turn counts against neither.
- **Budget:** each zero field of the turn's `Budget` takes the gate's
  default. The agent stops the turn at the budget it receives.

A refused turn returns a `*Refusal`, and the core answers it in the chat
with the reason and, for a rate limit, how long to wait. The log gets the
invocation id, the platform, and the reason, never the prompt.
`agent.Gate` builds the `Gate` from the bot's configuration file.

The gate decides who may start a turn. A command can narrow who may use
it with `RestrictTo`, which takes a check run against the caller:

- `MemberOf(ch)` allows the members of a channel, as the platform's
  `Conn` reports them. A moderators' room is the typical case.
- `Listed(access)` allows listed user and role ids, as the gate does.
- `AnyOf(...)` allows a caller that any of its checks allows.

The check applies in three places:

1. **A direct invocation.** A refused caller gets "You are not allowed
   to use !name."
2. **Before a turn starts.** The turn's tools are filtered, so the model
   is never offered a tool its caller may not use.
3. **When the model calls the tool.** The check runs again, because what
   it reads, such as a room's members, can change during a turn. The
   model is told that the caller is not allowed.

A check that fails with an error refuses too, but the reply says the
command failed rather than that the caller is not allowed, and the tool
is left out of the turn. A plain `Tool` restricts itself by implementing
`RestrictedTool`. The core wraps such a tool so that each call is
checked again, and the wrapper keeps the tool's need for approval.

## Sessions

A turn's session is the conversation it remembers, named by
`c.SessionKey(scope)`: an id-only key per channel, per thread, or per
user. `agent/sessions` keeps them in one SQLite database per bot, on the
pure-Go `modernc.org/sqlite` driver, which rihma already brings.

- Each session is a log of the writes terva's engine made through
  `core.TranscriptStore`. A resume replays the log into terva's own
  `core.MemoryTranscriptStore`, so terva, not airut, decides how usage
  totals, compactions, excluded images, and unanswered tool calls come
  back. The store passes terva's `transcripttest` suite.
- Message content is stored in airut's own tagged JSON, because terva's
  codec is not a stable API. A test fills every field of every content
  block by reflection, so a terva upgrade that adds a field fails until
  the codec carries it.
- A session idle longer than the configured retention, 30 days by
  default, is deleted with its log, when the store opens and hourly
  after.
- When the bot's agent can forget sessions, the bot gains a built-in
  `forget` command that clears the caller's own per-user session. A bot
  command named `forget` takes its place.

The queries are `.sql` files compiled by sqlc into typed `database/sql`
code, which is committed; `just sqlc-check` fails when it is stale. The
schema is a set of forward-only migrations embedded in the binary.

The database holds conversation text. Protect it and back it up like the
Matrix crypto store; airut never logs what is in it.

## Metrics

Every bot keeps metrics in memory, which `Bot.Metrics()` and
`Ctx.Metrics()` return. The core records these:

| Series | Labels | What it counts |
|---|---|---|
| `airut_commands_total` | `command`, `outcome` | invocations, by how each ended |
| `airut_command_seconds` | `command` | how long a command that ran took |
| `airut_events_total` | `kind`, `outcome` | event handler runs |
| `airut_event_seconds` | `kind` | how long one event's handlers took |
| `airut_agent_turns_total` | `command`, `outcome` | agent turns, refused ones included |
| `airut_agent_turn_seconds` | `command` | how long a turn that started took |
| `airut_agent_requests_total` | `command` | model requests |
| `airut_agent_tokens_total` | `command` | tokens the provider reported |
| `airut_agent_stops_total` | `command`, `stop` | turns by how they stopped: `done`, `budget`, or `error` |
| `airut_adapter_up` | `platform` | 1 while the adapter runs |
| `airut_uptime_seconds` | | seconds since the bot was made |

An outcome is `ok` or the error's kind, as the log names it: `arguments`,
`user`, `handler`, `panic`, `refused_access`, `refused_rate`, and so on. A
command is labelled with its registered name, and an unknown one with
`""` and the outcome `unknown`, because what was typed is chat text. An
agent turn started from an event handler has the command `""`.

A bot adds its own with `Add` for a counter, `Set` for a gauge, and
`Observe` or `Since` for a timing. A timing keeps a count, a sum, and a
maximum. That is enough for a mean and a rate, and costs no buckets.
Label values must be ids and kinds, never message text. Each distinct
value is a series kept for the bot's lifetime, so a label taken from chat
would let anyone grow memory without bound and would put the chat in the
scrape. A malformed name or a name reused as another kind panics, since
it is a mistake in the bot's code.

Two settings report them. `Config.MetricsAddr` serves `/metrics` in the
Prometheus text format, with a timing as a summary's `_count` and `_sum`
and a `_max` gauge beside it. `Run` fails before it starts an adapter if
it cannot listen. `Config.SummaryEvery` logs one `metrics summary` line
of what changed since the last one. Counters and timings appear there as
deltas. A bot without a scraper can read its rates from the log.

The endpoint has no authentication. Bind it to an address only the
scraper reaches.

## Configuration

The engine reads no file and no environment variable, so the framework
supplies everything. Each bot has one TOML file, which
[config.example.toml](config.example.toml) shows key by key. It holds:

- the model providers: OpenAI-compatible endpoints and their models;
- the agent's default model, budget, rate limit, and access lists;
- the session store;
- each platform's settings;
- where metrics are served, and how often they are logged.

Secrets are never in the file. A key that holds one names where the
deployment put it: an environment variable, a systemd credential, or a
file. `config.Load` reads them, and returns every problem in the file
at once, each named by its key and never quoting a value.

A bot's own settings go in the same file, under an `[app]` table, and
`config.LoadApp(path, &settings)` decodes them into the bot's struct by
toml tags. A field of type `config.Secret` takes a secret reference,
exactly as airut's own secrets do. Unknown keys and wrong types in
`[app]` are reported with the rest. `config.Load` refuses an `[app]`
table, because a program that calls it has no settings to receive one.

```go
type settings struct {
	ModRoom string `toml:"mod_room"`
	Booru   struct {
		URL   string        `toml:"url"`
		Token config.Secret `toml:"token"` // token = { credential = "booru-token" }
	} `toml:"booru"`
}

var s settings
cfg, err := config.LoadApp("/etc/bot/airut.toml", &s)
```

`airut/config` only parses: it imports neither terva nor a platform
library. `agent.Providers` turns its providers into terva clients, one
per provider id, and a model catalog holding every model the file lists.

## Building

rihma builds only with `CGO_ENABLED=0 -tags goolm`, and airut inherits
that for the whole module.
