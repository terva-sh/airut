package airut

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Settings configures a bot's runtime settings: values the bot declares
// in code, that its config file may set, and that the people allowed to
// change them may override from chat with the built-in set and unset
// commands.
type Settings struct {
	// Owner decides who may change a setting that names no Change check
	// of its own. airut has no roles, so the bot supplies it, as a list
	// of owners. Nil lets nobody change such a setting from chat.
	Owner AccessCheck
	// Values is the config layer, by setting name, as the config file's
	// [settings] table gives it. Run fails on a name no setting declares
	// and on a value its setting refuses, so a misspelt setting does not
	// silently do nothing.
	Values map[string]string
	// Store keeps chat overrides and the change log. It defaults to the
	// Agent when that is a SettingStore. Without a store, settings are
	// listed but cannot be changed from chat, since an override a
	// restart forgets is not one anybody can rely on.
	Store SettingStore
	// Notices are the channels told of each change, such as a
	// moderators' room. A notice that fails is logged by kind and does
	// not undo the change.
	Notices []Channel
}

// SettingLayer names where a setting's value comes from.
type SettingLayer string

const (
	LayerDefault  SettingLayer = "default"
	LayerConfig   SettingLayer = "config"
	LayerOverride SettingLayer = "override"
)

// SettingSource says how a change was made: with a command, or by an
// agent turn the caller confirmed.
type SettingSource string

const (
	ByCommand SettingSource = "command"
	ByAgent   SettingSource = "agent"
)

// SettingChange is one change to a setting. Old and New are the values
// in force before and after, as text; a string setting's value is chat
// text, so they are shown, never logged.
type SettingChange struct {
	Name     string
	Old, New string
	// Unset is true when the change dropped the override, so New is the
	// value of the layer beneath.
	Unset   bool
	By      User
	Channel Channel
	Source  SettingSource
	At      time.Time
}

// SettingStore keeps settings' chat overrides and their change log.
// terva.sh/airut/agent's Agent is one.
type SettingStore interface {
	// SettingOverrides returns every stored override, by name.
	SettingOverrides(ctx context.Context) (map[string]string, error)
	// SaveSetting records ch and, in the same transaction, makes ch.New
	// the override of ch.Name, or drops the override when ch.Unset. The
	// core serialises changes, and checks before saving that the value
	// a change was computed against is still the one in force.
	SaveSetting(ctx context.Context, ch SettingChange) error
	// SettingChanges returns name's latest changes, newest first, at
	// most limit of them.
	SettingChanges(ctx context.Context, name string, limit int) ([]SettingChange, error)
}

// Setting is a declared setting. Read Get each time the value is used,
// rather than copying it once, so a change takes effect without a
// restart.
type Setting[T any] struct{ s *setting }

// Get returns the value in force.
func (s *Setting[T]) Get() T { return s.s.cur.Load().v.(T) }

// Name returns the setting's name.
func (s *Setting[T]) Name() string { return s.s.name }

// The fields every kind of setting shares.
//
// Name is lowercase words of letters, digits, and underscores, joined by
// dots, as "proposals.max_open", at most 64 bytes. Description is one
// line of 1 to 100 characters. Read decides who may see the setting, and
// nil lets everyone; a setting someone may not read is answered as if it
// did not exist. Change decides who may change it, and nil defers to
// Settings.Owner.

// IntSetting declares a whole number from Min to Max.
type IntSetting struct {
	Name, Description string
	Default, Min, Max int
	Read, Change      AccessCheck
	// OnChange runs after a change is saved, one change at a time, for
	// what has to be rebuilt when the value changes.
	OnChange func(old, new int)
}

// DurationSetting declares a duration from Min to Max.
type DurationSetting struct {
	Name, Description string
	Default, Min, Max time.Duration
	Read, Change      AccessCheck
	OnChange          func(old, new time.Duration)
}

// BoolSetting declares a switch.
type BoolSetting struct {
	Name, Description string
	Default           bool
	Read, Change      AccessCheck
	OnChange          func(old, new bool)
}

// StringSetting declares text of at most MaxLen bytes, which must be
// from 1 to MaxSettingText. Its value is shown as it is, so it is no
// place for a secret.
type StringSetting struct {
	Name, Description string
	Default           string
	MaxLen            int
	Read, Change      AccessCheck
	OnChange          func(old, new string)
}

