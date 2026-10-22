// Package config loads a bot's configuration file: its model providers,
// its platforms, and the guards on its agent turns.
//
// The file is TOML. Secrets are never in it as plaintext: each one is
// named as an environment variable, a systemd credential, or a file, and
// Load reads it from there, or is sealed to an age recipient and bound
// to its key, and Load opens it with the key [secrets] names.
// docs/config.example.toml shows every key.
//
// The package parses into plain Go types and imports no terva package;
// agent.Providers turns the providers into terva clients.
package config

import (
	"bytes"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Config is one bot's configuration.
type Config struct {
	Bot Bot
	// Providers are the model providers, ordered by ID.
	Providers []Provider
	// Agent is nil when the file has no [agent] table, and the bot runs
	// no agent turns.
	Agent *Agent
	// Store is where agent sessions persist. Its Path is empty without
	// an agent.
	Store Store
	// Discord and Matrix are nil for a platform the bot does not serve.
	Discord *Discord
	Matrix  *Matrix
	// Metrics is how the bot reports its metrics. Its zero value reports
	// none, beyond what the core keeps in memory.
	Metrics Metrics
	// Settings are the [settings] table's values, by setting name, as
	// text, for airut.Config's Settings.Values. A nested table's keys join
	// with dots, so proposals.max_open names the same setting written
	// either way. The bot checks them against its declared settings when
	// it starts.
	Settings map[string]string
}

// Metrics is where the bot serves its metrics, and how often it logs
// them. They go to airut.Config's MetricsAddr and SummaryEvery.
type Metrics struct {
	// Listen is the address /metrics is served on, as "127.0.0.1:9464".
	// Empty serves nothing.
	Listen string
	// SummaryEvery is how often a summary is logged; zero logs none.
	SummaryEvery time.Duration
}

// Bot is the settings every platform shares.
type Bot struct {
	// Prefix triggers a text command. Empty leaves the core's default.
	Prefix string
	// ReplyDepth is how far up a reply chain the adapters resolve. Zero
	// leaves their default; -1 resolves none.
	ReplyDepth int
	// Owners are the bot's owners, by platform ("matrix", "discord"):
	// user ids, checked for the platform's form. airut.ByPlatform turns
	// it into airut.Config's Owners.
	Owners map[string][]string
}

// Agent is how agent turns run and who may start them.
type Agent struct {
	// Models is the chain of models a turn tries, in order, from the
	// file's model or models.
	Models Chain
	// Model is the first entry of Models.
	Model ModelRef
	// Budget is the default for a turn that sets none.
	Budget Budget
	Rate   Rate
	// History bounds what a turn resumes of a remembered conversation,
	// for a turn that sets no bound of its own. The zero value bounds
	// nothing.
	History History
	// Access lists who may start an agent turn, by platform. A platform
	// with no entry allows nobody.
	Access map[string]Access
	// Judge is the [agent.judge] table, or nil when there is none.
	Judge *Judge
	// Memory is the [agent.memory] table. Its zero value takes every
	// default.
	Memory Memory
}

// Memory configures long-term memory.
type Memory struct {
	// ModelWrites says, per scope ("user", "channel", "bot"), what
	// happens when the model asks to remember something: "off", "ask",
	// or "auto". A scope it leaves out takes the core's default.
	ModelWrites map[string]string
	// MaxUser, MaxChannel, and MaxBot cap the active memories of one
	// person, one channel, and the bot; zero takes the agent's default.
	MaxUser    int
	MaxChannel int
	MaxBot     int
}

// Judge configures the judge: one tool-free call for each judgement, on
// a model of its own.
type Judge struct {
	// Models is the chain of models a judgement tries, in order. Each
	// entry carries its own Output, Slots, CachePrompt, Timeout, and
	// MaxInputTokens.
	Models Chain
	// Model is the first entry of Models.
	Model ModelRef
	// Concurrency caps the judge calls in flight; zero means one.
	Concurrency int
	// Output, Slots, CachePrompt, Timeout, and MaxInputTokens are the
	// first entry's, for a caller that runs the judge on one model.
	//
	// Output is how the reply is constrained: "response_format",
	// "json_schema", "grammar", or "none"; empty means response_format.
	Output string
	// Slots, if positive, pins each room to one of this many llama.cpp
	// slots; CachePrompt sends llama.cpp's cache_prompt.
	Slots       int
	CachePrompt bool
	// Timeout bounds a call; zero takes the judge's default.
	Timeout time.Duration
	// MaxInputTokens caps each call's whole request, estimated at
	// BytesPerToken bytes a token; zero caps nothing, and zero
	// BytesPerToken takes the judge's default.
	MaxInputTokens int
	BytesPerToken  float64
	// Rate caps the judgements per channel; zero caps nothing.
	Rate JudgeRate
}

// JudgeRate caps judgements per channel in a sliding window.
type JudgeRate struct {
	PerChannel int
	Window     time.Duration
}

// Budget caps one agent turn.
type Budget struct {
	MaxTurns  int
	MaxTokens int
}

// History bounds what an agent turn resumes. A zero field bounds
// nothing; Compaction is "summarize", "drop", or empty for the agent's
// default.
type History struct {
	MaxMessages int
	MaxTokens   int
	MaxAge      time.Duration
	Compaction  string
	// RoomMessages is how many of the channel's latest messages a turn
	// sees as quoted chat, 0 for none.
	RoomMessages int
	// FollowWithin is how soon a person must speak again to go on with a
	// Conversation; zero takes airut's default.
	FollowWithin time.Duration
}

// Rate limits agent turns in a sliding window.
type Rate struct {
	PerUser    int
	PerChannel int
	Window     time.Duration
	// Owners is [agent.rate.for.owners], or nil when the file has none
	// and owners take the members' rate. A field the file leaves out
	// holds the members' value.
	Owners *GroupRate
	// Wait lets a turn the rate refuses wait for its slot, as
	// airut.Gate.Wait describes. Off by default.
	Wait bool
}

// GroupRate is the rate of a group of callers with caps of their own,
// counted in the Rate's Window. Zero means no cap of that kind.
type GroupRate struct {
	PerUser    int
	PerChannel int
}

// Access lists platform user and role ids, and the rooms whose members
// may start a turn.
type Access struct {
	Users []string
	Roles []string
	// Rooms admit their members. Only Matrix reads it.
	Rooms []string
}

// Store is the agent's session database.
type Store struct {
	Path      string
	Retention time.Duration
}

// Discord is the Discord platform's settings.
type Discord struct {
	Token Secret
	// Guilds register commands in these guilds; empty registers them
	// globally.
	Guilds []string
}

// Matrix is the Matrix platform's settings.
type Matrix struct {
	Homeserver string
	UserID     string
	// Password logs in when no session is stored yet. It may be zero
	// once one is.
	Password   Secret
	StateDir   string
	DeviceName string
	// RecoveryKey, if set, is the account's cross-signing recovery key.
	// With it the adapter signs its own device at start when the device
	// is not signed yet, as after the state directory was re-created.
	RecoveryKey Secret
	// Siblings is the [matrix.siblings] table, or nil without one.
	Siblings *Siblings
}

// MinFleetSecret is the fewest bytes a fleet secret may have. Every
// member of the coordination room sees each proof's MAC, so a secret
// short enough to guess could be found from one by trying guesses
// offline.
const MinFleetSecret = 32

// Siblings are the other airut bots of one owner, which cross-sign each
// other's Matrix identities after proving, with a shared fleet secret,
// that each is who it says. The matrix package documents the exchange.
type Siblings struct {
	// Users are the sibling bots' user ids. Only a bot on the list is
	// signed; the secret alone vouches for nobody else. The bot's own id
	// may be on it, so every sibling can share one list, and is skipped.
	Users []string
	// Room is the coordination room's id, where proofs are posted.
	Room string
	// Secret is the fleet secret, the same on every sibling, of
	// MinFleetSecret bytes or more.
	Secret Secret
	// PreviousSecret, if set, is the secret being replaced. Proofs made
	// with it are still accepted, so bots on the old and the new secret
	// agree while the credential is replaced on each. It never makes a
	// proof.
	PreviousSecret Secret
}

// Defaults for keys a file may leave out.
const (
	DefaultRetention  = 30 * 24 * time.Hour
	DefaultPerUser    = 5
	DefaultPerChannel = 20
	DefaultWindow     = 10 * time.Minute
)

// The file's shape. Secrets decode as any, so that a plain string can be
// refused with a reason rather than accepted.
type fileConfig struct {
	Bot       fileBot                 `toml:"bot"`
	Providers map[string]fileProvider `toml:"providers"`
	Agent     *fileAgent              `toml:"agent"`
	Store     *fileStore              `toml:"store"`
	Discord   *fileDiscord            `toml:"discord"`
	Matrix    *fileMatrix             `toml:"matrix"`
	Metrics   *fileMetrics            `toml:"metrics"`
	Secrets   *fileSecrets            `toml:"secrets"`
	// Settings is any for the reason App is.
	Settings any `toml:"settings"`
	// App is the bot's own settings, which LoadApp decodes. It is any,
	// not a table, so that a value of the wrong type is a problem
	// reported with the rest rather than a failure of the whole decode.
	App any `toml:"app"`
}

// fileSecrets is the [secrets] table: the key that opens sealed values.
type fileSecrets struct {
	Key any `toml:"key"`
}

type fileBot struct {
	Prefix     string              `toml:"prefix"`
	ReplyDepth *int                `toml:"reply_depth"`
	Owners     map[string][]string `toml:"owners"`
}

type fileAgent struct {
	Model string `toml:"model"`
	// Models holds strings and tables, which buildChain reads.
	Models  any          `toml:"models"`
	Budget  *fileBudget  `toml:"budget"`
	Rate    *fileRate    `toml:"rate"`
	History *fileHistory `toml:"history"`
	Access  fileAccess   `toml:"access"`
	Judge   *fileJudge   `toml:"judge"`
	Memory  *fileMemory  `toml:"memory"`
}

type fileMemory struct {
	ModelWrites map[string]string `toml:"model_writes"`
	MaxUser     int               `toml:"max_user"`
	MaxChannel  int               `toml:"max_channel"`
	MaxBot      int               `toml:"max_bot"`
}

type fileJudge struct {
	Model       string  `toml:"model"`
	Models      any     `toml:"models"`
	Concurrency int     `toml:"concurrency"`
	Output      *string `toml:"output"`
	Slots       *int    `toml:"slots"`
	CachePrompt *bool   `toml:"cache_prompt"`
	Timeout     *string `toml:"timeout"`
	MaxInput    *int    `toml:"max_input_tokens"`
	BytesPerTok float64 `toml:"bytes_per_token"`
	Rate        *struct {
		PerChannel int    `toml:"per_channel"`
		Window     string `toml:"window"`
	} `toml:"rate"`
}

type fileHistory struct {
	MaxMessages  int    `toml:"max_messages"`
	MaxTokens    int    `toml:"max_tokens"`
	MaxAge       string `toml:"max_age"`
	Compaction   string `toml:"compaction"`
	RoomMessages int    `toml:"room_messages"`
	FollowWithin string `toml:"follow_within"`
}

type fileBudget struct {
	MaxTurns  int `toml:"max_turns"`
	MaxTokens int `toml:"max_tokens"`
}

type fileRate struct {
	PerUser    *int   `toml:"per_user"`
	PerChannel *int   `toml:"per_channel"`
	Window     string `toml:"window"`
	Wait       bool   `toml:"wait"`
	// For is any, not a table of groups, so that a key other than owners
	// is a problem saying so, even one shaped for a later release.
	For any `toml:"for"`
}

type fileGroupRate struct {
	PerUser    *int `toml:"per_user"`
	PerChannel *int `toml:"per_channel"`
}

type fileAccess struct {
	Discord *fileIDs       `toml:"discord"`
	Matrix  *fileMatrixIDs `toml:"matrix"`
}

type fileMatrixIDs struct {
	Users []string `toml:"users"`
	Roles []string `toml:"roles"`
	Rooms []string `toml:"rooms"`
}

type fileIDs struct {
	Users []string `toml:"users"`
	Roles []string `toml:"roles"`
}

type fileStore struct {
	Path      string `toml:"path"`
	Retention string `toml:"retention"`
}

type fileDiscord struct {
	Token  any      `toml:"token"`
	Guilds []string `toml:"guilds"`
}

type fileMetrics struct {
	Listen       string `toml:"listen"`
	SummaryEvery string `toml:"summary_every"`
}

type fileMatrix struct {
	Homeserver  string        `toml:"homeserver"`
	UserID      string        `toml:"user_id"`
	Password    any           `toml:"password"`
	StateDir    string        `toml:"state_dir"`
	DeviceName  string        `toml:"device_name"`
	RecoveryKey any           `toml:"recovery_key"`
	Siblings    *fileSiblings `toml:"siblings"`
}

type fileSiblings struct {
	Users          []string `toml:"users"`
	Room           string   `toml:"room"`
	Secret         any      `toml:"secret"`
	PreviousSecret any      `toml:"previous_secret"`
}

// Load reads and checks the config file at path, and reads the secrets
// it names. Every problem is reported at once, as an *Error.
func Load(path string) (*Config, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return parse(path, src, reflect.Value{})
}

// parse reads src, the file at path, and decodes its [app] table into
// app when app is a struct LoadApp was given.
func parse(path string, src []byte, app reflect.Value) (*Config, error) {
	var f fileConfig
	dec := toml.NewDecoder(bytes.NewReader(src)).DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, decodeError(path, err)
	}
	p := &problems{file: path}
	sec := &secretSource{dir: filepath.Dir(path)}
	// The key is read before any secret, so that a sealed value anywhere
	// in the file opens with it.
	if f.Secrets != nil && f.Secrets.Key != nil {
		readSealKey(p, sec, f.Secrets.Key)
	}
	c := build(p, &f, sec)
	if f.App != nil {
		table, isTable := f.App.(map[string]any)
		switch {
		case !app.IsValid():
			p.add("app", "this program has no settings of its own to read from an [app] table")
		case !isTable:
			p.add("app", "the value is not a table")
		default:
			decodeTable(p, "app", table, app, sec)
		}
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	return c, nil
}

// maxReplyDepth bounds bot.reply_depth: each link is a request.
const maxReplyDepth = 10

// maxRoomMessages bounds agent.history.room_messages, as
// airut.MaxRoomMessages bounds a turn's.
const maxRoomMessages = 50

var (
	providerIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	snowflakeRE  = regexp.MustCompile(`^[0-9]{1,20}$`)
)

func build(p *problems, f *fileConfig, src *secretSource) *Config {
	c := &Config{Bot: Bot{Prefix: f.Bot.Prefix}}
	if strings.ContainsAny(f.Bot.Prefix, " \t\r\n") {
		p.add("bot.prefix", "a prefix cannot contain spaces")
	}
	if d := f.Bot.ReplyDepth; d != nil {
		switch {
		case *d < 0 || *d > maxReplyDepth:
			p.add("bot.reply_depth", fmt.Sprintf("must be 0 to %d", maxReplyDepth))
		case *d == 0:
			c.Bot.ReplyDepth = -1
		default:
			c.Bot.ReplyDepth = *d
		}
	}
	c.Bot.Owners = buildOwners(p, f.Bot.Owners)

	if m := f.Metrics; m != nil {
		c.Metrics.Listen = m.Listen
		if m.Listen != "" {
			if _, port, err := net.SplitHostPort(m.Listen); err != nil || port == "" {
				p.add("metrics.listen", `not an address such as "127.0.0.1:9464" or ":9464"`)
			}
		}
		if m.SummaryEvery != "" {
			c.Metrics.SummaryEvery = duration(p, "metrics.summary_every", m.SummaryEvery)
		}
	}

	ids := make([]string, 0, len(f.Providers))
	for id := range f.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c.Providers = append(c.Providers, buildProvider(p, id, f.Providers[id], src))
	}

	if f.Agent != nil {
		c.Agent = buildAgent(p, f.Agent, c.Providers)
		c.Store = buildStore(p, f.Store, src.dir)
	} else if f.Store != nil {
		p.add("store", "a session store needs an [agent] table; without one the bot runs no agent turns")
	}

	if f.Discord != nil {
		d := &Discord{Guilds: f.Discord.Guilds}
		if f.Discord.Token == nil {
			p.add("discord.token", "required: the bot token, as a secret reference")
		} else {
			d.Token = resolveSecret(p, "discord.token", f.Discord.Token, src)
		}
		for i, g := range d.Guilds {
			if !snowflakeRE.MatchString(g) {
				p.addf(fmt.Sprintf("discord.guilds.%d", i), "not a Discord id")
			}
		}
		c.Discord = d
	}
	if f.Matrix != nil {
		c.Matrix = buildMatrix(p, f.Matrix, src)
	}
	if f.Settings != nil {
		c.Settings = buildSettings(p, f.Settings)
	}
	if c.Discord == nil && c.Matrix == nil {
		p.add("(file)", "no platform is configured; add a [discord] or [matrix] table")
	}
	return c
}

