package airut

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type rollArgs struct {
	Dice    string `desc:"Dice to roll, like 2d6" min:"3" max:"20"`
	Times   *int   `desc:"How many times" min:"1" max:"10"`
	Private bool   `airut:",optional" desc:"Only you see the result"`
}

type everyKind struct {
	Text     string     `desc:"text" choices:"a, b,c"`
	Count    int64      `desc:"count"`
	Ratio    float64    `desc:"ratio"`
	Who      User       `desc:"who"`
	Where    *Channel   `desc:"where"`
	Group    *Role      `desc:"group"`
	File     Attachment `airut:",optional" desc:"file"`
	Flag     *bool      `desc:"flag"`
	Note     string     `airut:"note,optional,rest" desc:"note"`
	internal string
	Skipped  string `airut:"-"`
}

func f(v float64) *float64 { return &v }

func TestSpecFromArgs(t *testing.T) {
	b := New(Config{})
	c := b.Command("roll", "Roll dice", func(*Ctx, rollArgs) error { return nil })
	want := Spec{Name: "roll", Description: "Roll dice", Params: []Param{
		{Name: "dice", Description: "Dice to roll, like 2d6", Kind: KindString, Required: true, Min: f(3), Max: f(20)},
		{Name: "times", Description: "How many times", Kind: KindInteger, Min: f(1), Max: f(10)},
		{Name: "private", Description: "Only you see the result", Kind: KindBoolean},
	}}
	if got := c.Spec(); !reflect.DeepEqual(got, want) {
		t.Errorf("Spec:\n got %+v\nwant %+v", got, want)
	}
	if got, want := c.Usage(), "!roll <dice> [times] [private]"; got != want {
		t.Errorf("Usage = %q, want %q", got, want)
	}
}

func TestEveryKind(t *testing.T) {
	b := New(Config{Prefix: "."})
	c := b.Command("all", "Every kind", func(*Ctx, everyKind) error { return nil })
	var kinds []Kind
	var names []string
	for _, p := range c.Spec().Params {
		kinds = append(kinds, p.Kind)
		names = append(names, p.Name)
	}
	wantKinds := []Kind{KindString, KindInteger, KindNumber, KindUser, KindChannel, KindRole, KindAttachment, KindBoolean, KindString}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Errorf("kinds = %v, want %v", kinds, wantKinds)
	}
	if got := strings.Join(names, " "); got != "text count ratio who where group file flag note" {
		t.Errorf("names = %s", got)
	}
	if got := c.Spec().Params[0].Choices; !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("choices = %q", got)
	}
	if got, want := c.Usage(), ".all <text> <count> <ratio> <who> [where] [group] [file] [flag] [note...]"; got != want {
		t.Errorf("Usage = %q, want %q", got, want)
	}
}

func TestJSONSchema(t *testing.T) {
	b := New(Config{})
	c := b.Command("roll", "Roll dice", func(*Ctx, rollArgs) error { return nil })
	var got map[string]any
	if err := json.Unmarshal(c.Schema(), &got); err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(`{
		"type": "object",
		"additionalProperties": false,
		"required": ["dice"],
		"properties": {
			"dice": {"type": "string", "description": "Dice to roll, like 2d6", "minLength": 3, "maxLength": 20},
			"times": {"type": "integer", "description": "How many times", "minimum": 1, "maximum": 10},
			"private": {"type": "boolean", "description": "Only you see the result"}
		}
	}`), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("schema:\n got %s", c.Schema())
	}

	type pick struct {
		Mode string `desc:"Mode" choices:"fast,slow"`
		Who  *User  `desc:"Who"`
	}
	c = b.Command("pick", "Pick", func(*Ctx, pick) error { return nil })
	s := string(c.Schema())
	for _, frag := range []string{`"enum":["fast","slow"]`, `"description":"Who (a user id or mention)"`, `"required":["mode"]`} {
		if !strings.Contains(s, frag) {
			t.Errorf("schema %s lacks %s", s, frag)
		}
	}
}

func TestNoArgsCommand(t *testing.T) {
	b := New(Config{})
	c := b.Command("ping", "Ping", func(*Ctx) error { return nil })
	if len(c.Spec().Params) != 0 {
		t.Errorf("params = %v", c.Spec().Params)
	}
	if got := string(c.Schema()); got != `{"additionalProperties":false,"properties":{},"required":[],"type":"object"}` {
		t.Errorf("schema = %s", got)
	}
}

