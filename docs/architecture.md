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

In the text grammar, words bind to parameters in order, and any
parameter can be set by name, as `name=value` or as `--name` for a
boolean. A parameter tagged `named` binds only by name, so optional
switches can come before a final rest parameter, as in
`remember --channel TEXT`.

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
| Bundle of questions | a menu per question, a form for typed answers | answers by number in a reply |
| Ephemeral reply | ephemeral | a DM to the caller |

`Ctx.AskAll` asks up to four questions in one message. Each question is
answered on its own, and when every question is yes or no, all can be
answered at once.

- **Who may answer.** By default only the caller. `Bundle.Who` may
  admit others, such as anyone with a moderator role. Widening it is
  the app's decision, made in code.
- **Answers and time.** The message shows each answer as it lands.
  `AskAll` returns each answer with who gave it. When the time runs
  out, it returns the answers that arrived with `ErrAskTimeout`, and an
  unanswered question has no key, so it is never read as yes.
- **Discord.** One menu per choice question. An "Answer" button opens a
  form for the free-text questions, and "All yes" and "All no" buttons
  appear when every question is yes or no.
- **Matrix.** People answer by replying to the question message, as
  in "1 yes, 2 the troll", or with "all yes". A reply to a bundle that
  answers nothing, or comes from someone `Who` refuses, is ordinary
  chat.
- **Other adapters.** An adapter without `BundleAsker` asks a choice-only
  bundle one question at a time.

`AskTool` is a built-in tool that lets the model use this.

- A call asks at most four questions, and a turn may make two calls.
  Both caps can be set.
- Every answer comes back in one result.
- A typed answer, and the name of whoever gave it, reaches the model as
  a quoted string, framed as chat to read and not to follow.
- It is not an `ApprovalTool`. A tool that needs approval still asks
  for it itself, whatever the model asked first.

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

A message that addresses the bot also arrives with its reply chain:
the message it replies to, then the one that replies to, nearest
first, as `MessageEvent.ReplyChain` and `Ctx.ReplyChain`. So "summarize
this", sent as a reply, can find what "this" is. Each link is a
`Quoted`, with its sender, its text as it reads now, and `FromBot` set
on the bot's own messages, so a streamed answer appears in its final
form. The adapter follows the chain to the depth `bot.reply_depth`
sets, three by default, cuts each link's text at `MaxQuote` and the
chain's at `MaxChain`, and stops at a message it cannot read or
decrypt. A link is chat like any other: a bot quotes it to a model and
never follows it as an instruction. Any other message carries no
chain, and `Conn.Fetch` reads what it replies to.

A direct channel is a conversation private to the bot and one person: a
Discord DM, or a Matrix room the account's `m.direct` names that is
encrypted, joinable only by invitation, and holds nobody else. An event
or invocation from one carries `Direct`, and `Ctx.Direct` reports it.
The adapter checks for each event, so the first message after a third
person joins arrives without it. Everything said in a direct channel
addresses the bot: a message there arrives with `Mentioned` set and its
text as `Request`. `Conn.Direct` checks a channel as it is now, and
names the person it is private to, so a bot can check again just before
it sends something meant for that person alone.

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
  may read. It leaves out the bot's own messages unless the call passes
  `IncludeBot`, which returns them marked `FromBot`;
- `Fetch` reads one message of a channel by its ref, as it reads now,
  the bot's own included;
- `Member` reports whether a user is joined to a channel;
- `React` adds the bot's reaction to a message and returns a ref for
  the reaction, and `Unreact` retracts one. Together they let a bot swap
  a status reaction as work moves on. `Unreact` refuses anything that
  is not the bot's own reaction, with `ErrNotOwnReaction`, so it cannot
  remove a message or someone else's reaction. Reacting again with the
  same emoji returns the standing reaction's ref, so a bot that
  restarted before recording a ref can recover it;
- `Profile` reads the bot's own name, description, and picture, and
  `SetProfile` changes the ones a `ProfileChange` sets. Each adapter
  first checks the change against the bounds every platform accepts
  (`ProfileChange.Check`): a name of 1 to 32 characters, a description
  of at most 400, and a PNG, JPEG, GIF, or WebP picture of at most
  8 MiB whose bytes match the type it claims. A field the platform
  lacks fails with `ErrUnsupported` before anything changes.

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
  `@room` alone does not. In a direct room every line reads as if it
  opened with a mention, so `roll 2d6` calls roll there, and chat
  arrives addressed to the bot. Notices and edits are never commands.
- **Downtime.** rihma resumes a sync where it stopped, so messages sent
  while the bot was down arrive when it returns. A command older than
  `Options.MaxCommandAge`, ten minutes by default, is dropped rather
  than answered late. Every other message and reaction still reaches the
  handlers, so a bot sees what it missed. A message carries `Sent`, the
  homeserver's time for it, so a handler that answers chat can leave an
  old one unanswered.
