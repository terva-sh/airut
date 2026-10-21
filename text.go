package airut

import (
	"strings"
)

// token is one word of a text invocation. Double quotes group words, and
// a backslash inside quotes escapes the next character.
type token struct {
	text  string
	start int // byte offset of the token in the line
	// eq is the offset in text of the first '=' outside quotes, or -1.
	eq int
	// quoted reports whether any part of the token was quoted.
	quoted bool
}

var errUnclosedQuote = &ArgError{Msg: "a quote is not closed"}

// scanToken reads the token starting at line[i], which is not a space,
// and returns it with the offset just past it.
func scanToken(line string, i int) (token, int, error) {
	t := token{start: i, eq: -1}
	var b strings.Builder
	inQuote := false
	for ; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote && c == '\\' && i+1 < len(line):
			i++
			b.WriteByte(line[i])
		case c == '"':
			inQuote = !inQuote
			t.quoted = true
		case !inQuote && isSpace(c):
			t.text = b.String()
			return t, i, nil
		default:
			if c == '=' && !inQuote && t.eq < 0 {
				t.eq = b.Len()
			}
			b.WriteByte(c)
		}
	}
	if inQuote {
		return token{}, i, errUnclosedQuote
	}
	t.text = b.String()
	return t, i, nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// parseText binds the words of a text invocation to parameters, as
// strings. Required and optional parameters bind positionally in order,
// apart from named ones, which never do; any parameter binds as
// name=value, a boolean binds as --name, and a
// rest parameter takes the remainder of the line verbatim. The line is
// read a token at a time, so the text a rest parameter takes is never
// parsed: an unmatched quote in it is just a character.
func parseText(line string, params []field) (map[string]any, error) {
	// Attachments arrive as files, never as words, so the text grammar
	// skips them: by name and by position.
	var words []field
	for _, p := range params {
		if p.Kind != KindAttachment {
			words = append(words, p)
		}
	}
	params = words
	byName := map[string]*field{}
	for i := range params {
		byName[params[i].Name] = &params[i]
	}
	out := map[string]any{}
	set := func(p *field, v string) error {
		if _, dup := out[p.Name]; dup {
			return &ArgError{Param: p.Name, Msg: "given twice"}
		}
		out[p.Name] = v
		return nil
	}
	// named reports the parameter a token sets by name, as name=value or
	// as a --flag, and the value it sets.
	named := func(t token) (*field, string) {
		if t.eq > 0 {
			if p := byName[t.text[:t.eq]]; p != nil {
				return p, t.text[t.eq+1:]
			}
		}
		if !t.quoted && strings.HasPrefix(t.text, "--") {
			if p := byName[t.text[2:]]; p != nil && p.Kind == KindBoolean {
				return p, "true"
			}
		}
		return nil, ""
	}
	pos := 0
	for i := 0; ; {
		for i < len(line) && isSpace(line[i]) {
			i++
		}
		if i >= len(line) {
			return out, nil
		}
		for pos < len(params) && (out[params[pos].Name] != nil || params[pos].Named) {
			pos++
		}
		t, next, err := scanToken(line, i)
		if pos < len(params) && params[pos].Rest {
			// A word that names a parameter not yet given still binds by
			// name; anything else, even a word that does not scan, starts
			// the rest.
			if p, v := named(t); err == nil && p != nil && out[p.Name] == nil {
				if err := set(p, v); err != nil {
					return nil, err
				}
				i = next
				continue
			}
			out[params[pos].Name] = strings.TrimSpace(line[i:])
			return out, nil
		}
		if err != nil {
			if pos >= len(params) {
				return nil, &ArgError{Msg: "too many arguments"}
			}
			return nil, err
		}
		i = next
		if p, v := named(t); p != nil {
			if err := set(p, v); err != nil {
				return nil, err
			}
			continue
		}
		if pos >= len(params) {
			return nil, &ArgError{Msg: "too many arguments"}
		}
		out[params[pos].Name] = t.text
		pos++
	}
}
