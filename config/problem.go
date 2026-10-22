package config

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Problem is one thing wrong with a config file, named by its dotted key.
type Problem struct {
	Key string
	Msg string
}

func (p Problem) Error() string { return p.Key + ": " + p.Msg }

// Error is every problem one load found, so a broken file is fixed in one
// pass rather than one restart per mistake. No problem quotes the file:
// a secret written on the line that failed must not reach a log.
type Error struct {
	File     string
	Problems []Problem
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "config: %s has %d problem", e.File, len(e.Problems))
	if len(e.Problems) != 1 {
		b.WriteByte('s')
	}
	b.WriteByte(':')
	for _, p := range e.Problems {
		b.WriteString("\n  ")
		b.WriteString(p.Error())
	}
	return b.String()
}

// Keys lists the keys with problems, in order.
func (e *Error) Keys() []string {
	out := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		out[i] = p.Key
	}
	return out
}

type problems struct {
	file string
	list []Problem
}

func (p *problems) add(key, msg string) { p.list = append(p.list, Problem{Key: key, Msg: msg}) }

func (p *problems) addf(key, format string, args ...any) { p.add(key, fmt.Sprintf(format, args...)) }

func (p *problems) err() error {
	if len(p.list) == 0 {
		return nil
	}
	sort.SliceStable(p.list, func(i, j int) bool { return p.list[i].Key < p.list[j].Key })
	return &Error{File: p.file, Problems: p.list}
}

// decodeError turns a TOML decoding failure into problems that name keys.
// go-toml's own messages are never passed on, because they can quote the
// file: the text of a value that did not parse, on a line that may hold a
// secret.
func decodeError(file string, err error) error {
	p := &problems{file: file}
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		for _, e := range strict.Errors {
			p.add(strings.Join(e.Key(), "."), "unknown key; docs/config.example.toml lists every key")
		}
		return p.err()
	}
	var dec *toml.DecodeError
	if errors.As(err, &dec) {
		row, col := dec.Position()
		if key := strings.Join(dec.Key(), "."); key != "" {
			if want := expects(key); want != "" {
				p.addf(key, "the value is not %s (line %d, column %d)", want, row, col)
			} else {
				p.addf(key, "the value is not one this key accepts (line %d, column %d)", row, col)
			}
			return p.err()
		}
		// A syntax error has no key, and the text around it is not read
		// for one: inside a multiline string, a line that looks like
		// "key = …" is the value, and may be a secret.
		p.addf("(file)", "not valid TOML at line %d, column %d", row, col)
		return p.err()
	}
	p.add("(file)", "not valid TOML, or not in the shape airut reads")
	return p.err()
}

// expects names the kind of value key takes, following toml tags through
// fileConfig. A segment naming no field, such as a provider id or an
// index, is skipped.
func expects(key string) string { return expectsIn(reflect.TypeFor[fileConfig](), key) }

// expectsIn is expects from t, the type key is relative to.
func expectsIn(t reflect.Type, key string) string {
	for _, seg := range strings.Split(key, ".") {
		for t.Kind() == reflect.Map || t.Kind() == reflect.Slice || t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return ""
		}
		f, ok := fieldByTag(t, seg)
		if !ok {
			continue
		}
		t = f.Type
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t.Kind() == reflect.String:
		return "a string"
	case t.Kind() == reflect.Int:
		return "a whole number"
	case t.Kind() == reflect.Bool:
		return "true or false"
	case t.Kind() == reflect.Float64:
		return "a number"
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.String:
		return "a list of strings"
	case t.Kind() == reflect.Slice:
		return "an array of tables"
	case t.Kind() == reflect.Map, t.Kind() == reflect.Struct:
		return "a table"
	}
	return ""
}

// fieldByTag finds the field tagged name in t, or in a struct t embeds,
// as go-toml does.
func fieldByTag(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct && f.Tag.Get("toml") == "" {
			if inner, ok := fieldByTag(f.Type, name); ok {
				return inner, true
			}
			continue
		}
		tag, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
		if tag == name {
			return f, true
		}
	}
	return reflect.StructField{}, false
}
