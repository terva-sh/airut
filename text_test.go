package airut

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		line, prefix, name, rest string
		ok                       bool
	}{
		{"!roll 2d6 x", "!", "roll", "2d6 x", true},
		{"  !roll", "!", "roll", "", true},
		{"! roll  2d6", "!", "roll", "2d6", true},
		{"roll 2d6", "", "roll", "2d6", true},
		{"hello there", "!", "", "", false},
		{"!Roll", "!", "", "", false},
		{"!", "!", "", "", false},
		{"!!roll", "!!", "roll", "", true},
	}
	for _, c := range cases {
		name, rest, ok := SplitCommand(c.line, c.prefix)
		if name != c.name || rest != c.rest || ok != c.ok {
			t.Errorf("SplitCommand(%q, %q) = %q, %q, %v", c.line, c.prefix, name, rest, ok)
		}
	}
}

func textFields(t *testing.T, args any) []field {
	t.Helper()
	fs, err := deriveParams(reflect.TypeOf(args))
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestParseText(t *testing.T) {
	type args struct {
		Dice    string  `desc:"d"`
		Times   *int    `desc:"t"`
		Private *bool   `desc:"p"`
		Note    *string `airut:",rest" desc:"n"`
	}
	fs := textFields(t, args{})
	cases := []struct {
		line string
		want map[string]any
		err  string
	}{
		{"2d6", map[string]any{"dice": "2d6"}, ""},
		{"2d6 3", map[string]any{"dice": "2d6", "times": "3"}, ""},
		{"2d6 times=3", map[string]any{"dice": "2d6", "times": "3"}, ""},
		{"times=3 2d6", map[string]any{"dice": "2d6", "times": "3"}, ""},
		{"2d6 --private", map[string]any{"dice": "2d6", "private": "true"}, ""},
		{`"2d6 + 1"`, map[string]any{"dice": "2d6 + 1"}, ""},
		{`dice="2d6 + 1" 2`, map[string]any{"dice": "2d6 + 1", "times": "2"}, ""},
		{`"say \"hi\""`, map[string]any{"dice": `say "hi"`}, ""},
		{"2d6 3 yes for the  win, \"quoted\"", map[string]any{"dice": "2d6", "times": "3", "private": "yes", "note": `for the  win, "quoted"`}, ""},
		{"2d6 note=short", map[string]any{"dice": "2d6", "note": "short"}, ""},
		{"", map[string]any{}, ""},
		{`"unclosed`, nil, "a quote is not closed"},
		{"2d6 dice=3d6", nil, "dice: given twice"},
		// An unknown name=value is an ordinary word.
		{"a=b", map[string]any{"dice": "a=b"}, ""},
		// A quoted --flag is a word, not a flag.
		{`"--private"`, map[string]any{"dice": "--private"}, ""},
		// The rest is never parsed, so an unmatched quote in it is text.
		{`2d6 3 yes it's "odd`, map[string]any{"dice": "2d6", "times": "3", "private": "yes", "note": `it's "odd`}, ""},
		{`2d6 3 yes --private "odd`, map[string]any{"dice": "2d6", "times": "3", "private": "yes", "note": `--private "odd`}, ""},
		// A named word before the rest still binds by name.
		{`2d6 3 --private say "hi`, map[string]any{"dice": "2d6", "times": "3", "private": "true", "note": `say "hi`}, ""},
		{`2d6 "3`, nil, "a quote is not closed"},
	}
	for _, c := range cases {
		got, err := parseText(c.line, fs)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("parseText(%q) err = %v, want %q", c.line, err, c.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseText(%q) = %v, %v; want %v", c.line, got, err, c.want)
		}
	}

	type two struct {
		A string `desc:"a"`
	}
	if _, err := parseText("x y", textFields(t, two{})); err == nil || err.Error() != "too many arguments" {
		t.Errorf("too many: err = %v", err)
	}
}

func TestRestOnly(t *testing.T) {
	type question struct {
		Text string `airut:",rest" desc:"q"`
	}
	fs := textFields(t, question{})
	for line, want := range map[string]string{
		`Explain the " character`: `Explain the " character`,
		`"quoted" and "unclosed`:  `"quoted" and "unclosed`,
		`  spaced   out  `:        `spaced   out`,
		`text="named fully" `:     `named fully`,
	} {
		got, err := parseText(line, fs)
		if err != nil || got["text"] != want {
			t.Errorf("parseText(%q) = %q, %v; want %q", line, got["text"], err, want)
		}
	}
	// Set by name, the rest leaves nothing for the words after it.
	if _, err := parseText(`text=named "but unclosed`, fs); err == nil || err.Error() != "too many arguments" {
		t.Errorf("named rest then more: err = %v", err)
	}
}
