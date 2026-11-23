package app

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// pyDumps encodes v like Python's json.dumps(v, ensure_ascii=False): ", " and ": "
// separators and no HTML escaping. Map keys are sorted (Go maps have no order).
func pyDumps(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return pySpacing(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// pySpacing adds a space after every structural ',' and ':' outside strings.
func pySpacing(compact []byte) []byte {
	out := make([]byte, 0, len(compact)+len(compact)/8)
	inString, escaped := false, false
	for _, c := range compact {
		out = append(out, c)
		switch {
		case inString && escaped:
			escaped = false
		case inString && c == '\\':
			escaped = true
		case c == '"':
			inString = !inString
		case !inString && (c == ',' || c == ':'):
			out = append(out, ' ')
		}
	}
	return out
}

// encodeRecord builds a journal payload the way Python's Store.append does:
// {**event, "seq": seq, "timestamp": ts}. "type" comes first (Event.to_dict order),
// other keys follow sorted, then seq and timestamp.
func encodeRecord(event map[string]any, seq int64, ts float64) (string, error) {
	keys := make([]string, 0, len(event))
	for k := range event {
		if k != "type" && k != "seq" && k != "timestamp" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if _, ok := event["type"]; ok {
		keys = append([]string{"type"}, keys...)
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for _, k := range keys {
		if err := writeMember(&b, k, event[k]); err != nil {
			return "", err
		}
		b.WriteString(", ")
	}
	if err := writeMember(&b, "seq", seq); err != nil {
		return "", err
	}
	b.WriteString(", ")
	if err := writeMember(&b, "timestamp", ts); err != nil {
		return "", err
	}
	b.WriteByte('}')
	return b.String(), nil
}

func writeMember(b *bytes.Buffer, key string, value any) error {
	k, err := pyDumps(key)
	if err != nil {
		return err
	}
	v, err := pyDumps(value)
	if err != nil {
		return err
	}
	b.Write(k)
	b.WriteString(": ")
	b.Write(v)
	return nil
}

// orderedObject is a JSON object that keeps its key order (Python dict order).
type orderedObject []member

type member struct {
	Key   string
	Value any
}

func (o orderedObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(m.Key)
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(m.Value)
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// isPySpace matches Python's str.isspace for the characters str.strip removes.
func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// pyStrip is Python's str.strip().
func pyStrip(s string) string { return strings.TrimFunc(s, isPySpace) }

// pyLen is Python's len(str): the number of code points.
func pyLen(s string) int { return utf8.RuneCountInString(s) }

// firstLine is Python's s.splitlines()[0] (s must be non-empty after strip).
func firstLine(s string) string {
	for i, r := range s {
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			return s[:i]
		}
	}
	return s
}

// truncateRunes is Python's s[:n].
func truncateRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// decodeReplace is bytes.decode('utf-8', errors='replace').
func decodeReplace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size <= 1 {
			sb.WriteRune(utf8.RuneError)
			b = b[1:]
			continue
		}
		sb.Write(b[:size])
		b = b[size:]
	}
	return sb.String()
}
