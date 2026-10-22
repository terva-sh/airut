package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"filippo.io/age"
	"terva.sh/seal"
)

// Redacted is what a Secret prints as, everywhere but Value.
const Redacted = "[redacted]"

// Secret is a credential read from where the deployment put it. It is a
// struct rather than a string so that nothing prints it by accident:
// every fmt verb, JSON, text encoders, and slog see Redacted. Value is
// the one way out, for the call that hands it to the party it
// authenticates to.
type Secret struct {
	v string
}

// NewSecret wraps a value, for tests and for programs that load a
// credential themselves.
func NewSecret(v string) Secret { return Secret{v: v} }

// Value returns the secret itself.
func (s Secret) Value() string { return s.v }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.v == "" }

func (s Secret) String() string {
	if s.v == "" {
		return ""
	}
	return Redacted
}

// Format covers every fmt verb. String and GoString alone are not enough:
// fmt ignores them for a verb such as %d or %x, and formats the struct's
// field instead, which is the secret.
func (s Secret) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		fmt.Fprint(f, s.GoString())
		return
	}
	fmt.Fprint(f, s.String())
}

func (s Secret) GoString() string {
	if s.v == "" {
		return "config.Secret{}"
	}
	return "config.Secret{" + Redacted + "}"
}

func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// credentialsDirEnv is where systemd puts a unit's LoadCredential files.
const credentialsDirEnv = "CREDENTIALS_DIRECTORY"

// secretSource is what a secret reference is read with: the config
// file's directory, which a relative file is relative to, and the age
// identity from [secrets] key, which opens sealed values.
type secretSource struct {
	dir string
	// key is the identity from [secrets] key. It is nil when the file
	// names none, or when the one it names could not be read, which
	// keyFailed tells apart.
	key       *age.X25519Identity
	keyFailed bool
}

// sealScope is the scope of every binding airut seals and opens, as
// seal.Binding(sealScope, key path). It is one constant for every bot:
// see SealBinding.
const sealScope = "airut"

// SealBinding is the binding a value sealed for key, a dotted TOML key
// path such as "providers.cpa.api_key" or "app.booru.token", carries.
// A sealed value opens only at the key it was sealed for.
//
// The scope is "airut" for every bot rather than a name of the bot's.
// A bot's name would come from the same file a value could be moved
// within, so it would add nothing against the party a binding guards
// against; what keeps one bot's values out of another's file is that
// each bot has its own key.
func SealBinding(key string) string { return seal.Binding(sealScope, key) }

// Seal seals value for the key path key, to every recipient, and
// returns the field to write as { sealed = "..." }. It needs no
// identity, only the recipients, which are public. It refuses a key
// path no secret is read from, and an empty value, which Load would
// refuse.
func Seal(key, value string, recipients ...age.Recipient) (string, error) {
	if err := CheckSealKey(key); err != nil {
		return "", err
	}
	if value == "" {
		return "", errors.New("config: refusing to seal an empty value")
	}
	if len(recipients) == 0 {
		return "", errors.New("config: no recipients to seal for")
	}
	field, err := seal.Seal(SealBinding(key), value, recipients...)
	if err != nil {
		// seal's errors name neither the value nor the recipient's
		// identity, but say nothing more useful than this.
		return "", fmt.Errorf("config: cannot seal a value for %s", key)
	}
	return field, nil
}

