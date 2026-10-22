package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// Owners is what a config file says about the bot's owners, as airut
// claim reads it.
type Owners struct {
	// Config are the owners [bot] owners lists, by platform name.
	Config map[string][]string
	// Store is the session database owner claims are kept in. Its Path is
	// empty when the file has no [agent] table, and so no [store].
	Store Store
}

// LoadOwners reads the config file at path for airut claim: [bot] owners
// and [store], checked as Load checks them. It reads no secret and checks
// no other table, so it works where the bot's secrets cannot be read, and
// on a file with an [app] table. Problems are reported as an *Error.
func LoadOwners(path string) (*Owners, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var f fileConfig
	if err := toml.NewDecoder(bytes.NewReader(src)).DisallowUnknownFields().Decode(&f); err != nil {
		return nil, decodeError(path, err)
	}
	p := &problems{file: path}
	o := &Owners{Config: buildOwners(p, f.Bot.Owners)}
	if f.Agent != nil {
		o.Store = buildStore(p, f.Store, filepath.Dir(path))
	} else if f.Store != nil {
		p.add("store", "a session store needs an [agent] table; without one the bot runs no agent turns")
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	return o, nil
}

// CheckOwnerID reports whether id is a user id in platform's form: a
// Matrix user id such as @name:example.org, or a Discord user id. Its
// error never quotes id.
func CheckOwnerID(platform, id string) error {
	switch platform {
	case "matrix":
		if !validMXID(id) {
			return errors.New("not a Matrix user id, such as @name:example.org")
		}
	case "discord":
		if !snowflakeRE.MatchString(id) {
			return errors.New("not a Discord user id")
		}
	default:
		return errors.New(`not a platform: use "matrix" or "discord"`)
	}
	return nil
}
