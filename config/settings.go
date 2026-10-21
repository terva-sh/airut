package config

import (
	"sort"
	"strconv"
)

// buildSettings flattens the [settings] table into values by dotted
// name. A setting is a whole number, true or false, or a string, which
// is how a duration or an enum is written. A secret reference is
// refused, because a setting's value is shown in chat.
func buildSettings(p *problems, raw any) map[string]string {
	table, ok := raw.(map[string]any)
	if !ok {
		p.add("settings", "the value is not a table")
		return nil
	}
	out := map[string]string{}
	flattenSettings(p, "", table, out)
	return out
}

func flattenSettings(p *problems, prefix string, table map[string]any, out map[string]string) {
	keys := make([]string, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name := k
		if prefix != "" {
			name = prefix + "." + k
		}
		key := "settings." + name
		switch v := table[k].(type) {
		case string:
			out[name] = v
		case int64:
			out[name] = strconv.FormatInt(v, 10)
		case bool:
			out[name] = strconv.FormatBool(v)
		case map[string]any:
			if isSecretRef(v) {
				p.add(key, "a setting never holds a secret: its value is shown in chat")
				continue
			}
			flattenSettings(p, name, v, out)
		default:
			p.add(key, `a setting is a whole number, true or false, or a string, as a duration is written: "10m"`)
		}
	}
}

// isSecretRef reports whether t has the shape of a secret reference.
func isSecretRef(t map[string]any) bool {
	if len(t) != 1 {
		return false
	}
	for k := range t {
		return k == "env" || k == "credential" || k == "file"
	}
	return false
}