- **Invites.** The bot joins a room only when `Options.AcceptInvite`
  says yes. Without that function it joins none, so nobody can pull it
  into a room it was not meant for. The `Invite` it is given says
  whether the inviter marked the invite direct, so a bot can accept a
  direct chat from someone without accepting their group rooms. That
  mark is only the inviter's claim: the bot records a direct room it
  joins in its `m.direct`, and the per-event check decides whether the
  room is in fact direct.
- **Cross-signing.** Clients mark a message from an unsigned device
  "not verified by its owner", and a client that shares room keys only
  with verified devices stops sharing them with the bot. So the bot's
  device should be signed by the account's cross-signing identity.
  - `matrix.CrossSign` is the one-time setup. An operator runs it with
    the bot stopped, against the bot's own configuration, so it signs
    the device the bot uses. For an account with no identity it makes
    one, signs the device, and writes the recovery key once to the
    operator's terminal. For an account that has one, it never replaces
    it, because everyone who trusts the bot would stop. It asks for the
    recovery key and signs the device with it. `cmd/airut-example`
    runs it with `-cross-sign`, and reads a pasted key without echo.
    Running it beside the bot would have two processes write one
    device's crypto store.
  - With `[matrix] recovery_key` set, the adapter checks at start and
    signs a device that is not signed, as after the state directory was
    re-created. It logs only whether the device is signed, never the
    key, and a failure does not stop the bot.
  - `(*matrix.Adapter).CrossSigning` reports, from the server, whether
    the account has an identity and the running device is signed, for
    a status command.
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
    redacted messages, and, without `IncludeBot`, the bot's own, so a
    page can hold fewer messages than asked for. A message of the bot's
    reads by its newest edit, so a streamed answer reads as it ended.
  - `Fetch` reads and decrypts one event, with the newest edit its
    sender made applied. An edit by anyone else does not count, as
    clients ignore it too. A reply chain is read the same way, one
    event at a time.
  - `Member` reads the state store, which knows the members of the rooms
    the bot is in.
  - `React` sends an `m.reaction`, which mautrix never encrypts. The
    homeserver refuses a second identical reaction from the same
    sender, so on that refusal `React` finds the bot's standing one
    through the relations API.
  - `Unreact` fetches the event and redacts it only when it is an
    `m.reaction` the bot sent. The redaction stays on record.
  - `SetProfile` sets the global display name and avatar, which the
    homeserver carries into every room the bot is in. A picture is
    uploaded as plain media, because a profile is public. Matrix has no
    description, so a change that sets one fails with `ErrUnsupported`.
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
  handlers, with `Mentioned` set when a message mentions the bot or
  arrives in a DM. A one-to-one DM is a direct channel. A bot cannot be
  added to a group DM, and one the state knows is not direct.
  Discord sends the text of other people's messages only to a bot with
  the message content intent, which is privileged:
  `Options.MessageContent` asks for it, once it is enabled for the
  application in Discord's developer portal.
- **The Conn.**
  - `Send` derives each part's nonce from `Message.Key`, which Discord
    enforces for a few minutes.
  - `Open` downloads only from Discord's content servers, whose links
    are signed, and never sends the bot's token.
  - `History` pages back by message id and leaves out system messages
    such as a member joining and, without `IncludeBot`, the bot's own.
  - `Fetch` reads one message by its id. A reply chain's first link
    comes with the gateway's event, and each one past it is fetched, so
    only messages that address the bot pay for those requests. A slash
    command replies to nothing, so it carries no chain.
  - `Member` checks membership of the channel's guild, or a direct
    message's recipients. It says nothing of whether a member may read
    the channel.
  - `React` returns the message id and the emoji as the reaction's ref,
    because Discord gives a reaction no id of its own. `Unreact` calls
    the endpoint that removes only the bot's own reaction, and refuses a
    ref whose parts are not an id and an emoji before any request.
  - `SetProfile` changes the bot user's name and avatar, and the
    description of its application, which Discord shows on the bot's
    profile as "About me". Discord allows two name changes an hour, so
    a name or description the bot already has is not sent again. A
    picture is always sent, since Discord's avatar hash cannot be
    checked against the bytes.
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
- `core.WithAssembler`: the system prompt, which is the bot's character
  (`Options.Character`, or `agent.WithCharacter` for `agent.Open`)
  followed by the prompt of the turn's persona, from the personas the
  bot gives `agent.New`. The character is the bot's voice in every turn,
  and the persona's rules and tools come last. Then the chat the turn is
  shown, described below.
- `core.WithTranscriptStore`: the session for the turn's session key,
  resumed before the turn. Turns in one session run one at a time.