// buildRateFor checks [agent.rate.for]: owners is the only group with a
// rate of its own, and a field it leaves out inherits members'. It is
// accepted without [bot] owners, since a claimed owner takes it too.
func buildRateFor(p *problems, raw any, members Rate) *GroupRate {
	table, ok := raw.(map[string]any)
	if !ok {
		p.add("agent.rate.for", "the value is not a table")
		return nil
	}
	var owners *GroupRate
	for _, k := range slices.Sorted(maps.Keys(table)) {
		key := "agent.rate.for." + k
		if k != "owners" {
			p.add(key, "only owners has its own rate")
			continue
		}
		var f fileGroupRate
		decodeValue(p, key, table[k], reflect.ValueOf(&f).Elem(), nil)
		g := GroupRate{PerUser: members.PerUser, PerChannel: members.PerChannel}
		if f.PerUser != nil {
			g.PerUser = *f.PerUser
			if g.PerUser < 0 {
				p.add(key+".per_user", "must not be negative")
			}
		}
		if f.PerChannel != nil {
			g.PerChannel = *f.PerChannel
			if g.PerChannel < 0 {
				p.add(key+".per_channel", "must not be negative")
			}
		}
		owners = &g
	}
	return owners
}

// buildOwners checks [bot] owners: each key a platform, each id in its
// platform's form. A problem names the key and the index, never the id.
// A platform the file has no table for is accepted, so a deployment can
// turn a platform off without losing its owners.
func buildOwners(p *problems, f map[string][]string) map[string][]string {
	if len(f) == 0 {
		return nil
	}
	out := make(map[string][]string, len(f))
	for _, platform := range slices.Sorted(maps.Keys(f)) {
		ids := f[platform]
		key := "bot.owners." + platform
		if platform != "matrix" && platform != "discord" {
			p.add(key, `not a platform: use "matrix" or "discord"`)
			continue
		}
		for i, id := range ids {
			if err := CheckOwnerID(platform, id); err != nil {
				p.add(fmt.Sprintf("%s.%d", key, i), err.Error())
			}
		}
		out[platform] = slices.Clone(ids)
	}
	return out
}