// EnumSetting declares one of Choices.
type EnumSetting struct {
	Name, Description string
	Default           string
	Choices           []string
	Read, Change      AccessCheck
	OnChange          func(old, new string)
}

// MaxSettingText caps a string setting's value, in bytes.
const MaxSettingText = 500

// settingHistory is how many of a setting's changes "settings NAME"
// shows.
const settingHistory = 5

var settingNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`)

// setting is a declared setting of any kind.
type setting struct {
	name, desc string
	def        any
	// parse reads a value from text and checks it against the
	// declaration's bounds, with an error meant for the caller.
	parse  func(string) (any, error)
	format func(any) string
	// bounds describes the values parse accepts, for the listing.
	bounds string
	// limit, if set, checks a chat override against the other layers,
	// as the Gate's settings check theirs against the config file.
	limit        func(v any) error
	read, change AccessCheck
	onChange     func(old, new any)
	// gate is true for the Gate's own settings, whose config layer is
	// Config.Gate rather than Settings.Values.
	gate bool

	// lower is the value beneath the override, and cur the value in
	// force. Every change stores a new value in cur, so comparing
	// pointers tells whether the value moved.
	lower atomic.Pointer[settingValue]
	cur   atomic.Pointer[settingValue]
}

type settingValue struct {
	v     any
	layer SettingLayer
}

// DeclareInt declares an int setting. It panics on a declaration that
// makes no sense, such as a default outside its bounds, and once the bot
// is running.
func (b *Bot) DeclareInt(d IntSetting) *Setting[int] {
	if d.Min > d.Max {
		b.badSetting(d.Name, "Min is above Max")
	}
	s := &setting{
		name: d.Name, desc: d.Description, def: d.Default, read: d.Read, change: d.Change,
		format: func(v any) string { return strconv.Itoa(v.(int)) },
		bounds: fmt.Sprintf("a whole number from %d to %d", d.Min, d.Max),
	}
	s.parse = func(text string) (any, error) {
		n, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || n < d.Min || n > d.Max {
			return nil, Errorf("%s must be %s.", s.name, s.bounds)
		}
		return n, nil
	}
	if d.OnChange != nil {
		s.onChange = func(old, new any) { d.OnChange(old.(int), new.(int)) }
	}
	b.declare(s)
	return &Setting[int]{s}
}

// DeclareDuration declares a duration setting, written as Go writes a
// duration, such as "90s" or "1h30m". It panics as DeclareInt does.
func (b *Bot) DeclareDuration(d DurationSetting) *Setting[time.Duration] {
	if d.Min > d.Max {
		b.badSetting(d.Name, "Min is above Max")
	}
	s := &setting{
		name: d.Name, desc: d.Description, def: d.Default, read: d.Read, change: d.Change,
		format: func(v any) string { return formatDuration(v.(time.Duration)) },
		bounds: fmt.Sprintf("a duration from %s to %s", formatDuration(d.Min), formatDuration(d.Max)),
	}
	s.parse = func(text string) (any, error) {
		v, err := time.ParseDuration(strings.TrimSpace(text))
		if err != nil || v < d.Min || v > d.Max {
			return nil, Errorf("%s must be %s, such as 90s or 1h30m.", s.name, s.bounds)
		}
		return v, nil
	}
	if d.OnChange != nil {
		s.onChange = func(old, new any) { d.OnChange(old.(time.Duration), new.(time.Duration)) }
	}
	b.declare(s)
	return &Setting[time.Duration]{s}
}

// DeclareBool declares a switch, written as true or false, on or off,
// or yes or no. It panics as DeclareInt does.
func (b *Bot) DeclareBool(d BoolSetting) *Setting[bool] {
	s := &setting{
		name: d.Name, desc: d.Description, def: d.Default, read: d.Read, change: d.Change,
		format: func(v any) string { return strconv.FormatBool(v.(bool)) },
		bounds: "true or false",
	}
	s.parse = func(text string) (any, error) {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "true", "on", "yes":
			return true, nil
		case "false", "off", "no":
			return false, nil
		}
		return nil, Errorf("%s must be true or false.", s.name)
	}
	if d.OnChange != nil {
		s.onChange = func(old, new any) { d.OnChange(old.(bool), new.(bool)) }
	}
	b.declare(s)
	return &Setting[bool]{s}
}

// DeclareString declares a text setting. It panics as DeclareInt does.
func (b *Bot) DeclareString(d StringSetting) *Setting[string] {
	if d.MaxLen < 1 || d.MaxLen > MaxSettingText {
		b.badSetting(d.Name, fmt.Sprintf("MaxLen must be from 1 to %d", MaxSettingText))
	}
	s := &setting{
		name: d.Name, desc: d.Description, def: d.Default, read: d.Read, change: d.Change,
		format: func(v any) string { return v.(string) },
		bounds: fmt.Sprintf("text of at most %d bytes", d.MaxLen),
	}
	s.parse = func(text string) (any, error) {
		if len(text) > d.MaxLen || !utf8.ValidString(text) {
			return nil, Errorf("%s must be %s.", s.name, s.bounds)
		}
		return text, nil
	}
	if d.OnChange != nil {
		s.onChange = func(old, new any) { d.OnChange(old.(string), new.(string)) }
	}
	b.declare(s)
	return &Setting[string]{s}
}

// DeclareEnum declares a setting that takes one of a fixed set of
// values. It panics as DeclareInt does.
func (b *Bot) DeclareEnum(d EnumSetting) *Setting[string] {
	if len(d.Choices) == 0 {
		b.badSetting(d.Name, "an enum needs choices")
	}
	s := &setting{
		name: d.Name, desc: d.Description, def: d.Default, read: d.Read, change: d.Change,
		format: func(v any) string { return v.(string) },
		bounds: "one of " + strings.Join(d.Choices, ", "),
	}
	choices := slices.Clone(d.Choices)
	s.parse = func(text string) (any, error) {
		v := strings.ToLower(strings.TrimSpace(text))
		if !slices.Contains(choices, v) {
			return nil, Errorf("%s must be %s.", s.name, s.bounds)
		}
		return v, nil
	}
	if d.OnChange != nil {
		s.onChange = func(old, new any) { d.OnChange(old.(string), new.(string)) }
	}
	b.declare(s)
	return &Setting[string]{s}
}

func (b *Bot) badSetting(name, why string) {
	panic(fmt.Sprintf("airut: setting %q: %s", name, why))
}

// declare checks s and registers it, with its default in force.
func (b *Bot) declare(s *setting) {
	switch {
	case len(s.name) > 64 || !settingNameRE.MatchString(s.name):
		b.badSetting(s.name, "a name is lowercase words of letters, digits, and underscores, joined by dots, at most 64 bytes")
	case checkDescription("the setting", s.desc) != nil:
		b.badSetting(s.name, checkDescription("the setting", s.desc).Error())
	}
	if _, err := s.parse(s.format(s.def)); err != nil {
		b.badSetting(s.name, "the default is not one of its values")
	}
	def := &settingValue{v: s.def, layer: LayerDefault}
	s.lower.Store(def)
	s.cur.Store(def)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		b.badSetting(s.name, "declared after Run")
	}
	if b.settings == nil {
		b.settings = map[string]*setting{}
	}
	if b.settings[s.name] != nil {
		b.badSetting(s.name, "declared twice")
	}
	b.settings[s.name] = s
}

// formatDuration writes d as Go does, without the zero units Go leaves
// at the end, so an hour is "1h" rather than "1h0m0s".
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// settingStore is the store chat overrides go to, or nil for none.
func (b *Bot) settingStore() SettingStore {
	if b.cfg.Settings.Store != nil {
		return b.cfg.Settings.Store
	}
	s, _ := b.cfg.Agent.(SettingStore)
	return s
}

// startSettings applies the config layer and the stored overrides, as
// Run starts. A config value that fails fails Run; a stored override
// that no longer validates, as after a declaration changed, is logged
// by name and ignored, so the layers beneath apply.
func (b *Bot) startSettings(ctx context.Context) error {
	b.mu.Lock()
	settings := b.settings
	b.mu.Unlock()
	var errs []error
	for name, text := range b.cfg.Settings.Values {
		s := settings[name]
		switch {
		case s == nil:
			errs = append(errs, fmt.Errorf("airut: the config sets %q, which no setting declares", name))
			continue
		case s.gate:
			errs = append(errs, fmt.Errorf("airut: %q is set from the agent's configuration, not from settings", name))
			continue
		}
		v, err := s.parse(text)
		if err != nil {
			errs = append(errs, fmt.Errorf("airut: the config's %w", err))
			continue
		}
		val := &settingValue{v: v, layer: LayerConfig}
		s.lower.Store(val)
		s.cur.Store(val)
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	store := b.settingStore()
	if store == nil || len(settings) == 0 {
		return nil
	}
	overrides, err := store.SettingOverrides(ctx)
	if err != nil {
		return fmt.Errorf("airut: reading the stored settings: %w", err)
	}
	ignored := func(name string) {
		b.cfg.Logger.Warn("stored setting ignored: its value is no longer allowed", "setting", name)
	}
	// Every override that parses is applied first, and only then checked
	// against the others, since one can depend on another, as a cap on
	// its window. One that fails falls back to the layer beneath, which
	// can make another fail in turn, so the check repeats until nothing
	// falls back.
	var applied []*setting
	for _, name := range slices.Sorted(maps.Keys(overrides)) {
		s := settings[name]
		if s == nil {
			b.cfg.Logger.Info("stored setting ignored: no setting of that name is declared", "setting_bytes", len(name))
			continue
		}
		v, err := s.parse(overrides[name])
		if err != nil {
			ignored(name)
			continue
		}
		s.cur.Store(&settingValue{v: v, layer: LayerOverride})
		if s.limit != nil {
			applied = append(applied, s)
		}
	}
	for fell := true; fell; {
		fell = false
		for _, s := range applied {
			if cur := s.cur.Load(); cur.layer == LayerOverride && s.limit(cur.v) != nil {
				ignored(s.name)
				s.cur.Store(s.lower.Load())
				fell = true
			}
		}
	}
	return nil
}

// lookupSetting returns the setting named name if the caller of c may
// read it, and nil otherwise, so a setting someone may not read is
// indistinguishable from one that does not exist.
func (b *Bot) lookupSetting(c *Ctx, name string) (*setting, error) {
	b.mu.Lock()
	s := b.settings[strings.ToLower(strings.TrimSpace(name))]
	b.mu.Unlock()
	if s == nil {
		return nil, nil
	}
	ok, err := s.readable(c)
	if !ok {
		return nil, err
	}
	return s, nil
}

func (s *setting) readable(c *Ctx) (bool, error) {
	if s.read == nil {
		return true, nil
	}
	ok, err := s.read(c)
	if err != nil {
		c.bot.cfg.Logger.Info("setting read check failed", "invocation", c.inv.ID, "setting", s.name, "error_kind", errorKind(err))
	}
	return ok && err == nil, err
}

// mayChange reports whether the caller of c may change s.
func (b *Bot) mayChange(c *Ctx, s *setting) (bool, error) {
	check := s.change
	if check == nil {
		check = b.cfg.Settings.Owner
	}
	if check == nil {
		return false, nil
	}
	return check(c)
}

// changeSetting sets s to value, or drops its override when value is
// nil, for the caller of c, and answers old→new. A change an agent turn
// asks for is first put to the caller, with the exact values.
func (b *Bot) changeSetting(c *Ctx, s *setting, value *string) error {
	switch ok, err := b.mayChange(c, s); {
	case err != nil:
		return err
	case !ok:
		return Errorf("You are not allowed to change %s.", s.name)
	}
	store := b.settingStore()
	if store == nil {
		return Errorf("Settings cannot be changed here: this bot has no store to keep them in.")
	}
	if c.unasked() {
		return Errorf("A setting is changed only for someone who asked for it.")
	}
	seen := s.cur.Load()
	// next is always a new value, never the lower layer's own, so a set
	// and an unset while the caller decides cannot restore the pointer
	// the question was asked against and hide that the value moved.
	lower := s.lower.Load()
	next := &settingValue{v: lower.v, layer: lower.layer}
	if value != nil {
		v, err := s.parse(*value)
		if err != nil {
			return err
		}
		next = &settingValue{v: v, layer: LayerOverride}
	} else if seen.layer != LayerOverride {
		return Errorf("%s has no override to drop: it is %s, from the %s.", s.name, s.format(seen.v), seen.layer)
	}
	// The value an unset restores is checked too: the configured window
	// is no window at all once a cap has been set from chat.
	if s.limit != nil {
		if err := s.limit(next.v); err != nil {
			return err
		}
	}
	old, new := s.format(seen.v), s.format(next.v)
	if value != nil && old == new {
		return Errorf("%s is already %s.", s.name, old)
	}
	source := ByCommand
	if c.FromAgent() {
		source = ByAgent
		got, err := c.Ask(fmt.Sprintf("Change %s from %s to %s?", s.name, old, new), "Change", "Don't")
		if err != nil || got != "Change" {
			return errors.Join(Errorf("The caller did not agree to change %s.", s.name), err)
		}
	}

	b.settingsMu.Lock()
	if s.cur.Load() != seen {
		b.settingsMu.Unlock()
		return Errorf("%s changed while this was being decided, so nothing was changed. Ask again.", s.name)
	}
	if s.limit != nil {
		// Another setting it is checked against may have changed while
		// the caller decided.
		if err := s.limit(next.v); err != nil {
			b.settingsMu.Unlock()
			return err
		}
	}
	ch := SettingChange{Name: s.name, Old: old, New: new, Unset: value == nil, By: c.inv.Caller,
		Channel: c.inv.Channel, Source: source, At: time.Now()}
	if err := store.SaveSetting(c.ctx, ch); err != nil {
		b.settingsMu.Unlock()
		return err
	}
	s.cur.Store(next)
	if s.onChange != nil {
		s.runOnChange(b, seen.v, next.v)
	}
	b.settingsMu.Unlock()

	b.metrics.Add("airut_setting_changes_total", 1, "setting", s.name, "source", string(source))
	b.cfg.Logger.Info("setting changed", "invocation", c.inv.ID, "setting", s.name, "layer", string(next.layer),
		"source", string(source), "by", c.inv.Caller.ID)
	b.noticeSetting(c, ch)
	reply := fmt.Sprintf("%s: %s → %s.", s.name, old, new)
	if value == nil {
		reply = fmt.Sprintf("%s: %s → %s, from the %s.", s.name, old, new, next.layer)
	}
	_, err := c.replyAbout(s, reply)
	return err
}

// runOnChange calls the setting's OnChange, recovering a panic so a
// broken hook cannot take the change, which is saved, back.
func (s *setting) runOnChange(b *Bot, old, new any) {
	defer func() {
		if recover() != nil {
			b.cfg.Logger.Error("setting OnChange panicked", "setting", s.name)
		}
	}()
	s.onChange(old, new)
}

// noticeSetting tells Settings.Notices of ch.
func (b *Bot) noticeSetting(c *Ctx, ch SettingChange) {
	if len(b.cfg.Settings.Notices) == 0 {
		return
	}
	how := ""
	if ch.Source == ByAgent {
		how = ", through the assistant"
	}
	text := fmt.Sprintf("%s changed setting %s from %s to %s%s.", name(ch.By), ch.Name, ch.Old, ch.New, how)
	if ch.Unset {
		text = fmt.Sprintf("%s dropped the override of setting %s, so it went from %s to %s%s.", name(ch.By), ch.Name, ch.Old, ch.New, how)
	}
	for _, to := range b.cfg.Settings.Notices {
		conn, err := b.Conn(to.Platform)
		if err == nil {
			_, err = conn.Send(c.ctx, to, Message{Text: text})
		}
		if err != nil {
			b.cfg.Logger.Info("setting notice failed", "invocation", c.inv.ID, "platform", string(to.Platform),
				"channel", to.ID, "error_kind", errorKind(err))
		}
	}
}

// replyAbout answers about s where everyone in the channel sees it when
// everyone may read s, and privately otherwise.
func (c *Ctx) replyAbout(s *setting, text string) (MessageRef, error) {
	if s.read == nil {
		return c.Reply(text)
	}
	return c.ReplyPrivate(text)
}

// unasked reports whether c belongs to a Chime turn, which nobody asked
// for, so nobody is there to confirm anything.
func (c *Ctx) unasked() bool { return c.chime != nil || c.chimed }

type settingsArgs struct {
	Name string `airut:",optional" desc:"One setting to show, with its recent changes"`
}

// settingsCommand is the built-in settings, which is a tool too.
func settingsCommand(b *Bot) *Command {
	c, err := newCommand(b, "settings", "List the bot's settings, or show one and its recent changes", func(c *Ctx, a settingsArgs) error {
		if strings.TrimSpace(a.Name) == "" {
			text, err := b.listSettings(c)
			if err != nil {
				return err
			}
			_, err = c.ReplyPrivate(text)
			return err
		}
		s, err := b.lookupSetting(c, a.Name)
		if err != nil {
			return err
		}
		if s == nil {
			return Errorf("There is no setting %s.", strings.TrimSpace(a.Name))
		}
		text, err := b.showSetting(c, s)
		if err != nil {
			return err
		}
		_, err = c.ReplyPrivate(text)
		return err
	})
	if err != nil {
		panic("airut: the built-in settings command: " + err.Error())
	}
	c.tool = true
	return c
}

func (b *Bot) listSettings(c *Ctx) (string, error) {
	b.mu.Lock()
	all := make([]*setting, 0, len(b.settings))
	for _, s := range b.settings {
		all = append(all, s)
	}
	b.mu.Unlock()
	slices.SortFunc(all, func(x, y *setting) int { return strings.Compare(x.name, y.name) })
	var out strings.Builder
	for _, s := range all {
		if ok, _ := s.readable(c); !ok {
			continue
		}
		cur := s.cur.Load()
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		fmt.Fprintf(&out, "%s = %s (%s). %s", s.name, s.format(cur.v), cur.layer, s.desc)
	}
	if out.Len() == 0 {
		return "There are no settings you can see.", nil
	}
	out.WriteString("\nShow one with: settings NAME")
	return out.String(), nil
}

func (b *Bot) showSetting(c *Ctx, s *setting) (string, error) {
	cur, lower := s.cur.Load(), s.lower.Load()
	var out strings.Builder
	fmt.Fprintf(&out, "%s = %s, from the %s.\n%s\nIt is %s. The default is %s.", s.name, s.format(cur.v), cur.layer,
		s.desc, s.bounds, s.format(s.def))
	if lower.layer == LayerConfig {
		fmt.Fprintf(&out, " The config sets %s.", s.format(lower.v))
	}
	if store := b.settingStore(); store != nil {
		changes, err := store.SettingChanges(c.ctx, s.name, settingHistory)
		if err != nil {
			return "", err
		}
		if len(changes) > 0 {
			out.WriteString("\nLatest changes:")
		}
		for _, ch := range changes {
			fmt.Fprintf(&out, "\n%s by %s: %s → %s", ch.At.UTC().Format("2006-01-02 15:04 MST"), name(ch.By), ch.Old, ch.New)
			switch {
			case ch.Unset:
				out.WriteString(", override dropped")
			case ch.Source == ByAgent:
				out.WriteString(", through the assistant")
			}
		}
	}
	return out.String(), nil
}

type setArgs struct {
	Name  string `desc:"The setting, as settings lists it"`
	Value string `airut:",rest" desc:"Its new value"`
}

// setCommand is the built-in set, which is a tool too: a change an
// agent asks for is put to the caller first.
func setCommand(b *Bot) *Command {
	c, err := newCommand(b, "set", "Change one of the bot's settings", func(c *Ctx, a setArgs) error {
		s, err := b.lookupSetting(c, a.Name)
		if err != nil {
			return err
		}
		if s == nil {
			return Errorf("There is no setting %s.", strings.TrimSpace(a.Name))
		}
		return b.changeSetting(c, s, &a.Value)
	})
	if err != nil {
		panic("airut: the built-in set command: " + err.Error())
	}
	c.tool = true
	return c
}

type unsetArgs struct {
	Name string `desc:"The setting, as settings lists it"`
}

// unsetCommand is the built-in unset, which drops an override.
func unsetCommand(b *Bot) *Command {
	c, err := newCommand(b, "unset", "Drop a setting's override, so its config value or default applies", func(c *Ctx, a unsetArgs) error {
		s, err := b.lookupSetting(c, a.Name)
		if err != nil {
			return err
		}
		if s == nil {
			return Errorf("There is no setting %s.", strings.TrimSpace(a.Name))
		}
		return b.changeSetting(c, s, nil)
	})
	if err != nil {
		panic("airut: the built-in unset command: " + err.Error())
	}
	c.tool = true
	return c
}
