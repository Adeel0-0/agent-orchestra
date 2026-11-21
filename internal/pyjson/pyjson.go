// Package pyjson writes JSON the way Python's json.dumps does by default:
// ", " and ": " separators, non-ASCII escaped as \uXXXX (ensure_ascii), and
// no HTML escaping. Python and Go then append identical-looking lines to the
// same events.jsonl. Readers on both sides parse JSON, so this is cosmetic
// compatibility, not a correctness requirement.
package pyjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// Marshal encodes v like json.dumps(v).
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return pythonize(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// MarshalIndent encodes v like json.dumps(v, indent=2): one item per line,
// "," at line ends and ": " after keys.
func MarshalIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return asciiOnly(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// pythonize adds a space after every structural ',' and ':' outside strings
// and escapes non-ASCII characters.
func pythonize(compact []byte) []byte {
	out := make([]byte, 0, len(compact)+len(compact)/8)
	inString, escaped := false, false
	for i := 0; i < len(compact); {
		c := compact[i]
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRune(compact[i:])
			out = appendEscapedRune(out, r)
			i += size
			continue
		}
		out = append(out, c)
		i++
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

func asciiOnly(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] < utf8.RuneSelf {
			out = append(out, b[i])
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		out = appendEscapedRune(out, r)
		i += size
	}
	return out
}

// Non-ASCII only ever appears inside JSON strings, so escaping it there is
// always valid. Characters outside the BMP become a surrogate pair, as in Python.
func appendEscapedRune(out []byte, r rune) []byte {
	if r == utf8.RuneError {
		return append(out, `�`...)
	}
	if r > 0xFFFF {
		hi, lo := utf16.EncodeRune(r)
		return append(out, fmt.Sprintf(`\u%04x\u%04x`, hi, lo)...)
	}
	return append(out, fmt.Sprintf(`\u%04x`, r)...)
}