func buildJudge(p *problems, f *fileJudge, providers []Provider) *Judge {
	j := &Judge{Concurrency: f.Concurrency, BytesPerToken: f.BytesPerTok}
	if f.MaxInput != nil && *f.MaxInput < 0 {
		p.add("agent.judge.max_input_tokens", "must not be negative")
	}
	if f.BytesPerTok < 0 {
		p.add("agent.judge.bytes_per_token", "must not be negative")
	}
	if f.Concurrency < 0 {
		p.add("agent.judge.concurrency", "must not be negative")
	}
	// The table's output, slots, cache_prompt, and timeout override
	// every entry's model, as they set the one model's before chains.
	site := useSite{key: "agent.judge", judge: true, maxInput: f.MaxInput,
		caps: fileCaps{Output: f.Output, Slots: f.Slots, CachePrompt: f.CachePrompt, Timeout: f.Timeout}}
	checkCaps(p, site.key, site.caps)
	j.Models = buildChain(p, site, f.Model, f.Models, providers)
	if len(j.Models) > 0 {
		first := j.Models[0]
		j.Model = first.Ref()
		j.Output, j.Slots, j.CachePrompt, j.Timeout = first.Output, first.Slots, first.CachePrompt, first.Timeout
		j.MaxInputTokens = first.MaxInputTokens
	}
	if r := f.Rate; r != nil {
		j.Rate.PerChannel = r.PerChannel
		if r.PerChannel < 0 {
			p.add("agent.judge.rate.per_channel", "must not be negative")
		}
		if r.Window != "" {
			j.Rate.Window = duration(p, "agent.judge.rate.window", r.Window)
		} else if r.PerChannel > 0 {
			p.add("agent.judge.rate.window", "required with per_channel")
		}
	}
	return j
}

