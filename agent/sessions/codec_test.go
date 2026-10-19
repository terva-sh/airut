package sessions

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"terva.sh/terva/packages/provider"
)

// blocks is one of each content block the codec carries. A block type
// terva adds is caught by encodeBlocks' default case, not here.
var blocks = []provider.Content{
	provider.TextBlock{},
	provider.ImageBlock{},
	provider.ToolCallBlock{},
	provider.ToolResultBlock{},
	provider.ReasoningBlock{},
	provider.CompactionBlock{},
}

// fill sets every field of v to a value other than its zero, so a field
// the codec drops shows up as a difference after the round trip. A field
// of a kind fill does not know fails the test: terva added something the
// codec has to be taught.
func fill(t *testing.T, v reflect.Value) {
	t.Helper()
	for i := range v.NumField() {
		f, sf := v.Field(i), v.Type().Field(i)
		switch {
		case f.Kind() == reflect.String:
			f.SetString("value of " + sf.Name)
		case f.Kind() == reflect.Bool:
			f.SetBool(true)
		case f.Type() == reflect.TypeFor[[]byte]() || f.Type() == reflect.TypeFor[json.RawMessage]():
			// Not JSON, which the codec must carry all the same.
			f.SetBytes([]byte("{not json " + sf.Name))
		case f.Type() == reflect.TypeFor[[]provider.Content]():
			f.Set(reflect.ValueOf([]provider.Content{provider.TextBlock{Text: "inner"}}))
		default:
			t.Fatalf("%s.%s is a %s, which the codec does not carry; teach codec.go and this test",
				v.Type().Name(), sf.Name, f.Type())
		}
	}
}

func roundTrip(t *testing.T, m provider.Message) provider.Message {
	t.Helper()
	w, err := encodeMessage(m)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	var back wireMessage
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	got, err := decodeMessage(back)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCodecCarriesEveryField(t *testing.T) {
	for _, b := range blocks {
		v := reflect.New(reflect.TypeOf(b)).Elem()
		fill(t, v)
		m := provider.Message{
			Role:    provider.RoleAssistant,
			Content: []provider.Content{v.Interface().(provider.Content)},
			Time:    time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
			Meta:    map[string]string{"k": "v"},
		}
		if got := roundTrip(t, m); !reflect.DeepEqual(got, m) {
			t.Errorf("%T did not round-trip:\n got %#v\nwant %#v", b, got, m)
		}
	}
}

// TestCodecMessageFields fails when provider.Message gains a field, which
// wireMessage would silently drop.
func TestCodecMessageFields(t *testing.T) {
	var names []string
	for f := range reflect.TypeFor[provider.Message]().Fields() {
		names = append(names, f.Name)
	}
	if want := []string{"Role", "Content", "Time", "Meta"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("provider.Message has fields %v; wireMessage carries %v", names, want)
	}
}

func TestCodecKeepsNilAndEmptyApart(t *testing.T) {
	for _, m := range []provider.Message{
		{Role: provider.RoleUser},
		{Role: provider.RoleUser, Content: []provider.Content{}, Meta: map[string]string{}},
		{Role: provider.RoleUser, Content: []provider.Content{
			provider.ImageBlock{MimeType: "image/png"},
			provider.ImageBlock{MimeType: "image/png", Data: []byte{}},
			provider.ToolCallBlock{ID: "a"},
			provider.ToolCallBlock{ID: "b", Arguments: json.RawMessage{}},
			provider.ToolResultBlock{CallID: "a"},
			provider.ToolResultBlock{CallID: "b", Content: []provider.Content{}},
		}},
	} {
		if got := roundTrip(t, m); !reflect.DeepEqual(got, m) {
			t.Errorf("round trip changed nil or empty:\n got %#v\nwant %#v", got, m)
		}
	}
}

// otherBlock is a content block the codec does not know.
type otherBlock struct{ provider.TextBlock }

func TestCodecRefusesAnUnknownBlock(t *testing.T) {
	if _, err := encodeMessage(provider.Message{Content: []provider.Content{otherBlock{}}}); err == nil {
		t.Fatal("an unknown block was encoded; it would be lost on reload")
	}
	if _, err := decodeMessage(wireMessage{Content: []wireBlock{{Type: "video"}}}); err == nil {
		t.Fatal("an unknown block type was decoded")
	}
}
