package settings

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// FormatValue renders a resolved value as a one-line TOML value: tables and
// secret references as inline tables, so the output can be pasted back into
// a file or a --set. A secret renders as its reference, never its value.
func FormatValue(v any) string {
	switch x := v.(type) {
	case string:
		return quote(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	case []string:
		return formatList(x, quote)
	case []int:
		return formatList(x, strconv.Itoa)
	case [][]string:
		return formatList(x, func(argv []string) string { return formatList(argv, quote) })
	case []any:
		return formatList(x, FormatValue)
	case SecretRef:
		table := map[string]any{}
		if x.Env != "" {
			table["env"] = x.Env
		}
		if x.Op != "" {
			table["op"] = x.Op
		}
		return FormatValue(table)
	case map[string]any:
		if len(x) == 0 {
			return "{}"
		}
		parts := make([]string, 0, len(x))
		for _, k := range slices.Sorted(maps.Keys(x)) {
			key := k
			if !bareKey.MatchString(k) {
				key = quote(k)
			}
			parts = append(parts, key+" = "+FormatValue(x[k]))
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	default:
		return fmt.Sprint(x)
	}
}

func formatList[T any](items []T, format func(T) string) string {
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = format(item)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// quote renders s as a TOML basic string. strconv.Quote is not one: it emits
// \x.. and \a escapes TOML does not have.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
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
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
