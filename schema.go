package airut

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Kind is a parameter's type. The kinds match Discord's application
// command option types one for one, so an adapter maps them directly.
type Kind int

// The parameter kinds.
const (
	KindString Kind = iota + 1
	KindInteger
	KindNumber
	KindBoolean
	KindUser
	KindChannel
	KindRole
	KindAttachment
)

var kindNames = map[Kind]string{
	KindString: "string", KindInteger: "integer", KindNumber: "number",
	KindBoolean: "boolean", KindUser: "user", KindChannel: "channel",
	KindRole: "role", KindAttachment: "attachment",
}

func (k Kind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// Param describes one argument of a command.
type Param struct {
	Name        string
	Description string
	Kind        Kind
	Required    bool
	// Rest marks a string that takes the remainder of a text invocation
	// verbatim. Only the last parameter can be one.
	Rest bool
	// Named marks an optional parameter a text invocation sets only by
	// name, as name=value or as --name for a boolean, so words never bind
	// to it by position. It lets options come before a rest parameter.
	Named bool
	// Choices restricts a string to these values.
	Choices []string
	// Min and Max bound a number's value or a string's length.
	Min, Max *float64
}

// Spec describes a command independently of any platform, for adapters
// to register.
type Spec struct {
	Name        string
	Description string
	Params      []Param
}

// Limits every command keeps, so that one definition registers on every
// platform. They are Discord's, which are the tightest.
const (
	maxNameLen        = 32
	maxDescriptionLen = 100
	maxParams         = 25
	maxChoices        = 25
	maxChoiceLen      = 100
	maxStringLen      = 6000
)

var nameRE = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

func validName(s string) bool { return nameRE.MatchString(s) }

func checkDescription(what, d string) error {
	if n := utf8.RuneCountInString(d); n < 1 || n > maxDescriptionLen {
		return fmt.Errorf("%s needs a description of 1 to %d characters, has %d", what, maxDescriptionLen, n)
	}
	return nil
}

var (
	userType       = reflect.TypeFor[User]()
	channelType    = reflect.TypeFor[Channel]()
	roleType       = reflect.TypeFor[Role]()
	attachmentType = reflect.TypeFor[Attachment]()
)

// kindOf maps a field type to its Kind, reporting whether the field is a
// pointer, which makes it optional.
func kindOf(t reflect.Type) (k Kind, ptr bool, ok bool) {
	if t.Kind() == reflect.Pointer {
		t, ptr = t.Elem(), true
	}
	switch t {
	case userType:
		return KindUser, ptr, true
	case channelType:
		return KindChannel, ptr, true
	case roleType:
		return KindRole, ptr, true
	case attachmentType:
		return KindAttachment, ptr, true
	}
	switch t.Kind() {
	case reflect.String:
		return KindString, ptr, true
	case reflect.Bool:
		return KindBoolean, ptr, true
	case reflect.Int, reflect.Int64:
		return KindInteger, ptr, true
	case reflect.Float64:
		return KindNumber, ptr, true
	}
	return 0, false, false
}

// field ties a Param to the struct field it fills.
type field struct {
	Param
	index int
	ptr   bool
}

// deriveParams reads the parameters from an argument struct's fields and
// tags, and checks them against the limits every platform shares.
func deriveParams(t reflect.Type) ([]field, error) {
	var fields []field
	seen := map[string]bool{}
	optionalSeen := false
	for i := range t.NumField() {
		sf := t.Field(i)
		tag := sf.Tag.Get("airut")
		if tag == "-" || !sf.IsExported() {
			continue
		}
		name, flags, _ := strings.Cut(tag, ",")
		if name == "" {
			name = snakeCase(sf.Name)
		}
		where := fmt.Sprintf("field %s (%s)", sf.Name, name)
		if !validName(name) {
			return nil, fmt.Errorf("%s: a parameter name must match %s", where, nameRE)
		}
		if seen[name] {
			return nil, fmt.Errorf("%s: a second parameter named %s", where, name)
		}
		seen[name] = true
		k, ptr, ok := kindOf(sf.Type)
		if !ok {
			return nil, fmt.Errorf("%s: type %s is not a parameter type", where, sf.Type)
		}
		f := field{Param: Param{Name: name, Description: sf.Tag.Get("desc"), Kind: k, Required: !ptr}, index: i, ptr: ptr}
		for _, fl := range strings.Split(flags, ",") {
			switch fl {
			case "":
			case "optional":
				f.Required = false
			case "rest":
				f.Rest = true
			case "named":
				f.Named = true
			default:
				return nil, fmt.Errorf("%s: unknown airut tag option %q", where, fl)
			}
		}
		if err := checkDescription(where, f.Description); err != nil {
			return nil, err
		}
		if err := tagBounds(&f.Param, sf.Tag, where); err != nil {
			return nil, err
		}
		if f.Rest && k != KindString {
			return nil, fmt.Errorf("%s: only a string can take the rest of the line", where)
		}
		if f.Named && (f.Required || f.Rest) {
			return nil, fmt.Errorf("%s: a named parameter must be optional, and cannot take the rest of the line", where)
		}
		if f.Required && optionalSeen {
			return nil, fmt.Errorf("%s: a required parameter cannot follow an optional one", where)
		}
		optionalSeen = optionalSeen || !f.Required
		fields = append(fields, f)
	}
	if len(fields) > maxParams {
		return nil, fmt.Errorf("%d parameters, more than the %d a command can have", len(fields), maxParams)
	}
	for i, f := range fields {
		if f.Rest && i != len(fields)-1 {
			return nil, fmt.Errorf("parameter %s takes the rest of the line, so it must be the last", f.Name)
		}
	}
	return fields, nil
}

func tagBounds(p *Param, tag reflect.StructTag, where string) error {
	if c, ok := tag.Lookup("choices"); ok {
		if p.Kind != KindString {
			return fmt.Errorf("%s: choices apply only to a string", where)
		}
		for _, v := range strings.Split(c, ",") {
			if v = strings.TrimSpace(v); v == "" {
				continue
			}
			if n := utf8.RuneCountInString(v); n > maxChoiceLen {
				return fmt.Errorf("%s: a choice can be at most %d characters, %q has %d", where, maxChoiceLen, v, n)
			}
			p.Choices = append(p.Choices, v)
		}
		if len(p.Choices) == 0 || len(p.Choices) > maxChoices {
			return fmt.Errorf("%s: choices must list 1 to %d values", where, maxChoices)
		}
	}
	for _, b := range []struct {
		key string
		dst **float64
	}{{"min", &p.Min}, {"max", &p.Max}} {
		s, ok := tag.Lookup(b.key)
		if !ok {
			continue
		}
		switch p.Kind {
		case KindString, KindInteger, KindNumber:
		default:
			return fmt.Errorf("%s: %s applies only to a string or a number", where, b.key)
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("%s: %s=%q is not a number", where, b.key, s)
		}
		switch {
		case p.Kind == KindString && (v < 0 || v > maxStringLen || v != math.Trunc(v)):
			return fmt.Errorf("%s: a string's %s length must be a whole number from 0 to %d", where, b.key, maxStringLen)
		case p.Kind == KindString && b.key == "max" && v < 1:
			return fmt.Errorf("%s: a string's max length must be at least 1", where)
		case math.IsNaN(v) || math.Abs(v) > maxInteger:
			return fmt.Errorf("%s: %s must be a number from %d to %d", where, b.key, -maxInteger, maxInteger)
		case p.Kind == KindInteger && v != math.Trunc(v):
			return fmt.Errorf("%s: an integer's %s must be a whole number", where, b.key)
		}
		*b.dst = &v
	}
	if p.Min != nil && p.Max != nil && *p.Min > *p.Max {
		return fmt.Errorf("%s: min is greater than max", where)
	}
	return nil
}

// snakeCase turns a Go field name such as DiceCount into dice_count.
func snakeCase(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		if unicode.IsUpper(r) {
			if i > 0 && (unicode.IsLower(rs[i-1]) || i+1 < len(rs) && unicode.IsLower(rs[i+1])) {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// usage renders a command's text grammar: required parameters in angle
// brackets, optional ones in square brackets, and a rest parameter with
// an ellipsis.
func usage(prefix string, s Spec) string {
	var b strings.Builder
	b.WriteString(prefix + s.Name)
	for _, p := range s.Params {
		name := p.Name
		switch {
		case p.Rest:
			name += "..."
		case p.Named && p.Kind == KindBoolean:
			name = "--" + name
		case p.Named:
			name += "=..."
		}
		if p.Required {
			b.WriteString(" <" + name + ">")
		} else {
			b.WriteString(" [" + name + "]")
		}
	}
	return b.String()
}

// jsonSchema renders a command's parameters as the JSON Schema an agent
// tool declares. Users, channels, and roles are strings the adapter
// resolves, as it does for text arguments.
func jsonSchema(s Spec) json.RawMessage {
	props := map[string]any{}
	required := []string{}
	for _, p := range s.Params {
		prop := map[string]any{"description": p.Description}
		switch p.Kind {
		case KindString:
			prop["type"] = "string"
			if len(p.Choices) > 0 {
				prop["enum"] = p.Choices
			}
			if p.Min != nil {
				prop["minLength"] = int(*p.Min)
			}
			if p.Max != nil {
				prop["maxLength"] = int(*p.Max)
			}
		case KindInteger, KindNumber:
			prop["type"] = map[Kind]string{KindInteger: "integer", KindNumber: "number"}[p.Kind]
			if p.Min != nil {
				prop["minimum"] = *p.Min
			}
			if p.Max != nil {
				prop["maximum"] = *p.Max
			}
		case KindBoolean:
			prop["type"] = "boolean"
		case KindUser, KindChannel, KindRole:
			prop["type"] = "string"
			prop["description"] = p.Description + " (a " + p.Kind.String() + " id or mention)"
		}
		props[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}
	out, err := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	})
	if err != nil {
		panic(err) // only maps, strings, and numbers: cannot fail
	}
	return out
}
