package openrouter

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// keyOrder puts message keys in the order Python builds them, so text that the
// model sees (and sizes) matches json.dumps of the Python dicts. Other keys
// follow alphabetically.
var keyOrder = map[string]int{"role": 0, "tool_call_id": 1, "content": 2, "tool_calls": 3,
	"reasoning_details": 4, "reasoning": 5, "id": 6, "type": 7, "function": 8, "name": 9, "arguments": 10,
	"ok": 11, "output": 12}

func orderedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		oi, iok := keyOrder[keys[i]]
		oj, jok := keyOrder[keys[j]]
		switch {
		case iok && jok:
			return oi < oj
		case iok != jok:
			return iok
		}
		return keys[i] < keys[j]
	})
	return keys
}

// pyDumps writes v like Python's json.dumps(v, ensure_ascii=ascii): ", " and
// ": " separators and only the escapes Python uses.
func pyDumps(v any, ascii bool) string {
	var b strings.Builder
	writePy(&b, v, ascii)
	return b.String()
}

func writePy(b *strings.Builder, v any, ascii bool) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		writePyString(b, x, ascii)
	case json.Number:
		b.WriteString(string(x))
	case float64:
		b.WriteString(pyNumber(x))
	case float32:
		b.WriteString(pyNumber(float64(x)))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case map[string]any:
		b.WriteByte('{')
		for i, k := range orderedKeys(x) {
			if i > 0 {
				b.WriteString(", ")
			}
			writePyString(b, k, ascii)
			b.WriteString(": ")
			writePy(b, x[k], ascii)
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteString(", ")
			}
			writePy(b, item, ascii)
		}
		b.WriteByte(']')
	case []map[string]any:
		items := make([]any, len(x))
		for i, m := range x {
			items[i] = m
		}
		writePy(b, items, ascii)
	default:
		raw, err := json.Marshal(x)
		if err != nil {
			b.WriteString("null")
			return
		}
		var generic any
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if dec.Decode(&generic) != nil {
			b.WriteString("null")
			return
		}
		writePy(b, generic, ascii)
	}
}

func pyNumber(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e16 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func writePyString(b *strings.Builder, s string, ascii bool) {
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(b, `\u%04x`, r)
			case ascii && r >= 0x7f:
				if r > 0xFFFF {
					hi, lo := utf16.EncodeRune(r)
					fmt.Fprintf(b, `\u%04x\u%04x`, hi, lo)
				} else {
					fmt.Fprintf(b, `\u%04x`, r)
				}
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// runeLen is Python's len() of a str.
func runeLen(s string) int { return utf8.RuneCountInString(s) }
