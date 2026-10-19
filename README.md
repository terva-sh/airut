# airut

*Airut* is Finnish for a herald or messenger.

airut is a Go framework for writing dedicated Discord and Matrix bots.
An airut bot is an ordinary command bot: commands are typed handlers
that run as plain code. When a handler needs judgment rather than a
lookup, its author hands the work to an agent loop with `c.Agent(...)`,
and [terva](https://terva.sh)'s engine runs that loop with only the
tools, persona, session, and budget the author chose.

```go
bot := airut.New(cfg)

bot.Command("roll", "Roll dice", roll).AsTool() // a command, and a tool the agent may call

bot.Command("ask", "Ask the assistant", func(c *airut.Ctx, a AskArgs) error {
    return c.Agent(airut.Turn{
        Prompt:  a.Question,
        Persona: "server-helper",
        Tools:   bot.Tools("roll", "lookup_rules"),
        Session: airut.PerChannel,
        Budget:  airut.Budget{Turns: 8, Tokens: 40_000},
    }).StreamTo(c)
})

err := bot.Run(ctx, discord.Adapter(dcfg), matrix.New(cfg.Matrix, matrix.Options{}))
```

**Status: early.** The core is in place: commands, `Ctx`, routing, and
the gate on agent turns, tested through the in-process fake adapter in
`airuttest`. So are the agent package with its SQLite session store, and
the Matrix adapter, tested against a throwaway Synapse. The Discord
adapter is not written yet, so the last line above does not build
today. The design is in [docs/architecture.md](docs/architecture.md).

## How it differs from terva's bot mode

terva can already run as a chat bot: its connectors carry Discord,
Telegram, and Matrix conversations to an agent session, and every
message reaches the agent. airut turns that round. The bot's code is in
charge of the conversation, and the agent is a library call the author
makes when they want one. A bot with no agent commands never starts a
model.

## Packages

| Package | Holds | Imports |
|---|---|---|
| `terva.sh/airut` | commands, `Ctx`, routing, the portable reply surface, metrics | neither terva nor any platform library |
| `terva.sh/airut/discord` | the Discord adapter | discordgo |
| `terva.sh/airut/matrix` | the Matrix adapter | [rihma](https://github.com/terva-sh/rihma) |
| `terva.sh/airut/agent` | `c.Agent(...)` on terva's engine | terva |
| `terva.sh/airut/config` | the bot's TOML file: providers, platforms, agent guards; secrets by reference ([example](docs/config.example.toml)) | go-toml |

A test keeps the core package free of terva and of every platform, so a
bot that only has plain commands never builds either.

## Getting it

```sh
go get terva.sh/airut
```

The module lives at [github.com/terva-sh/airut](https://github.com/terva-sh/airut),
which receives each release. Pin a tagged version: until 1.0, a minor
version may change the API.

## Building

airut's Matrix adapter is built on rihma, which builds only without cgo
and on mautrix's pure-Go Olm backend, so every build and test runs as:

```sh
CGO_ENABLED=0 go build -tags goolm ./...
```

A build without the tag fails on purpose, naming the tag. The
[justfile](justfile) sets both for every task, and `just ci` runs the
same checks as CI: vet, gofmt, the build guard, the import-layering
test, the test suite, and a cross-build for every supported target.

## License

MIT. See [LICENSE](LICENSE).
