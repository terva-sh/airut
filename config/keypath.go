package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Key paths name a place in the file, in problems and in the binding of
// a sealed value. A binding must name exactly one place, so a path is
// written so that no two places share one:
//
//   - A key is written as TOML writes it in a dotted key: bare when it is
//     made only of A-Z, a-z, 0-9, _ and -, and otherwise quoted as a TOML
//     basic string, as in app.accounts."a.b".password.
//   - An array index is written in brackets after its array, as in
//     app.mirrors[0].key, so it never reads as a table key "0", which is
//     written app.mirrors.0.

// keyPart is a key as a path writes it.
func keyPart(k string) string {
	if isBareKey(k) {
		return k
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range k {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func isBareKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// childKey is the path of key k in the table at path.
func childKey(path, k string) string { return path + "." + keyPart(k) }

// indexKey is the path of item i in the array at path.
func indexKey(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }

// pathPart is one step of a parsed key path: a key, or an index when
// isIndex is set.
type pathPart struct {
	key     string
	index   int
	isIndex bool
}

// parseKeyPath reads a key path in the form keyPart, childKey, and
// indexKey write, and refuses one they would write otherwise, so that
// the path parsed is the path a binding compares. Its errors name the
// position of a mistake, never the text.
func parseKeyPath(s string) ([]pathPart, error) {
	var parts []pathPart
	i := 0
	for {
		// A key, bare or quoted.
		if i < len(s) && s[i] == '"' {
			k, n, err := readQuoted(s[i:])
			if err != nil {
				return nil, fmt.Errorf("%w at byte %d", err, i+1)
			}
			parts = append(parts, pathPart{key: k})
			i += n
		} else {
			j := i
			for j < len(s) && s[j] != '.' && s[j] != '[' {
				j++
			}
			if !isBareKey(s[i:j]) {
				return nil, fmt.Errorf("a key at byte %d is neither bare (A-Z, a-z, 0-9, _, -) nor quoted", i+1)
			}
			parts = append(parts, pathPart{key: s[i:j]})
			i = j
		}
		// Any number of indexes.
		for i < len(s) && s[i] == '[' {
			j := strings.IndexByte(s[i:], ']')
			if j < 0 {
				return nil, fmt.Errorf("an index at byte %d has no closing bracket", i+1)
			}
			digits := s[i+1 : i+j]
			n, err := strconv.Atoi(digits)
			if err != nil || n < 0 || strconv.Itoa(n) != digits {
				return nil, fmt.Errorf("an index at byte %d is not a whole number written plainly", i+1)
			}
			parts = append(parts, pathPart{index: n, isIndex: true})
			i += j + 1
		}
		if i == len(s) {
			return parts, nil
		}
		if s[i] != '.' {
			return nil, fmt.Errorf("byte %d does not separate keys", i+1)
		}
		i++
	}
}

// readQuoted reads the quoted key s begins with, and returns it and how
// many bytes it took. It accepts only what keyPart writes for it.
func readQuoted(s string) (string, int, error) {
	var b strings.Builder
	for i := 1; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			return "", 0, errors.New("a quoted key is not valid UTF-8")
		case r == '"':
			k := b.String()
			if keyPart(k) != s[:i+1] {
				return "", 0, errors.New("a quoted key is not written the one way a key path writes it")
			}
			return k, i + 1, nil
		case r == '\\':
			if i+1 >= len(s) {
				return "", 0, errors.New("a quoted key ends in an escape")
			}
			switch e := s[i+1]; e {
			case '"', '\\':
				b.WriteByte(e)
				i += 2
			case 'b':
				b.WriteByte('\b')
				i += 2
			case 't':
				b.WriteByte('\t')
				i += 2
			case 'n':
				b.WriteByte('\n')
				i += 2
			case 'f':
				b.WriteByte('\f')
				i += 2
			case 'r':
				b.WriteByte('\r')
				i += 2
			case 'u':
				if i+6 > len(s) {
					return "", 0, errors.New("a quoted key has a short \\u escape")
				}
				n, err := strconv.ParseUint(s[i+2:i+6], 16, 32)
				if err != nil {
					return "", 0, errors.New("a quoted key has a bad \\u escape")
				}
				b.WriteRune(rune(n))
				i += 6
			default:
				return "", 0, errors.New("a quoted key has an escape TOML does not write here")
			}
		default:
			b.WriteRune(r)
			i += size
		}
	}
	return "", 0, errors.New("a quoted key has no closing quote")
}
