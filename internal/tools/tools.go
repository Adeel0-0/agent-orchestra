// Package tools holds the small project-scoped tools used by API players
// (orchestra/tools). Every accessed path is resolved and confined to the project;
// commands run with a deadline and bounded output but are NOT an OS sandbox.
package tools

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// OutputLimit caps text returned to the model, in characters (code points).
	OutputLimit = 16000
	// FileLimit caps files read or written by the text tools, in bytes.
	FileLimit = 1024 * 1024
	// DefaultScanLimit bounds the files glob and grep visit.
	DefaultScanLimit = 10000
)

// Result is one tool outcome. Path and Kind are set when a file changed.
type Result struct {
	Output string
	OK     bool
	Path   string
	Kind   string // "add" or "update"
}

// ToolError is a tool failure that is reported to the model as a failed result
// (Python's ValueError from the tools).
type ToolError struct{ Message string }

func (e *ToolError) Error() string { return e.Message }

func toolErr(format string, args ...any) error {
	return &ToolError{Message: fmt.Sprintf(format, args...)}
}

// Bounded cuts text to limit characters and marks the cut.
func Bounded(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return firstRunes(text, limit) + "\n[output truncated]"
}

func firstRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// StringArg returns args[name] (or def when absent and def is non-nil) as a string.
func StringArg(args map[string]any, name string, def *string) (string, error) {
	v, ok := args[name]
	if !ok && def != nil {
		return *def, nil
	}
	s, isString := v.(string)
	if !ok || !isString {
		return "", toolErr("%s must be a string", name)
	}
	return s, nil
}

// intArg reports whether v is a JSON integer (Python's `type(v) is int`):
// json.Number written without '.', 'e' or 'E', or a Go integer. Floats and
// booleans are not integers. Values too large for int saturate.
func intArg(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		s := string(n)
		if strings.ContainsAny(s, ".eE") {
			return 0, false
		}
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
				if strings.HasPrefix(s, "-") {
					return math.MinInt, true
				}
				return math.MaxInt, true
			}
			return 0, false
		}
		return int(i), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case int32:
		return int(n), true
	}
	return 0, false
}

// numberArg accepts any JSON number or Go number (not bool or string). The second
// result is Python's repr of the value, used in messages.
func numberArg(v any) (float64, string, bool) {
	switch n := v.(type) {
	case json.Number:
		s := string(n)
		f, err := strconv.ParseFloat(s, 64)
		if err != nil && !isRangeErr(err) {
			return 0, "", false
		}
		if strings.ContainsAny(s, ".eE") {
			return f, pyFloatRepr(f), true
		}
		return f, strings.TrimPrefix(s, "+"), true
	case float64:
		return n, pyFloatRepr(n), true
	case float32:
		return float64(n), pyFloatRepr(float64(n)), true
	case int:
		return float64(n), strconv.Itoa(n), true
	case int64:
		return float64(n), strconv.FormatInt(n, 10), true
	}
	return 0, "", false
}

func isRangeErr(err error) bool {
	ne, ok := err.(*strconv.NumError)
	return ok && ne.Err == strconv.ErrRange
}

// pyFloatRepr formats f like Python's repr(float) for ordinary magnitudes.
func pyFloatRepr(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	case math.IsNaN(f):
		return "nan"
	}
	abs := math.Abs(f)
	if abs != 0 && (abs < 1e-4 || abs >= 1e16) {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		// Python writes 1e-05 and 1e+16: at least two exponent digits.
		mant, exp, _ := strings.Cut(s, "e")
		sign := exp[:1]
		digits := exp[1:]
		if len(digits) < 2 {
			digits = "0" + digits
		}
		return mant + "e" + sign + digits
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}