// CheckSealKey refuses a key path no secret is read from, so that a
// typo is caught when a value is sealed rather than when the bot
// starts. Seal checks it too; a tool calls it first to refuse the key
// path before it asks for the value.
//
// A key path is written as Load names keys in problems: keys joined by
// dots, each bare when it is made only of A-Z, a-z, 0-9, _ and -, and
// otherwise quoted as a TOML basic string; an array index in brackets.
// So a token in a map under [app] keyed "a.b" is app.tokens."a.b", and
// the key of the first [[app.mirrors]] is app.mirrors[0].key. There is
// one way to write each path, because the binding compares it byte for
// byte. Errors never quote the key path.
func CheckSealKey(key string) error {
	parts, err := parseKeyPath(key)
	if err != nil {
		return fmt.Errorf("config: not a key path: %w", err)
	}
	keyAt := func(i int) string {
		if i < len(parts) && !parts[i].isIndex {
			return parts[i].key
		}
		return "\x00"
	}
	hasIndex := false
	for _, pt := range parts {
		hasIndex = hasIndex || pt.isIndex
	}
	switch top := keyAt(0); {
	case top == "secrets" && keyAt(1) == "key" && len(parts) == 2:
		return errors.New("config: secrets.key opens sealed values and cannot itself be sealed")
	case top == "providers" && len(parts) == 3 && providerIDRE.MatchString(keyAt(1)) && keyAt(2) == "api_key",
		top == "discord" && len(parts) == 2 && keyAt(1) == "token",
		top == "matrix" && len(parts) == 2 && (keyAt(1) == "password" || keyAt(1) == "recovery_key"):
		return nil
	case top == "app" && len(parts) >= 2 && !parts[1].isIndex:
		return nil
	case hasIndex && top != "app":
		return errors.New("config: an index appears only in a key path under app")
	}
	return errors.New("config: no secret is read from that key path; seal for providers.ID.api_key, discord.token, matrix.password, matrix.recovery_key, or a key under app")
}

// readSealKey reads [secrets] key, the age identity that opens sealed
// values, into src. raw is a reference of the other three kinds; a
// sealed key would need a key to open it.
func readSealKey(p *problems, src *secretSource, raw any) {
	const key = "secrets.key"
	if ref, ok := raw.(map[string]any); ok {
		if _, sealed := ref["sealed"]; sealed {
			p.add(key, "the key that opens sealed values cannot itself be sealed; name it as { env }, { credential }, or { file }")
			src.keyFailed = true
			return
		}
	}
	n := len(p.list)
	content := resolveSecret(p, key, raw, src)
	if len(p.list) > n {
		src.keyFailed = true
		return
	}
	id, err := seal.ParseIdentity(content.Value())
	if err != nil {
		// seal's message names which way the content is wrong and never
		// quotes it.
		p.addf(key, "not an age identity: %s", strings.TrimPrefix(err.Error(), "seal: "))
		src.keyFailed = true
		return
	}
	src.key = id
}

// resolveSecret reads the secret that raw, a key's value in the file,
// refers to. raw is a table with exactly one of env, credential, file,
// or sealed; a plain string is refused, because secrets never live in
// the file as plaintext. A relative file is relative to src.dir, the
// config file's directory. A sealed value opens with src.key, and only
// at key.
//
// Problems name the key, never a value: not the secret read, and not the
// name or path the file gives, which could be a secret pasted into the
// wrong place.
func resolveSecret(p *problems, key string, raw any, src *secretSource) Secret {
	ref, ok := raw.(map[string]any)
	if !ok {
		s, isString := raw.(string)
		switch {
		case isString && seal.IsSealed(s):
			p.add(key, `a sealed value is written as { sealed = "enc:age:v2:..." }`)
		case isString:
			p.add(key, `a secret is never written in the file; name where it is, as { env = "NAME" }, { credential = "NAME" }, or { file = "PATH" }, or seal it with airut seal`)
		default:
			p.add(key, `the value is not a secret reference: { env = "NAME" }, { credential = "NAME" }, { file = "PATH" }, or { sealed = "enc:age:v2:..." }`)
		}
		return Secret{}
	}
	if len(ref) != 1 {
		p.add(key, "a secret reference names exactly one of env, credential, file, or sealed")
		return Secret{}
	}
	for kind, v := range ref {
		name, ok := v.(string)
		if !ok || name == "" {
			p.addf(key+"."+kind, "the value is not a non-empty string")
			return Secret{}
		}
		switch kind {
		case "env":
			val, set := os.LookupEnv(name)
			if !set || val == "" {
				p.add(key, "the environment variable it names is not set")
				return Secret{}
			}
			return Secret{v: val}
		case "credential":
			if name != filepath.Base(name) || name == "." || name == ".." {
				p.addf(key+".credential", "not a credential name: it names a file in $%s, without a path", credentialsDirEnv)
				return Secret{}
			}
			credDir := os.Getenv(credentialsDirEnv)
			if credDir == "" {
				p.addf(key, "a credential needs $%s, which systemd sets for a unit with LoadCredential=", credentialsDirEnv)
				return Secret{}
			}
			return readSecretFile(p, key, "credential", filepath.Join(credDir, name))
		case "file":
			if !filepath.IsAbs(name) {
				name = filepath.Join(src.dir, name)
			}
			return readSecretFile(p, key, "file", name)
		case "sealed":
			return openSealed(p, key, name, src)
		default:
			p.addf(key+"."+kind, "unknown key; a secret reference names env, credential, file, or sealed")
			return Secret{}
		}
	}
	return Secret{}
}

