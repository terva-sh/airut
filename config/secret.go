package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

// resolveSecret reads the secret that raw, a key's value in the file,
// refers to. raw is a table with exactly one of env, credential, or file;
// a plain string is refused, because secrets never live in the file. A
// relative file is relative to dir, the config file's directory.
//
// Problems name the key, never a value: not the secret read, and not the
// name or path the file gives, which could be a secret pasted into the
// wrong place.
func resolveSecret(p *problems, key string, raw any, dir string) Secret {
	ref, ok := raw.(map[string]any)
	if !ok {
		if _, isString := raw.(string); isString {
			p.add(key, `a secret is never written in the file; name where it is, as { env = "NAME" }, { credential = "NAME" }, or { file = "PATH" }`)
		} else {
			p.add(key, `the value is not a secret reference: { env = "NAME" }, { credential = "NAME" }, or { file = "PATH" }`)
		}
		return Secret{}
	}
	if len(ref) != 1 {
		p.add(key, "a secret reference names exactly one of env, credential, or file")
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
				name = filepath.Join(dir, name)
			}
			return readSecretFile(p, key, "file", name)
		default:
			p.addf(key+"."+kind, "unknown key; a secret reference names env, credential, or file")
			return Secret{}
		}
	}
	return Secret{}
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
