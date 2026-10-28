// Package yamlw writes the small, ordered YAML documents the CLI generates
// (the Compose override). Strings are always double-quoted with JSON-style
// escapes, which YAML reads identically, so no value can change type.
package yamlw

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// KV is one mapping entry.
type KV struct {
	K string
	V any
}

// Map keeps key order.
type Map []KV

// List is a sequence.
type List []any

// Marshal renders v (a Map) as a YAML document.
func Marshal(v Map) []byte {
	var b bytes.Buffer
	writeMap(&b, v, 0)
	return b.Bytes()
}

func pad(n int) string { return strings.Repeat(" ", n) }

func scalar(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return strconv.Quote(x), true
	case bool:
		return strconv.FormatBool(x), true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case uint32:
		return strconv.FormatUint(uint64(x), 10), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case nil:
		return "null", true
	}
	return "", false
}

func key(k string) string {
	for _, r := range k {
		if r != '_' && r != '-' && r != '.' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return strconv.Quote(k)
		}
	}
	return k
}

func writeMap(b *bytes.Buffer, m Map, indent int) {
	for _, kv := range m {
		b.WriteString(pad(indent) + key(kv.K) + ":")
		writeValue(b, kv.V, indent)
	}
}

func writeValue(b *bytes.Buffer, v any, indent int) {
	if s, ok := scalar(v); ok {
		b.WriteString(" " + s + "\n")
		return
	}
	switch x := v.(type) {
	case Map:
		if len(x) == 0 {
			b.WriteString(" {}\n")
			return
		}
		b.WriteString("\n")
		writeMap(b, x, indent+2)
	case List:
		if len(x) == 0 {
			b.WriteString(" []\n")
			return
		}
		b.WriteString("\n")
		writeList(b, x, indent+2)
	case []string:
		l := make(List, len(x))
		for i, s := range x {
			l[i] = s
		}
		writeValue(b, l, indent)
	default:
		panic(fmt.Sprintf("yamlw: unsupported type %T", v))
	}
}

func writeList(b *bytes.Buffer, l List, indent int) {
	for _, item := range l {
		if s, ok := scalar(item); ok {
			b.WriteString(pad(indent) + "- " + s + "\n")
			continue
		}
		m, ok := item.(Map)
		if !ok || len(m) == 0 {
			panic(fmt.Sprintf("yamlw: unsupported list item %T", item))
		}
		// First entry shares the dash line; the rest align under it.
		var first bytes.Buffer
		writeMap(&first, m[:1], 0)
		b.WriteString(pad(indent) + "- " + reindent(first.String(), indent+2))
		writeMap(b, m[1:], indent+2)
	}
}

// reindent shifts continuation lines of s by n spaces.
func reindent(s string, n int) string {
	lines := strings.SplitAfter(s, "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i] != "" {
			lines[i] = pad(n) + lines[i]
		}
	}
	return strings.Join(lines, "")
}