func buildAgent(p *problems, f *fileAgent, providers []Provider) *Agent {
	a := &Agent{Access: map[string]Access{}}

	a.Models = buildChain(p, useSite{key: "agent"}, f.Model, f.Models, providers)
	if len(a.Models) > 0 {
		a.Model = a.Models[0].Ref()
	}
	if f.Judge != nil {
		a.Judge = buildJudge(p, f.Judge, providers)
	}
	if m := f.Memory; m != nil {
		a.Memory = Memory{ModelWrites: m.ModelWrites, MaxUser: m.MaxUser, MaxChannel: m.MaxChannel, MaxBot: m.MaxBot}
		for _, s := range slices.Sorted(maps.Keys(m.ModelWrites)) {
			switch w := m.ModelWrites[s]; {
			case s != "user" && s != "channel" && s != "bot":
				p.add("agent.memory.model_writes."+s, `is not a scope: use "user", "channel", or "bot"`)
			case w != "off" && w != "ask" && w != "auto":
				p.add("agent.memory.model_writes."+s, `must be "off", "ask", or "auto"`)
			}
		}
		for _, c := range []struct {
			key string
			n   int
		}{{"max_bot", m.MaxBot}, {"max_channel", m.MaxChannel}, {"max_user", m.MaxUser}} {
			if c.n < 0 {
				p.add("agent.memory."+c.key, "must not be negative")
			}
		}
	}

	// A budget is required: it is what an agent turn may spend.
	if f.Budget == nil {
		p.add("agent.budget", "required: max_turns and max_tokens cap a turn that sets no budget")
	} else {
		a.Budget = Budget(*f.Budget)
		if a.Budget.MaxTurns <= 0 {
			p.add("agent.budget.max_turns", "must be at least 1")
		}
		if a.Budget.MaxTokens <= 0 {
			p.add("agent.budget.max_tokens", "must be at least 1")
		}
	}

	a.Rate = Rate{PerUser: DefaultPerUser, PerChannel: DefaultPerChannel, Window: DefaultWindow}
	if r := f.Rate; r != nil {
		if r.PerUser != nil {
			a.Rate.PerUser = *r.PerUser
		}
		if r.PerChannel != nil {
			a.Rate.PerChannel = *r.PerChannel
		}
		if a.Rate.PerUser <= 0 {
			p.add("agent.rate.per_user", "must be at least 1")
		}
		if a.Rate.PerChannel <= 0 {
			p.add("agent.rate.per_channel", "must be at least 1")
		}
		if r.Window != "" {
			a.Rate.Window = duration(p, "agent.rate.window", r.Window)
		}
		a.Rate.Wait = r.Wait
		if r.For != nil {
			a.Rate.Owners = buildRateFor(p, r.For, a.Rate)
		}
	}

	if h := f.History; h != nil {
		a.History = History{MaxMessages: h.MaxMessages, MaxTokens: h.MaxTokens, Compaction: h.Compaction, RoomMessages: h.RoomMessages}
		if h.RoomMessages < 0 || h.RoomMessages > maxRoomMessages {
			p.add("agent.history.room_messages", fmt.Sprintf("must be 0 to %d", maxRoomMessages))
		}
		if h.MaxMessages < 0 {
			p.add("agent.history.max_messages", "must not be negative")
		}
		if h.MaxTokens < 0 {
			p.add("agent.history.max_tokens", "must not be negative")
		}
		if h.MaxAge != "" {
			a.History.MaxAge = duration(p, "agent.history.max_age", h.MaxAge)
		}
		if h.FollowWithin != "" {
			a.History.FollowWithin = duration(p, "agent.history.follow_within", h.FollowWithin)
		}
		switch h.Compaction {
		case "", "summarize", "drop":
		default:
			p.add("agent.history.compaction", `must be "summarize" or "drop"`)
		}
	}

	if d := f.Access.Discord; d != nil {
		for i, u := range d.Users {
			if !snowflakeRE.MatchString(u) {
				p.addf(fmt.Sprintf("agent.access.discord.users.%d", i), "not a Discord user id")
			}
		}
		for i, r := range d.Roles {
			if !snowflakeRE.MatchString(r) {
				p.addf(fmt.Sprintf("agent.access.discord.roles.%d", i), "not a Discord role id")
			}
		}
		a.Access["discord"] = Access{Users: d.Users, Roles: d.Roles}
	}
	if m := f.Access.Matrix; m != nil {
		for i, u := range m.Users {
			if !validMXID(u) {
				p.addf(fmt.Sprintf("agent.access.matrix.users.%d", i), "not a Matrix user id, such as @name:example.org")
			}
		}
		if len(m.Roles) > 0 {
			p.add("agent.access.matrix.roles", "Matrix has no roles; list users or rooms")
		}
		for i, r := range m.Rooms {
			if !validRoomID(r) {
				p.addf(fmt.Sprintf("agent.access.matrix.rooms.%d", i), "not a Matrix room id, such as !abc:example.org")
			}
		}
		a.Access["matrix"] = Access{Users: m.Users, Rooms: m.Rooms}
	}
	return a
}