func TestSnakeCase(t *testing.T) {
	for in, want := range map[string]string{
		"Dice": "dice", "DiceCount": "dice_count", "UserID": "user_id",
		"HTTPServer": "http_server", "ID": "id", "A1": "a1",
	} {
		if got := snakeCase(in); got != want {
			t.Errorf("snakeCase(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRegistrationPanics checks that every definition a platform would
// reject fails at registration, naming the command and what is wrong.
func TestRegistrationPanics(t *testing.T) {
	long := strings.Repeat("x", 101)
	cases := []struct {
		name, desc string
		handler    any
		want       string
	}{
		{"Roll", "d", func(*Ctx) error { return nil }, "a command name must match"},
		{"", "d", func(*Ctx) error { return nil }, "a command name must match"},
		{strings.Repeat("a", 33), "d", func(*Ctx) error { return nil }, "a command name must match"},
		{"ok", "", func(*Ctx) error { return nil }, "1 to 100 characters"},
		{"ok", long, func(*Ctx) error { return nil }, "1 to 100 characters"},
		{"ok", "d", func() error { return nil }, "the handler must be"},
		{"ok", "d", func(*Ctx, string) error { return nil }, "the handler must be"},
		{"ok", "d", func(*Ctx, rollArgs) {}, "the handler must be"},
		{"ok", "d", "not a func", "the handler must be"},
		{"ok", "d", func(*Ctx, struct {
			A string
		}) error {
			return nil
		}, "field A (a) needs a description"},
		{"ok", "d", func(*Ctx, struct {
			A []string `desc:"a"`
		}) error {
			return nil
		}, "is not a parameter type"},
		{"ok", "d", func(*Ctx, struct {
			A *string `desc:"a"`
			B string  `desc:"b"`
		}) error {
			return nil
		}, "a required parameter cannot follow an optional one"},
		{"ok", "d", func(*Ctx, struct {
			A string `airut:",rest" desc:"a"`
			B string `desc:"b"`
		}) error {
			return nil
		}, "must be the last"},
		{"ok", "d", func(*Ctx, struct {
			A int `airut:",rest" desc:"a"`
		}) error {
			return nil
		}, "only a string can take the rest"},
		{"ok", "d", func(*Ctx, struct {
			A string `airut:"x" desc:"a"`
			B string `airut:"x" desc:"b"`
		}) error {
			return nil
		}, "a second parameter named x"},
		{"ok", "d", func(*Ctx, struct {
			A string `airut:"Bad" desc:"a"`
		}) error {
			return nil
		}, "a parameter name must match"},
		{"ok", "d", func(*Ctx, struct {
			A string `airut:",sometimes" desc:"a"`
		}) error {
			return nil
		}, "unknown airut tag option"},
		{"ok", "d", func(*Ctx, struct {
			A int `desc:"a" choices:"1,2"`
		}) error {
			return nil
		}, "choices apply only to a string"},
		{"ok", "d", func(*Ctx, struct {
			A bool `desc:"a" min:"1"`
		}) error {
			return nil
		}, "applies only to a string or a number"},
		{"ok", "d", func(*Ctx, struct {
			A int `desc:"a" min:"5" max:"1"`
		}) error {
			return nil
		}, "min is greater than max"},
		{"ok", "d", func(*Ctx, struct {
			A string `desc:"a" choices:"short,xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"`
		}) error {
			return nil
		}, "a choice can be at most 100 characters"},
		{"ok", "d", func(*Ctx, struct {
			A int `desc:"a" min:"0.5"`
		}) error {
			return nil
		}, "an integer's min must be a whole number"},
		{"ok", "d", func(*Ctx, struct {
			A int64 `desc:"a" min:"9007199254740994"`
		}) error {
			return nil
		}, "min must be a number from -9007199254740992 to 9007199254740992"},
		{"ok", "d", func(*Ctx, struct {
			A float64 `desc:"a" max:"NaN"`
		}) error {
			return nil
		}, "max must be a number from"},
		{"ok", "d", func(*Ctx, struct {
			A float64 `desc:"a" max:"Inf"`
		}) error {
			return nil
		}, "max must be a number from"},
		{"ok", "d", func(*Ctx, struct {
			A string `desc:"a" max:"9000"`
		}) error {
			return nil
		}, "from 0 to 6000"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			defer func() {
				r := recover()
				msg, _ := r.(string)
				if r == nil || !strings.Contains(msg, c.want) || !strings.Contains(msg, "airut: command") {
					t.Errorf("panic = %v, want one containing %q", r, c.want)
				}
			}()
			New(Config{}).Command(c.name, c.desc, c.handler)
		})
	}
}

func TestTooManyParams(t *testing.T) {
	fields := make([]reflect.StructField, 26)
	for i := range fields {
		fields[i] = reflect.StructField{Name: "F" + strings.Repeat("x", i), Type: reflect.TypeFor[string](), Tag: `desc:"d"`}
	}
	if _, err := deriveParams(reflect.StructOf(fields)); err == nil || !strings.Contains(err.Error(), "more than the 25") {
		t.Errorf("err = %v", err)
	}
}

func TestDuplicateAndLateRegistration(t *testing.T) {
	b := New(Config{})
	b.Command("ping", "Ping", func(*Ctx) error { return nil })
	mustPanic(t, "registered twice", func() { b.Command("ping", "Ping", func(*Ctx) error { return nil }) })
	mustPanic(t, "no command of that name is registered with AsTool", func() { b.Tools("ping") })
	mustPanic(t, "no command of that name is registered with AsTool", func() { b.Tools("nope") })
	type withFile struct {
		F Attachment `desc:"f"`
	}
	c := b.Command("upload", "Upload", func(*Ctx, withFile) error { return nil })
	mustPanic(t, "cannot be a tool", func() { c.AsTool() })
}

func mustPanic(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		if r := recover(); r == nil || !strings.Contains(r.(string), want) {
			t.Errorf("panic = %v, want one containing %q", r, want)
		}
	}()
	f()
}
