package airut

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ArgError is a problem with the arguments of one invocation. Its text is
// shown to the caller, so it never names more than the parameter.
type ArgError struct {
	Param string
	Msg   string
}

func (e *ArgError) Error() string {
	if e.Param == "" {
		return e.Msg
	}
	return e.Param + ": " + e.Msg
}

// resolver turns a text token into a User, Channel, or Role.
type resolver func(ctx context.Context, kind Kind, token string) (any, error)

// coerce converts one raw value to the Go value of kind k. A raw value is
// a string from text, a JSON value from a tool call, or a typed value
// from a platform that types its arguments.
func coerce(ctx context.Context, p *field, raw any, resolve resolver) (any, error) {
	bad := func(want string) error {
		return &ArgError{Param: p.Name, Msg: "must be " + want}
	}
	switch p.Kind {
	case KindString:
		if s, ok := raw.(string); ok {
			return s, nil
		}
		return nil, bad("text")
	case KindBoolean:
		switch v := raw.(type) {
		case bool:
			return v, nil
		case string:
			switch strings.ToLower(v) {
			case "true", "yes", "on", "1":
				return true, nil
			case "false", "no", "off", "0":
				return false, nil
			}
		}
		return nil, bad("true or false")
	case KindInteger:
		n, err := toInteger(raw)
		if err != nil {
			return nil, &ArgError{Param: p.Name, Msg: err.Error()}
		}
		return n, nil
	case KindNumber:
		var f float64
		switch v := raw.(type) {
		case int64:
			f = float64(v)
		case int:
			f = float64(v)
		case float64:
			f = v
		case json.Number:
			var err error
			if f, err = v.Float64(); err != nil {
				return nil, bad("a number")
			}
		case string:
			var err error
			if f, err = strconv.ParseFloat(v, 64); err != nil {
				return nil, bad("a number")
			}
		default:
			return nil, bad("a number")
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, bad("a number")
		}
		return f, nil
	case KindUser, KindChannel, KindRole:
		switch v := raw.(type) {
		case User:
			if p.Kind == KindUser {
				return v, nil
			}
		case Channel:
			if p.Kind == KindChannel {
				return v, nil
			}
		case Role:
			if p.Kind == KindRole {
				return v, nil
			}
		case string:
			if resolve == nil {
				return nil, bad("a " + p.Kind.String())
			}
			got, err := resolve(ctx, p.Kind, v)
			if err != nil || !entityOfKind(got, p.Kind) {
				return nil, &ArgError{Param: p.Name, Msg: "is not a " + p.Kind.String() + " here"}
			}
			return got, nil
		}
		return nil, bad("a " + p.Kind.String())
	case KindAttachment:
		if a, ok := raw.(Attachment); ok {
			return a, nil
		}
		return nil, bad("an attachment")
	}
	return nil, bad(p.Kind.String())
}

// maxInteger is the largest magnitude an integer parameter takes, the
// limit Discord sets and the largest a float64 holds exactly.
const maxInteger = 1 << 53

var errWhole = errors.New("must be a whole number")

// toInteger reads a whole number without passing it through float64,
// which would round values past 2^53 to a neighbour that fits.
func toInteger(raw any) (int64, error) {
	var n int64
	switch v := raw.(type) {
	case int64:
		n = v
	case int:
		n = int64(v)
	case float64:
		if v != math.Trunc(v) || math.Abs(v) > maxInteger {
			return 0, errWhole
		}
		n = int64(v)
	case json.Number:
		return toInteger(string(v))
	case string:
		var err error
		if n, err = strconv.ParseInt(v, 10, 64); err != nil {
			if f, ferr := strconv.ParseFloat(v, 64); ferr == nil && !math.IsNaN(f) {
				return 0, errWhole
			}
			return 0, errors.New("must be a number")
		}
	default:
		return 0, errors.New("must be a number")
	}
	if n > maxInteger || n < -maxInteger {
		return 0, fmt.Errorf("must be between %d and %d", -maxInteger, maxInteger)
	}
	return n, nil
}

func entityOfKind(v any, k Kind) bool {
	switch v.(type) {
	case User:
		return k == KindUser
	case Channel:
		return k == KindChannel
	case Role:
		return k == KindRole
	}
	return false
}

// check enforces a parameter's choices and bounds on a coerced value.
func check(p *field, v any) error {
	switch v := v.(type) {
	case string:
		if len(p.Choices) > 0 && !slices.Contains(p.Choices, v) {
			return &ArgError{Param: p.Name, Msg: "must be one of " + strings.Join(p.Choices, ", ")}
		}
		n := float64(utf8.RuneCountInString(v))
		if p.Min != nil && n < *p.Min {
			return &ArgError{Param: p.Name, Msg: fmt.Sprintf("must be at least %g characters", *p.Min)}
		}
		if p.Max != nil && n > *p.Max {
			return &ArgError{Param: p.Name, Msg: fmt.Sprintf("must be at most %g characters", *p.Max)}
		}
	case int64, float64:
		f := reflect.ValueOf(v).Convert(reflect.TypeFor[float64]()).Float()
		if p.Min != nil && f < *p.Min {
			return &ArgError{Param: p.Name, Msg: fmt.Sprintf("must be at least %g", *p.Min)}
		}
		if p.Max != nil && f > *p.Max {
			return &ArgError{Param: p.Name, Msg: fmt.Sprintf("must be at most %g", *p.Max)}
		}
	}
	return nil
}

// bind builds an argument struct of type t from raw values keyed by
// parameter name, and files bound in order to attachment parameters.
func bind(ctx context.Context, t reflect.Type, params []field, raw map[string]any, files []Attachment, resolve resolver) (reflect.Value, error) {
	out := reflect.New(t).Elem()
	for name := range raw {
		if !slices.ContainsFunc(params, func(f field) bool { return f.Name == name }) {
			return reflect.Value{}, &ArgError{Param: name, Msg: "is not a parameter of this command"}
		}
	}
	for i := range params {
		p := &params[i]
		v, ok := raw[p.Name]
		if !ok && p.Kind == KindAttachment && len(files) > 0 {
			v, ok, files = files[0], true, files[1:]
		}
		if !ok || v == nil {
			if p.Required {
				return reflect.Value{}, &ArgError{Param: p.Name, Msg: "is required"}
			}
			continue
		}
		got, err := coerce(ctx, p, v, resolve)
		if err != nil {
			return reflect.Value{}, err
		}
		if err := check(p, got); err != nil {
			return reflect.Value{}, err
		}
		dst := out.Field(p.index)
		gv := reflect.ValueOf(got)
		if p.ptr {
			ptr := reflect.New(dst.Type().Elem())
			ptr.Elem().Set(gv.Convert(dst.Type().Elem()))
			dst.Set(ptr)
		} else {
			dst.Set(gv.Convert(dst.Type()))
		}
	}
	return out, nil
}

// decodeJSON reads a tool call's arguments into raw values.
func decodeJSON(args json.RawMessage) (map[string]any, error) {
	raw := map[string]any{}
	if len(strings.TrimSpace(string(args))) == 0 {
		return raw, nil
	}
	notObject := &ArgError{Msg: "the arguments are not a JSON object"}
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.UseNumber()
	// A null decodes into a nil map; it is not an object either.
	if err := dec.Decode(&raw); err != nil || raw == nil {
		return nil, notObject
	}
	// One object and nothing after it, so a malformed payload never runs
	// the command on its first part.
	if _, err := dec.Token(); err != io.EOF {
		return nil, notObject
	}
	return raw, nil
}