func buildStore(p *problems, f *fileStore, dir string) Store {
	s := Store{Retention: DefaultRetention}
	if f == nil || f.Path == "" {
		p.add("store.path", "required: an agent keeps its sessions in this database")
		return s
	}
	s.Path = f.Path
	if !filepath.IsAbs(s.Path) {
		s.Path = filepath.Join(dir, s.Path)
	}
	if f.Retention != "" {
		s.Retention = duration(p, "store.retention", f.Retention)
	}
	return s
}

func buildMatrix(p *problems, f *fileMatrix, src *secretSource) *Matrix {
	m := &Matrix{Homeserver: f.Homeserver, UserID: f.UserID, StateDir: f.StateDir, DeviceName: f.DeviceName}
	checkURL(p, "matrix.homeserver", f.Homeserver)
	if !validMXID(f.UserID) {
		p.add("matrix.user_id", "required: the bot's Matrix user id, such as @bot:example.org")
	}
	if f.Password != nil {
		m.Password = resolveSecret(p, "matrix.password", f.Password, src)
	}
	if f.RecoveryKey != nil {
		m.RecoveryKey = resolveSecret(p, "matrix.recovery_key", f.RecoveryKey, src)
	}
	if f.Siblings != nil {
		m.Siblings = buildSiblings(p, f.Siblings, src)
	}
	if m.StateDir == "" {
		p.add("matrix.state_dir", "required: the directory that keeps the Matrix session and crypto store")
	} else if !filepath.IsAbs(m.StateDir) {
		m.StateDir = filepath.Join(src.dir, m.StateDir)
	}
	return m
}

