package config

import (
	"bytes"
	"encoding"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// LoadApp reads the config file at path as Load does, and decodes its
// [app] table, the bot's own settings, into app: a pointer to a struct
// whose fields carry toml tags, as in `toml:"main_room"`. Nested tables,
// arrays, and maps decode into nested structs, slices or arrays, and
// maps.
//
// A field of type Secret takes a secret reference, as airut's own
// secrets do: { env = "NAME" }, { credential = "NAME" },
// { file = "PATH" }, or { sealed = "enc:age:v2:..." }. A sealed value
// is bound to its own key path under app, such as app.booru.token, and
// opens nowhere else. A plain string there is refused, because secrets
// are never in the file as plaintext. A key the struct does not name, or a value of
// the wrong type, is a problem, reported with every other problem in the
// file. A file without an [app] table leaves app as it was.
func LoadApp(path string, app any) (*Config, error) {
	v := reflect.ValueOf(app)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return nil, errors.New("config: LoadApp needs a pointer to a struct")
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return parse(path, src, v.Elem())
}

var (
	secretType          = reflect.TypeFor[Secret]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// decodeTable decodes raw, the table at key, into the struct v.
func decodeTable(p *problems, key string, raw map[string]any, v reflect.Value, src *secretSource) {
	t := v.Type()
	fields := map[string]int{}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		fields[name] = i
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		i, ok := fields[k]
		if !ok {
			p.add(childKey(key, k), "unknown key")
			continue
		}
		decodeValue(p, childKey(key, k), raw[k], v.Field(i), src)
	}
}

// decodeValue decodes raw, the value at key, into v.
func decodeValue(p *problems, key string, raw any, v reflect.Value, src *secretSource) {
	t := v.Type()
	switch {
	case t == secretType:
		v.Set(reflect.ValueOf(resolveSecret(p, key, raw, src)))
	case t.Kind() == reflect.Pointer:
		nv := reflect.New(t.Elem())
		decodeValue(p, key, raw, nv.Elem(), src)
		v.Set(nv)
	case t.Kind() == reflect.Struct && !reflect.PointerTo(t).Implements(textUnmarshalerType):
		m, ok := raw.(map[string]any)
		if !ok {
			p.add(key, "the value is not a table")
			return
		}
		decodeTable(p, key, m, v, src)
	case t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8:
		list, ok := raw.([]any)
		if !ok {
			p.add(key, "the value is not an array")
			return
		}
		s := reflect.MakeSlice(t, len(list), len(list))
		for i, item := range list {
			decodeValue(p, indexKey(key, i), item, s.Index(i), src)
		}
		v.Set(s)
	case t.Kind() == reflect.Array:
		list, ok := raw.([]any)
		if !ok {
			p.add(key, "the value is not an array")
			return
		}
		if len(list) > t.Len() {
			p.addf(key, "the array holds at most %d values", t.Len())
			return
		}
		for i, item := range list {
			decodeValue(p, indexKey(key, i), item, v.Index(i), src)
		}
	case t.Kind() == reflect.Map && t.Key().Kind() == reflect.String:
		m, ok := raw.(map[string]any)
		if !ok {
			p.add(key, "the value is not a table")
			return
		}
		out := reflect.MakeMapWithSize(t, len(m))
		for k, item := range m {
			ev := reflect.New(t.Elem()).Elem()
			decodeValue(p, childKey(key, k), item, ev, src)
			out.SetMapIndex(reflect.ValueOf(k).Convert(t.Key()), ev)
		}
		v.Set(out)
	default:
		decodeLeaf(p, key, raw, v)
	}
}

// decodeLeaf decodes a scalar through the TOML decoder itself, so a
// value converts exactly as it would in a struct go-toml decodes.
func decodeLeaf(p *problems, key string, raw any, v reflect.Value) {
	src, err := toml.Marshal(map[string]any{"v": raw})
	holder := reflect.New(reflect.StructOf([]reflect.StructField{{Name: "V", Type: v.Type(), Tag: `toml:"v"`}}))
	if err == nil {
		// Strict, so that no type reaching here can take a table with
		// keys its fields do not name.
		err = toml.NewDecoder(bytes.NewReader(src)).DisallowUnknownFields().Decode(holder.Interface())
	}
	if err != nil {
		p.add(key, "the value is not "+kindName(v.Type()))
		return
	}
	v.Set(holder.Elem().Field(0))
}

func kindName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "text"
	case reflect.Bool:
		return "true or false"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "a whole number in range"
	case reflect.Float32, reflect.Float64:
		return "a number"
	}
	return "one this key accepts"
}