- `core.WithCatalog`: the model catalog `agent.Providers` builds from
  the bot's configuration.
- A turn filter that holds the budget: before each model request it
  stops the turn once the turn has made `Budget.Turns` requests or used
  `Budget.Tokens` tokens, and the chat is told why. The answer so far is
  kept, because a stop never cuts a response off mid-stream.

Output reaches the chat in one of three ways:

- `StreamTo` posts one reply and edits it as the answer grows, at most
  once per `Config.EditInterval`.
- `Text` returns the answer for the app to post.
- `RunTo` hands the app each piece and each tool call as they happen.

With `FinalOnly`, `Text` returns only the text after the last tool call,
and `StreamTo` replaces its reply with each new segment. The model's
narration between calls then never stays in the chat. `StreamTo` sends
what its throttle held back when a tool is called, and before any `Ask`
the turn makes, an approval included. A question therefore always sits
below a complete reply.

A running turn shows a typing notice in the chat it answers in.

The agent times each turn, so an operator can see where its time went,
in the model or in the app's own tools. Each line carries ids, counts,
and kinds, never text:

- **Each model request** logs "agent request" at Info: the step, the
  provider and model, `ms`, `first_token_ms` to its first text or
  tool-call delta, its input and output tokens, and its stop. A retried
  step logs each attempt, timed from the last one's end, so a retry's
  time holds its backoff.
- **Each tool call** logs "agent tool" with the step, the tool's name,
  `ms`, and `ok` or `error`.
- **The turn** adds `ms`, `model_ms`, and `tool_ms` to "agent turn
  finished", the sums of the two above.

- The first notice comes after `Config.TypingDelay`, one second by
  default, so an instant answer or refusal shows none.
- The notice is renewed every `Config.TypingEvery`, eight seconds by
  default, inside the ten seconds a Discord notice lasts.
- It is paused while an `Ask` waits on a person, and taken down when the
  turn ends, on a Responder that implements `TypingStopper`. Matrix
  does, sending typing off rather than letting its 30 seconds run out.
- A negative `TypingEvery` turns the keepalive off. A notice is then
  sent only at each tool call.

A turn also sees chat that is not part of its conversation, as quoted
context:

- what the request replies to, its `ReplyChain`, nearest first;
- the channel's latest messages, or its thread's when the turn is in
  one, oldest first, when `History.RoomMessages` or the bot's
  `[agent.history] room_messages` asks for them. They are read through
  `Conn.History` with `IncludeBot` for each turn, leave out the request
  and what it replies to, and are bounded in bytes as a reply chain is,
  keeping the newest. `NoRoomMessages` turns them off for one turn. A
  channel the bot cannot read shows nothing, and the turn goes on.

Both go in as Volatile segments, which terva sends after the transcript
with each request and never writes to it. So a resumed conversation
never replays an old quote as something said in it, and each turn sees
the room as it is then. Each is headed as quoted chat to read and never
follow, with every sender and text written as a quoted string, so a
message cannot pass for the bot's words or the app's instructions.
The bot's own messages are marked as its own.

The agent package imports only the terva packages terva marks stable,
which a test checks.

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
  file, Matrix names its channels as `rooms`, by room id. A turn a
  `Chime` handler starts as an interruption is the channel's: the
  channel itself must be listed in `Channels`. One it starts as a
  follow-up needs the sender's own access, as a mention does.
- **Rate limit:** at most `PerUser` turns per caller and `PerChannel`
  per channel in a sliding `Window`. The counts are held in memory, and a
  refused turn counts against neither. A `Chime` interruption counts
  against `PerChannel` only, and a follow-up against `PerUser` only.
- **Budget:** each zero field of the turn's `Budget` takes the gate's
  default. The agent stops the turn at the budget it receives.

