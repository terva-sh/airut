// Package airut is a framework for Discord and Matrix bots whose
// commands run as plain code and can hand work to a scoped agent turn.
//
// A Bot holds typed commands, registered with Bot.Command, and handlers
// for the messages and reactions that are not commands, registered with
// Bot.OnMessage and Bot.OnReaction. Bot.Run carries it to each platform
// through that platform's Adapter, such as the one in package matrix. A
// handler replies through its Ctx. When it needs judgment rather than a
// lookup, it calls Ctx.Agent with a Turn that names the only tools,
// persona, session and budget the agent gets. Package agent runs those
// turns on terva's engine, and package airuttest runs a bot in process
// for tests. docs/architecture.md has the design.
//
// Every build and test runs with CGO_ENABLED=0 and -tags goolm, because
// the Matrix adapter is built on rihma, which accepts no other build. A
// build without the tag fails; see guard_nogoolm.go.
package airut
