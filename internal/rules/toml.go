package rules

import (
	"fmt"
	"strconv"
	"strings"
)

// parseTOML reads the small TOML subset tests/rules.toml uses, because ccsbx has zero third-party
// modules. Supported: `[[name]]` array-of-tables headers, `key = "string"`, `key = 123`,
// `key = true|false`, one-line arrays of strings or ints, and `#` comments. Anything else is an
// error, so a typo can't silently drop a field.
func parseTOML(src string) (map[string][]map[string]any, error) {
	out := map[string][]map[string]any{}
	var cur map[string]any
	for n, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[[") && strings.HasSuffix(line, "]]") {
			name := strings.TrimSpace(line[2 : len(line)-2])
			cur = map[string]any{}
			out[name] = append(out[name], cur)
			continue
		}
		if strings.HasPrefix(line, "[") {
			return nil, fmt.Errorf("line %d: only [[tables]] are supported", n+1)
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key = value", n+1)
		}
		if cur == nil {
			return nil, fmt.Errorf("line %d: key outside a [[table]]", n+1)
		}
		key := strings.TrimSpace(k)
		val, err := parseValue(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("line %d: %s: %w", n+1, key, err)
		}
		if _, dup := cur[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", n+1, key)
		}
		cur[key] = val
	}
	return out, nil
}

// stripComment removes a trailing # comment that is not inside a string.
func stripComment(s string) string {
	in := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if in {
				i++
			}
		case '"':
			in = !in
		case '#':
			if !in {
				return s[:i]
			}
		}
	}
	return s
}

func parseValue(v string) (any, error) {
	switch {
	case v == "true":
		return true, nil
	case v == "false":
		return false, nil
	case strings.HasPrefix(v, `"`):
		s, err := strconv.Unquote(v)
		if err != nil {
			return nil, fmt.Errorf("bad string %s", v)
		}
		return s, nil
	case strings.HasPrefix(v, "["):
		if !strings.HasSuffix(v, "]") {
			return nil, fmt.Errorf("arrays must be on one line")
		}
		var items []any
		for _, p := range splitTop(v[1 : len(v)-1]) {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			it, err := parseValue(p)
			if err != nil {
				return nil, err
			}
			items = append(items, it)
		}
		return items, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n, nil
	}
	return nil, fmt.Errorf("unsupported value %q", v)
}

// splitTop splits on commas outside strings.
func splitTop(s string) []string {
	var parts []string
	in, start := false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if in {
				i++
			}
		case '"':
			in = !in
		case ',':
			if !in {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}
