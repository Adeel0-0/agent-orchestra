package players

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// DumpsJSON encodes v like Python's json.dumps(v, ensure_ascii=False): ", " and ": "
// separators, non-ASCII kept as is, no HTML escaping. Object keys come out sorted
// (Go maps have no insertion order); readers parse the JSON, so only the look differs.
func DumpsJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return string(spaceSeparators(bytes.TrimRight(buf.Bytes(), "\n")))
}

func jsonText(v any) string { return DumpsJSON(v) }

// spaceSeparators adds a space after every structural ',' and ':' outside strings.
func spaceSeparators(compact []byte) []byte {
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
