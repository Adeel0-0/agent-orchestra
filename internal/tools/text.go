package tools

import (
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

// isLineBreak matches the characters Python's str.splitlines splits on.
func isLineBreak(r rune) bool {
	switch r {
	case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// splitLines is Python's str.splitlines(keepends): "\r\n" is one break and a
// trailing break does not start an empty line.
func splitLines(s string, keepEnds bool) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); {
		r, size := rune(s[i]), 1
		if r >= 0x80 {
			r, size = utf8.DecodeRuneInString(s[i:])
		}
		if !isLineBreak(r) {
			i += size
			continue
		}
		end := i + size
		if r == '\r' && end < len(s) && s[end] == '\n' {
			end++
		}
		if keepEnds {
			lines = append(lines, s[start:end])
		} else {
			lines = append(lines, s[start:i])
		}
		start, i = end, end
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// fnmatchCase is Python's fnmatch.fnmatchcase: '*' any run, '?' one character,
// '[seq]' and '[!seq]' sets; backslash is an ordinary character.
func fnmatchCase(name, pattern string) bool {
	re := compiledPattern(pattern)
	return re.MatchString(name)
}

var (
	patternMu    sync.Mutex
	patternCache = map[string]*regexp.Regexp{}
)

func compiledPattern(pattern string) *regexp.Regexp {
	patternMu.Lock()
	defer patternMu.Unlock()
	if re, ok := patternCache[pattern]; ok {
		return re
	}
	re, err := regexp.Compile(translateGlob(pattern))
	if err != nil {
		re = regexp.MustCompile(`^(?s:` + regexp.QuoteMeta(pattern) + `)$`)
	}
	if len(patternCache) > 256 {
		patternCache = map[string]*regexp.Regexp{}
	}
	patternCache[pattern] = re
	return re
}

// neverMatch is an RE2 class that matches no character (Python uses (?!)).
const neverMatch = `[^\x00-\x{10FFFF}]`

// translateGlob ports fnmatch.translate to RE2 syntax.
func translateGlob(pattern string) string {
	pat := []rune(pattern)
	n := len(pat)
	var out strings.Builder
	out.WriteString(`^(?s:`)
	for i := 0; i < n; {
		c := pat[i]
		i++
		switch c {
		case '*':
			for i < n && pat[i] == '*' {
				i++
			}
			out.WriteString(`.*`)
		case '?':
			out.WriteString(`.`)
		case '[':
			j := i
			if j < n && pat[j] == '!' {
				j++
			}
			if j < n && pat[j] == ']' {
				j++
			}
			for j < n && pat[j] != ']' {
				j++
			}
			if j >= n {
				out.WriteString(`\[`)
				continue
			}
			out.WriteString(globSet(pat, i, j))
			i = j + 1
		default:
			out.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	out.WriteString(`)$`)
	return out.String()
}

// globSet translates pat[i:j], the inside of a [...] set.
func globSet(pat []rune, i, j int) string {
	stuff := string(pat[i:j])
	if !strings.Contains(stuff, "-") {
		stuff = escapeSetChunk(stuff, false)
	} else {
		var chunks []string
		k := i + 1
		if pat[i] == '!' {
			k = i + 2
		}
		for {
			idx := indexRune(pat, '-', k, j)
			if idx < 0 {
				break
			}
			chunks = append(chunks, string(pat[i:idx]))
			i = idx + 1
			k = idx + 3
		}
		if chunk := string(pat[i:j]); chunk != "" {
			chunks = append(chunks, chunk)
		} else {
			chunks[len(chunks)-1] += "-"
		}
		for k := len(chunks) - 1; k > 0; k-- {
			prev, cur := []rune(chunks[k-1]), []rune(chunks[k])
			if len(prev) > 0 && len(cur) > 0 && prev[len(prev)-1] > cur[0] {
				chunks[k-1] = string(prev[:len(prev)-1]) + string(cur[1:])
				chunks = append(chunks[:k], chunks[k+1:]...)
			}
		}
		escaped := make([]string, len(chunks))
		for idx, s := range chunks {
			escaped[idx] = escapeSetChunk(s, true)
		}
		stuff = strings.Join(escaped, "-")
	}
	if stuff == "" {
		return neverMatch
	}
	if stuff == "!" {
		return `.`
	}
	switch stuff[0] {
	case '!':
		stuff = "^" + stuff[1:]
	case '^':
		stuff = `\` + stuff
	}
	return "[" + stuff + "]"
}

// escapeSetChunk escapes characters RE2 would treat specially inside a set.
func escapeSetChunk(s string, hyphen bool) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\', r == '[', r == ']', r == '&', r == '~', r == '|':
			b.WriteByte('\\')
		case hyphen && r == '-':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func indexRune(pat []rune, r rune, from, to int) int {
	for i := from; i < to && i < len(pat); i++ {
		if pat[i] == r {
			return i
		}
	}
	return -1
}