// buildSiblings checks [matrix.siblings]. Problems name the key and an
// index, never an id or a secret.
func buildSiblings(p *problems, f *fileSiblings, src *secretSource) *Siblings {
	const key = "matrix.siblings"
	s := &Siblings{Users: f.Users, Room: f.Room}
	if len(f.Users) == 0 {
		p.add(key+".users", "required: the sibling bots' Matrix user ids")
	}
	seen := map[string]bool{}
	for i, u := range f.Users {
		switch {
		case !validMXID(u):
			p.addf(fmt.Sprintf("%s.users.%d", key, i), "not a Matrix user id, such as @name:example.org")
		case seen[u]:
			p.addf(fmt.Sprintf("%s.users.%d", key, i), "listed twice")
		}
		seen[u] = true
	}
	if !validRoomID(f.Room) {
		p.add(key+".room", "required: the coordination room's id, such as !abc:example.org, never an alias")
	}
	if f.Secret == nil {
		p.add(key+".secret", "required: the fleet secret, as a secret reference")
	} else {
		s.Secret = fleetSecret(p, key+".secret", f.Secret, src)
	}
	if f.PreviousSecret != nil {
		s.PreviousSecret = fleetSecret(p, key+".previous_secret", f.PreviousSecret, src)
	}
	return s
}