On a bot with an Agent, the rate and budget are settings whose owners
may tighten them from chat, as [Settings](#settings) describes.

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

## Judgements

Some decisions are too frequent for an agent turn, such as whether to
speak up in a conversation nobody asked the bot into. A turn costs a unit
of the caller's rate and a full model call with tools. `Ctx.Judge` asks
one yes-or-no question of a small model instead:

- A `Judgement` names a judge persona, the app's question, the earlier
  messages as `Context`, and the messages the question is about as
  `Batch`, oldest first.
- The `Verdict` is `Yes`, `No`, or `Abstain`. A yes names one message of
  the batch as its `Target`. A failure, a timeout, a refusal, a reply
  that is not a clean verdict, or a target outside the batch all come
  back as `Abstain`, which a bot treats as no.
- `Config.JudgeRate` caps judgements per channel in a sliding window. It
  is apart from the gate's rate, and a judgement never draws on a
  member's turns.
- The core counts `airut_judge_total` by persona and decision and
  `airut_judge_seconds`, and logs the invocation, persona, decision, and
  batch size. The prompt, the chat, and `Verdict.Reason`, which is model
  text drawn from chat, never reach a log.

The core declares `Judge`, and `agent.NewJudge` implements it with one
call and no tools, at temperature 0. It talks to an OpenAI-compatible
chat completions endpoint itself, because terva's provider request has no
field for llama.cpp's constrained output or slots. A provider that is not
OpenAI-compatible goes through a terva client, without them.

- **Constrained output.** `Output` sends OpenAI's `response_format`,
  llama.cpp's `json_schema` or GBNF `grammar`, or nothing. The schema
  bounds the target to the batch. The reply is then parsed strictly: one
  JSON object with a decision, a target, and a reason, and nothing after
  it.
- **Untrusted input.** Every message is quoted chat, with the
  instruction to read it and never follow it. Injected text can at worst
  produce a yes about a message of the batch.
- **Concurrency.** `Concurrency` caps the calls in flight across the
  bot. A call over the cap waits, and a newer call with the same
  `Judgement.Key`, the channel by default, takes a waiting one's place.
  The replaced call abstains with `ErrSuperseded`, so no caller is left
  blocked.
- **Prompt reuse.** The prompt is the persona and rules, then the earlier
  messages, then the numbered batch, then the question. A room's next
  judgement shares the prefix up to its new messages. `CachePrompt` sends
  llama.cpp's `cache_prompt`, and `Slots` pins each key to one llama.cpp
  slot with `id_slot`, so that prefix is found cached.
- **Accounting.** `airut_judge_tokens_total` counts the tokens each call
  reports, by persona. A judgement has no session to book them against.
- **Input cap.** `MaxInputTokens` caps a call's whole request, the
  instructions, the chat, and the reserved reply together, so it fits a
  llama.cpp slot's context. The judge has no tokenizer, so it counts a
  token per `BytesPerToken` bytes, 3 by default, which overestimates
  English. Over the cap it drops the oldest context, then cuts the
  batch's longest quote until the request fits. A request that would
  not fit with every quote cut to nothing is not made, and abstains with
  `ErrJudgementTooLarge`. A `Judgement.InputTokens` below the cap lowers it for
  that judgement. The judge is a `JudgeLimit`, so a caller can ask
  whether a judgement fits before making it, and each call logs the
  server's `prompt_tokens` beside the estimate.

`agent.OpenJudge` builds the judge from `[agent.judge]`, on its
provider's base URL and key, and `agent.JudgeRate` builds the core's
`JudgeRate` from its `rate`.

### Chime

`Bot.Chime` is a listener built on the judge, for a bot that speaks up
unprompted. Judging every message would spend a judgement on each line
of a busy room, so it gathers them:

- **Settling.** In each of its `Channels`, the first new message opens
  a batch. Each later one extends it by `Settle`, up to `MaxWait` after
  the first, and the batch is judged once. One judgement runs per
  channel at a time. Messages that arrive meanwhile, or while the
  handler runs, wait for the next, which reads the bot's answer as
  context. No batch is dropped unjudged: a message is judged, or ages
  out by `MaxAge`.
- **What is judged.** Commands never reach the judge, and neither does a
  message older than `MaxAge` when it arrives, as after downtime. A
  message that addresses the bot, and one in a direct channel, are the
  bot's own handlers' to answer. The first is read as part of the batch
  but can never be chosen, and the second is not read at all.
- **The bot's own messages.** Whatever the bot sends in the channel
  through a `Ctx`, such as a Chime answer, a mention turn's reply, or a
  command's, joins the context marked `FromBot` and cut to `BotQuote`
  bytes, with its latest edit. That is how the judge tells a follow-up
  to the bot from chatter. What an app sends through a `Conn` directly,
  such as a work queue's posts, is neither read nor engages the room.
- **Context.** The judge reads the channel's earlier messages before the
  batch. The window keeps its start while it grows to twice
  `ContextMessages`, then drops to `ContextMessages` in one step. A
  sliding window would change the prompt's first message on every
  judgement, and the model server could reuse almost none of its cache.
  `ContextAge` and `ContextBytes` bound it the same way: past either, it
  drops to half. With a judge that is a `JudgeLimit`, a judgement that
  would not fit drops the oldest half of the window until it does, and
  `ContextTokens` lowers the judge's cap. The window lives in memory, so
  a restart starts it empty.
- **Cold and engaged.** A channel is cold until the bot speaks in it.
  Its answers are interruptions: `MaxPerHour` caps them, and `Cooldown`
  is the gap after each. Messages that arrive while a channel is at its
  cap or in its gap are deferred and judged together when it ends. A
  channel the bot spoke in is engaged for `EngagedFor`, restarted by
  each of its sends. People talking among themselves do not extend it.
  An engaged channel is judged with `FollowUpPersona` and
  `FollowUpPrompt`, with no gap and no cap.
- **A yes that came too late.** Before `Handler` runs, a yes is
  dropped when a message that arrived after the batch closed replies to
  the target, or comes from someone else, the bot included, and replies
  to nothing. Either may already be the answer. A second judgement
  would cost one every time the room is busy, which is when judgements
  are scarcest, so the rule is this cheap one. `StayOut` drops a yes
  about a message that replies to another person, and does not judge
  such a message at all when the listener saw the message replied to.
  Otherwise it fetches that message, and one it cannot read counts as a
  person's. A reply to the bot is no exchange between other people.
- **Whose turn.** The handler's `Ctx` stands on the chosen message, so a
  reply answers it, and `Chimed.FollowUp` says which kind it is. An
  interruption's agent turn is the channel's, because nobody asked for
  it: the channel must be in the gate's `Access.Channels`, and the turn
  takes one of `PerChannel`'s turns and none of the sender's. A
  follow-up's is the sender's, as a mention's is: it needs their access
  and takes one of their `PerUser` turns, none of `PerChannel`'s. So
  interruptions, follow-ups, and the room's mentions and commands draw
  on three separate limits, and `Config.JudgeRate` stays as a safety
  limit for the model server.
- **Hooks.** `OnPicked` runs on a yes, just before the handler, so an app
  can react to the message it is answering. `OnLimited` runs when a
  message in an engaged channel cannot be answered because the judge's
  rate or the sender's `PerUser` stopped it, which Chime asks the gate
  about before the handler runs. A cold channel calls neither on a
  limit, since an unasked bot stays silent.
- **Settings and state.** With `Settings` set to a prefix, the listener
  declares `PREFIX.cooldown`, `PREFIX.max_per_hour`,
  `PREFIX.engaged_for`, `PREFIX.max_age`, and, with a capped
  `JudgeLimit`, `PREFIX.context_tokens`, read at use time. The
  configured `MaxPerHour` and the judge's cap are ceilings chat cannot
  raise. `Bot.ChimeState` reports when a channel goes cold, when its gap
  ends, how many interruptions are left this hour, and what is pending.
  A channel belongs to one listener, so its state has one answer.
- **Misses.** A reaction among `Misses` on a reply the handler sent
  counts once in `airut_chime_misses_total`. `airut_chime_total` counts
  each batch by outcome and by whether the channel was engaged:
  `spoke`, `failed`, `declined`, `stale`, `stayed_out`, `not_judged`,
  `deferred`, `aged_out`, `limited`, or `refused`. A yes is logged as
  `picked`.

## Sessions

A turn's session is the conversation it remembers, named by
`c.SessionKey(scope)`: an id-only key per channel, per thread, per
user, or per user in one channel (`PerUserChannel`), or the one the
`Conversation` scope picks, described below. Inside a running turn the
turn's own scope names the session the turn resolved to. In a direct
channel every scope but `NoSession` names the one conversation there,
so what a person says to the bot alone is never resumed in a group room,
and the reverse. `agent/sessions` keeps them in one SQLite database per bot, on the
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
- A turn's `History` bounds what it resumes: `MaxMessages` and
  `MaxTokens`, with tokens estimated at four bytes each, and `MaxAge`.
  A field the turn leaves zero takes the bot's `[agent.history]`
  default, and with neither nothing is bounded but the model's window.
  `Compaction` says what happens to the oldest part past the caps:
  `summarize`, the default, has terva compact before the turn, keeping
  a recent tail beside the summary; `drop` leaves it out of the resume.
  Either way the cut moves on to the next message a person sent, so a
  resume never opens on a tool result whose call was cut. Messages
  older than `MaxAge` are left out whatever the compaction, by the time
  terva recorded on each.
- A dropped message stays in the log until the session is forgotten or
  expires, and is left out again at each resume. Nothing needs
  migrating: terva has always recorded each message's time.
- The agent also links each of the bot's answers to the session that
  produced it, and the core declares this as `SessionLinker`. `StreamTo`
  links the reply it posts, and `TurnRun.Link` links a message an app
  posted itself from `Text` or `RunTo`. A reply to a linked answer
  resumes that session, whatever the turn's scope, when the person
  replying may read it:
  - anyone in the channel for the conversation the channel or one of
    its threads shares;
  - only that person for their own, and only in its channel, or for a
    `PerUser` one outside every direct channel. A direct channel's
    conversation is never resumed from anywhere else.

  Otherwise the turn's own scope decides, and the turn still sees what
  the reply replies to as quoted chat. That is the owner's rule for a
  reply into another member's private conversation: a reply points the
  bot at something, and never opens someone else's history. Links are
  ids only, and go with their session when it is forgotten or expires.
- The `Conversation` scope follows a person as someone in the room
  would, in this order: a reply resumes what it replies to, as above; a
  thread is one conversation for everyone in it; a person who speaks
  again within `History.FollowWithin`, 15 minutes by default, goes on in
  the conversation they last spoke in in the channel, whether their own,
  a shared one they replied into, or a thread they came back from;
  otherwise a new one of their own starts. The agent records, per person
  and channel, the session each `Conversation` turn ran in, and the
  record goes with its session. Each of a person's own conversations in
  a channel is a generation of their per-user-in-a-channel key, named by
  when it began, so an old one stays until it expires, and a reply to
  its answers still resumes it. A direct channel keeps its one
  conversation under every scope, `Conversation` included. Without a
  `SessionLinker` the scope is `PerUserChannel`.
- When the bot's agent can forget sessions, the bot gains a built-in
  `forget` command. Plain `forget` clears the caller's own
  conversations: their per-user session and, when the agent is a
  `SessionPrefixForgetter`, every per-user-in-a-channel one, by the key
  prefix their id begins. `forget channel` and `forget thread` clear a
  conversation others share, so only a caller `Config.ForgetShared`
  admits may use them, and with it unset nobody may. A bot command named
  `forget` takes the built-in's place.

The queries are `.sql` files compiled by sqlc into typed `database/sql`
code, which is committed; `just sqlc-check` fails when it is stale. The
schema is a set of forward-only migrations embedded in the binary.

The database holds conversation text. Protect it and back it up like the
Matrix crypto store; airut never logs what is in it.

## Memory

An agent that is a `MemoryKeeper` keeps long-term memory in three
scopes: a person's, a channel's, and the bot's own. The core decides who
may write, see, and remove a memory, so the rules hold for any agent.
The keeper stores memories, folds near duplicates together, and archives
what falls past a cap. `agent.Agent` keeps them in the session store.

- **Built-in commands.** As with `forget`, a running bot gains three:
  - `remember TEXT` saves a memory about the caller. `--channel` saves
    it for the channel, `--bot` for the bot, `about=@someone` for
    another person, and `key=NAME` names it, so a later memory with the
    same key replaces it.
  - `memories` lists, privately, the caller's own memories, the
    channel's, and the bot's, with their numbers and who noted them.
  - `unremember ID` removes one.
  A bot command of the same name takes a built-in's place.
- **Who may use them.** `Config.MemoryAccess` is set as each built-in's
  `RestrictTo` check, so it holds for the `remember` tool too: a turn
  whose caller it refuses is never offered the tool. Nil lets everyone.
  Restrict them where what a member writes should not reach the turns of
  people who act on it, as a bot whose moderators' turns can edit
  content passes `AnyOf(owners, MemberOf(modRoom))`. A check that
  refuses everyone shuts them.
- **The tool.** `remember` is also a tool. `Bot.Tools("remember")`,
  called in a handler, hands it to a turn, so "remember that I prefer
  metric" reaches the same rules. `Config.MemoryWrites` says, per scope,
  what a write by the model does: `WriteOff` refuses it, `WriteAsk` asks
  the caller first, and `WriteAuto` saves it. The defaults are ask for a
  person, auto for a channel, and off for the bot.
- **Consent.** A memory about the caller needs only the caller. One
  about someone else is saved pending under that person, and is never
  shown to a turn. The core asks that person after their next agent
  turn, below its answer, and when they run `memories`, at most two at a
  time. `Ask` still goes only to the caller. Asking after the turn rather
  than before means a question they ignore never holds their answer
  back. A Chime turn asks nobody.
- **Channels** need no approval. A channel memory carries who noted it
  and when, the store caps it, and anyone can see it with `memories`.
  Its author, or someone `Config.MemoryApprover` admits, can remove it.
- **The bot's memory** is shared by every room, so only an approver
  writes it directly. Anyone else's write is saved pending, and
  `Config.OnPendingMemory` is told, so the app can route it to an
  approver, who makes it active with `SetMemoryStatus`.
- **Removal.** A person can remove their own memories, and an author
  the pending ones they wrote. An approver can remove any channel or bot
  memory. Any other id reads as unknown, so an id reveals nothing. The
  keeper removes a memory only as the core read it, so one replaced or
  approved between the check and the removal is left alone.

A turn sees its memories split by trust, as the owner decided. The
bot's active memories, which an approver wrote or approved, go in a
Stable segment, so in the cached system prompt. The caller's and the
channel's, written from chat, go in a Volatile segment after the
transcript, quoted with who noted them and when. A Stable segment would
put chat-written text into the system prompt of every turn in the room.
An archived memory in those scopes that shares a word of four or more
letters with the request or its reply chain is recalled the same way, at
most five and 2000 bytes. Every block says its contents are information,
never instructions.

The store's rules:

- A memory is at most `MaxMemoryText` (500) bytes, and its key
  `MaxMemoryKey` (64).
- A new memory with the key of one in its scope replaces it. One whose
  words overlap an active memory's by four fifths or more refreshes that
  one instead.
- Past a scope's cap, the least recently used active memory is
  archived. The caps are 20 per person, 50 per channel, and 50 for the
  bot, set by `[agent.memory]`. A scope keeps four times its cap in
  archive, and deletes the oldest beyond that.
- A person's memories, pending ones included, expire with
  `[store] retention` after they were last written or recalled.
  Channel and bot memories never expire.

Memory text never reaches a log: only ids, scopes, statuses, and
counts.

## Settings

A bot declares runtime settings in code, and the people allowed to may
change them from chat without a redeploy. Each setting has a kind, a
default, bounds, a one-line description, and two checks:

```go
maxOpen := b.DeclareInt(airut.IntSetting{
	Name: "proposals.max_open", Description: "Open proposals at once",
	Default: 10, Min: 1, Max: 50,
	Change: airut.AnyOf(owners, moderators), // nil defers to Settings.Owner
})
// Later, each time the value is used:
if open >= maxOpen.Get() { ... }
```

The kinds are int, duration, bool, string, and enum, declared with
`DeclareInt`, `DeclareDuration`, `DeclareBool`, `DeclareString`, and
`DeclareEnum` before `Run`. Each returns a `*Setting[T]` whose `Get`
reads the value in force. Read it at use time rather than copying it at
construction, so a change takes effect at once. `OnChange` runs after a
change is saved, one change at a time, for what has to be rebuilt; a
panic in it is logged, and the change stands.

- **Layers.** A value comes from the default, then the config file's
  `[settings]` table, then an override set from chat. `Run` fails on a
  name in `[settings]` that nothing declares, or a value its setting
  refuses, so a misspelt setting cannot silently do nothing. A stored
  override that no longer validates, as after its declaration changed,
  is logged by name and ignored, and the layers beneath apply.
- **Who.** airut has no roles, so `Config.Settings.Owner` is an access
  check the bot supplies. A setting's `Change` check defaults to it, and
  with no Owner nothing is changeable from chat. A setting's `Read`
  check defaults to everyone. One the caller may not read is answered as
  if it did not exist, so its name does not leak.
- **Built-in commands.** A bot with settings gains three, which a bot
  command of the same name replaces:
  - `settings` lists, privately, the settings the caller may read, each
    with its value and layer. `settings NAME` shows one with its bounds,
    default, configured value, and latest changes.
  - `set NAME VALUE` validates the value and answers old → new.
  - `unset NAME` drops the override and answers with the value now in
    force.
- **The agent.** `settings`, `set`, and `unset` are tools too. A change
  the model asks for runs the caller's `Change` check, then asks the
  caller "Change NAME from OLD to NEW?" with the exact values, and is
  made only if nothing changed the setting while they decided. A Chime
  turn, which nobody asked for, cannot change a setting. The generic
  approval question is not used, because it shows only the raw
  arguments, before the old value is read.
- **The store.** `Config.Settings.Store`, or the Agent when it is a
  `SettingStore`, keeps overrides and a change log of name, old, new,
  who, when, channel, and whether a command or the agent made it.
  `agent.Agent` keeps them in the session database, at most 100 changes
  per setting. Without a store, settings are listed but `set` and
  `unset` are refused, because an override a restart forgets is not one
  anybody can rely on.
- **Notices.** `Config.Settings.Notices` names channels told of each
  change, such as a moderators' room. A notice that fails is logged by
  kind and does not undo the change.

No setting kind holds a secret, and `[settings]` refuses a secret
reference: a setting's value is shown in chat. A value never reaches a
log either, since a string setting's value is chat text. The log gets
the setting's name, the layer, the actor's id, and the source.

On a bot with an Agent, the Gate's limits are settings:
`agent.per_user`, `agent.per_channel`, `agent.window`,
`agent.max_turns`, and `agent.max_tokens`. Their config layer is
`Config.Gate`, from `[agent]` rate and budget, and it is also their
ceiling: from chat a limit can be tightened, or loosened back to the
configured value, and no further, so spend never exceeds what the
deployed file allows. A cap needs a positive window, so the window is
set first. The access lists stay out of settings for the reason owners
do: a list someone can edit from chat lets them grant themselves
access.

## Profile

`Config.ProfileEditors` registers the built-in `profile` command for
the callers it allows; nil registers none. `profile` alone shows the
bot's profile on that platform, privately. `profile name TEXT`,
`profile about TEXT`, and `profile avatar` with a picture attached each
change one field, on the platform the command was used on. The
picture's type is read from its bytes, not from what the platform
reported.

The command takes an attachment, so it can never be a tool: no agent
turn can rename the bot or change its face, however it is asked. A bot
that wants a profile kept in code can call `Conn.SetProfile` from
`OnReady`. The platform keeps the profile, so airut stores no copy of
it. The log names the field and the picture's size, never the name or
the description.

## Version and changelog

A bot tells people which version runs and what changed in it with two
built-in commands, from notes it ships with itself.

```go
//go:embed CHANGELOG.md
var notes string

b := airut.New(airut.Config{
	Version: version, // set at build time from git describe --tags
	Changelog: &airut.Changelog{
		Notes:   notes,
		Heading: "📚 New in the archive ({version}):",
	},
})
```

- **`version`.** `Config.Version` brings a `version` command that
  answers in the channel with the version as it was set, a build's
  suffix included.
- **`changelog [version]`.** `Config.Changelog` brings a `changelog`
  command that answers privately with one version's notes under the
  `Heading`, in which `{version}` is the version. With no version it
  shows the release `Config.Version` is, or the one a build such as
  `v1.3.0-4-gabc1234-dirty` was built from. Without a `Version` it
  shows the newest. For a version the notes do not cover, the answer
  says so and names the newest they do.
- **Who.** Both are open to everyone, and both are tools an app may name
  in a Turn. A bot command of the same name replaces either.

The notes are posted word for word. They are written for the rooms, in
the bot's own voice, when the release is prepared, and reviewed with
it. Commit subjects are written for developers, release notes fetched
from a forge would need network access and a token, and a model
rewriting the notes at runtime would cost a call and put text no person
reviewed in front of every room.

`terva.sh/airut/changelog` reads the notes and imports only the
standard library. A level-2 heading names a version, in Keep a
Changelog's form, `## [1.2.0] - 2026-10-22`, or plainly, as `## v1.2.0`
or `## 1.2.0`. A version's notes run to the next level-2 heading; a
`## ` line in a code fence does not end them. `## Unreleased` is
skipped with its notes, and so is the text above the first heading and
Keep a Changelog's link definitions, which chat would show raw. Any
other level-2 heading is an error, as is a version with two sections,
and `Run` fails on either before it starts an adapter, so a typo cannot
silently lose a release's notes.

Versions are semver 2.0.0, the leading `v` optional, and order by its
precedence, so `v1.10.0` is above `v1.2.0` and `v1.3.0-rc.1` below
`v1.3.0`. The package compares them itself rather than adding a module
for it. Its other helpers:

- `Covers(notes, version)` reports whether the notes have a section with
  text for a version, so a bot's release check or test can refuse to tag
  a release that has none.
- `Log.Between(after, upTo)` returns the sections above one version up
  to and including another, newest first: what an update from one to
  the other changed.
- `IsRelease` tells a release tag, prerelease included, from a build
  that git describe names after a tag, as `v1.3.0-4-gabc1234` or
  `v1.3.0-dirty`. Semver reads such a suffix as a prerelease, so this is
  the check that separates them. `Base` returns the tag a build was made
  from.

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
| `airut_agent_request_seconds` | `provider`, `model` | how long each model request took |
| `airut_agent_first_token_seconds` | `provider`, `model` | how long a request took to its first delta |
| `airut_agent_tool_seconds` | `tool` | how long each tool call took |
| `airut_judge_total` | `persona`, `decision` | judgements, by decision, abstentions included |
| `airut_judge_seconds` | `persona` | how long a judgement took, waiting for the judge included |
| `airut_judge_request_seconds` | `model`, `outcome` | how long the judge's call to its model took, by `ok` or the error's kind |
| `airut_chime_total` | `persona`, `outcome`, `engaged` | Chime batches, by how each ended |
| `airut_chime_misses_total` | `persona` | Chime replies marked as a miss |
| `airut_memory_writes_total` | `scope`, `source`, `status` | memories saved, by how they were saved |
| `airut_profile_changes_total` | `platform`, `field` | changes made with the profile command |
| `airut_memory_consents_total` | `answer` | answers to a pending memory's question |
| `airut_setting_changes_total` | `setting`, `source` | settings changed from chat, by a command or the agent |
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
- where metrics are served, and how often they are logged;
- the config layer of the bot's runtime settings, under `[settings]`.

Secrets are never in the file. A key that holds one names where the
deployment put it: an environment variable, a systemd credential, or a
file. `config.Load` reads them, and returns every problem in the file
at once, each named by its key and never quoting a value.

A bot's own configuration goes in the same file, under an `[app]` table, and
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
