package players

import (
	"math"
	"strconv"
)

// Helpers for reading decoded JSON (map[string]any) the way the Python mappers read
// dicts: obj.get(key, default), "or" fallbacks and truthiness.

// truthy is Python's bool(v) for decoded JSON values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case []string:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// mapOf returns obj[key] as an object, or an empty one (Python: obj.get(key) or {}).
func mapOf(obj map[string]any, key string) map[string]any {
	if m, ok := obj[key].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// listOf returns obj[key] as a list, or nil (Python: obj.get(key) or []).
func listOf(obj map[string]any, key string) []any {
	l, _ := obj[key].([]any)
	return l
}

// strOf returns obj[key] as text, or def when it is absent or null.
// Non-string values are formatted the way Python's str() would.
func strOf(obj map[string]any, key, def string) string {
	v, ok := obj[key]
	if !ok || v == nil {
		return def
	}
	return PyStr(v)
}

// PyStr formats a decoded JSON value like Python's str().
func PyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1e15 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return jsonText(v)
}

// intOf returns obj[key] as a whole number, or 0 (Python: obj.get(key, 0) on token counts).
func intOf(obj map[string]any, key string) int64 {
	switch x := obj[key].(type) {
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	}
	return 0
}

// orNil maps "" to nil so an unknown id serializes as null, like Python's None.
func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}