// fleetSecret reads a fleet secret and refuses one shorter than
// MinFleetSecret bytes. The problem gives the bound, never the length
// read.
func fleetSecret(p *problems, key string, raw any, src *secretSource) Secret {
	n := len(p.list)
	s := resolveSecret(p, key, raw, src)
	if len(p.list) == n && len(s.Value()) < MinFleetSecret {
		p.addf(key, "a fleet secret must be at least %d bytes, made at random", MinFleetSecret)
		return Secret{}
	}
	return s
}

// checkURL requires an absolute http or https URL. It never quotes the
// value, which could carry credentials in its userinfo.
func checkURL(p *problems, key, raw string) {
	if raw == "" {
		p.add(key, "required")
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		p.add(key, "not an http or https URL")
		return
	}
	if u.User != nil {
		p.add(key, "a URL here cannot carry credentials; name the key as a secret reference")
	}
}

func duration(p *problems, key, raw string) time.Duration {
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		p.add(key, `not a positive duration such as "10m" or "720h"`)
		return 0
	}
	return d
}

func validMXID(s string) bool { return validSigiled(s, "@") }

// validRoomID accepts a room id, not an alias: an alias can be moved to
// another room, which would hand that room's members the access.
func validRoomID(s string) bool { return validSigiled(s, "!") }

func validSigiled(s, sigil string) bool {
	local, server, ok := strings.Cut(strings.TrimPrefix(s, sigil), ":")
	return strings.HasPrefix(s, sigil) && ok && local != "" && server != ""
}
