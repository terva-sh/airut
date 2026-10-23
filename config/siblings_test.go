package config

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

const siblingsBase = `
[matrix]
homeserver = "https://m.example.org"
user_id = "@a:example.org"
state_dir = "m"
`

func TestSiblings(t *testing.T) {
	fleet := strings.Repeat("f", MinFleetSecret)
	old := strings.Repeat("o", MinFleetSecret+8)
	t.Setenv("AIRUT_TEST_FLEET", fleet)
	t.Setenv("AIRUT_TEST_FLEET_OLD", old)
	c, err := load(t, siblingsBase+`[matrix.siblings]
users = ["@a:example.org", "@b:example.org"]
room = "!coord:example.org"
secret = { env = "AIRUT_TEST_FLEET" }
previous_secret = { env = "AIRUT_TEST_FLEET_OLD" }
`)
	if err != nil {
		t.Fatal(err)
	}
	want := &Siblings{Users: []string{"@a:example.org", "@b:example.org"}, Room: "!coord:example.org",
		Secret: NewSecret(fleet), PreviousSecret: NewSecret(old)}
	if !reflect.DeepEqual(c.Matrix.Siblings, want) {
		t.Errorf("siblings = %#v", c.Matrix.Siblings)
	}
	if c, err := load(t, siblingsBase); err != nil || c.Matrix.Siblings != nil {
		t.Errorf("without the table: %v, %v", c, err)
	}
}

// TestShortFleetSecret refuses a fleet secret under MinFleetSecret bytes,
// at either key, naming the key and never the value.
func TestShortFleetSecret(t *testing.T) {
	if len(sentinel) >= MinFleetSecret {
		t.Fatal("the sentinel is not short")
	}
	t.Setenv("AIRUT_TEST_FLEET", sentinel)
	t.Setenv("AIRUT_TEST_FLEET_OK", strings.Repeat("k", MinFleetSecret))
	for _, c := range []struct{ name, src, key string }{
		{"secret", `secret = { env = "AIRUT_TEST_FLEET" }`, "matrix.siblings.secret"},
		{"previous", "secret = { env = \"AIRUT_TEST_FLEET_OK\" }\nprevious_secret = { env = \"AIRUT_TEST_FLEET\" }", "matrix.siblings.previous_secret"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, siblingsBase+"[matrix.siblings]\nusers = [\"@b:example.org\"]\nroom = \"!coord:example.org\"\n"+c.src+"\n")
			if got := problemKeys(t, err); !slices.Equal(got, []string{c.key}) {
				t.Errorf("keys = %q\n%v", got, err)
			}
			if strings.Contains(err.Error(), sentinel) || !strings.Contains(err.Error(), "at least 32 bytes") {
				t.Errorf("the error: %v", err)
			}
		})
	}
}

func TestSiblingsProblems(t *testing.T) {
	t.Setenv("AIRUT_TEST_FLEET", strings.Repeat("f", MinFleetSecret))
	for _, c := range []struct {
		name, src string
		keys      []string
	}{
		{"empty", "[matrix.siblings]\n", []string{"matrix.siblings.room", "matrix.siblings.secret", "matrix.siblings.users"}},
		{"bad ids", "[matrix.siblings]\nusers = [\"@b:example.org\", \"" + sentinel + "\", \"@b:example.org\"]\nroom = \"#coord:example.org\"\nsecret = { env = \"AIRUT_TEST_FLEET\" }\n",
			[]string{"matrix.siblings.room", "matrix.siblings.users.1", "matrix.siblings.users.2"}},
		{"a plain secret", "[matrix.siblings]\nusers = [\"@b:example.org\"]\nroom = \"!coord:example.org\"\nsecret = \"" + sentinel + strings.Repeat("x", MinFleetSecret) + "\"\n",
			[]string{"matrix.siblings.secret"}},
		{"an unknown key", "[matrix.siblings]\nusers = [\"@b:example.org\"]\nroom = \"!coord:example.org\"\nsecret = { env = \"AIRUT_TEST_FLEET\" }\nwindow = \"1h\"\n",
			[]string{"matrix.siblings.window"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, siblingsBase+c.src)
			if got := problemKeys(t, err); !slices.Equal(got, c.keys) {
				t.Errorf("keys = %q, want %q\n%v", got, c.keys, err)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Errorf("the error quotes the file: %v", err)
			}
		})
	}
}

func TestSealSiblingSecrets(t *testing.T) {
	for _, key := range []string{"matrix.siblings.secret", "matrix.siblings.previous_secret"} {
		if err := CheckSealKey(key); err != nil {
			t.Errorf("CheckSealKey(%s) = %v", key, err)
		}
	}
	if err := CheckSealKey("matrix.siblings.room"); err == nil {
		t.Error("CheckSealKey accepted matrix.siblings.room")
	}
}

// verify_in_chat is off unless the file turns it on.
func TestVerifyInChat(t *testing.T) {
	c, err := load(t, siblingsBase)
	if err != nil || c.Matrix.VerifyInChat {
		t.Fatalf("without the key: %v, %v", c.Matrix.VerifyInChat, err)
	}
	c, err = load(t, siblingsBase+"verify_in_chat = true\n")
	if err != nil || !c.Matrix.VerifyInChat {
		t.Fatalf("with the key: %v, %v", c.Matrix.VerifyInChat, err)
	}
	if _, err := load(t, siblingsBase+"verify_in_chat = \"yes\"\n"); err == nil {
		t.Error("a string was taken for verify_in_chat")
	}
}