// maxBindingShown caps how much of a moved value's binding a problem
// quotes. Anyone with the recipient can seal a value, so the binding is
// text from whoever wrote the file, and is kept short.
const maxBindingShown = 128

// openSealed opens field, the sealed value at key. Each problem names
// the kind of failure, never the value, the field, or the key's
// content.
func openSealed(p *problems, key, field string, src *secretSource) Secret {
	if !seal.IsSealed(field) {
		if strings.HasPrefix(field, "enc:age:") {
			p.add(key+".sealed", "sealed in a version airut does not open; seal it again with airut seal")
		} else {
			p.add(key+".sealed", "not a sealed value: it begins "+seal.Prefix+", as airut seal prints it")
		}
		return Secret{}
	}
	switch {
	case src.keyFailed:
		p.add(key, "a sealed value, not opened: secrets.key could not be read")
		return Secret{}
	case src.key == nil:
		p.add(key, "a sealed value needs the key that opens it, named as [secrets] key")
		return Secret{}
	}
	value, err := seal.Open(SealBinding(key), field, src.key)
	var noMatch *age.NoIdentityMatchError
	switch {
	case errors.Is(err, seal.ErrMismatch):
		// Opened again only to say where the value came from; the value
		// itself is dropped.
		got, _, _ := seal.OpenUnchecked(field, src.key)
		p.addf(key, "a sealed value moved from another key: it was sealed for %s", describeBinding(got))
		return Secret{}
	case errors.As(err, &noMatch):
		p.add(key, "a sealed value that secrets.key does not open: it was sealed for another recipient")
		return Secret{}
	case err != nil:
		p.add(key, "a sealed value that is damaged and does not open")
		return Secret{}
	case value == "":
		p.add(key, "a sealed value that opens empty")
		return Secret{}
	}
	return Secret{v: value}
}

// describeBinding names the key a moved value was sealed for, or says it
// was sealed outside airut.
func describeBinding(b string) string {
	at, ok := strings.CutPrefix(b, sealScope+"|")
	if !ok {
		return "a program other than airut"
	}
	if len(at) > maxBindingShown {
		at = at[:maxBindingShown] + "..."
	}
	return strconv.Quote(at)
}

// readSecretFile reads a secret, trimming the one line ending every
// editor and echo adds. what is "credential" or "file", for messages,
// which never carry the path.
func readSecretFile(p *problems, key, what, path string) Secret {
	b, err := os.ReadFile(path)
	if err != nil {
		reason := "cannot be read"
		if errors.Is(err, os.ErrNotExist) {
			reason = "does not exist"
		} else if errors.Is(err, os.ErrPermission) {
			reason = "is not readable by this user"
		}
		p.addf(key, "the %s it names %s", what, reason)
		return Secret{}
	}
	// One line ending only: a secret may itself end in a newline.
	v, crlf := strings.CutSuffix(string(b), "\r\n")
	if !crlf {
		v = strings.TrimSuffix(v, "\n")
	}
	if v == "" {
		p.addf(key, "the %s it names is empty", what)
		return Secret{}
	}
	return Secret{v: v}
}

var _ fmt.Formatter = Secret{}
