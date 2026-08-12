package gate

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// validRepoPath accepts the normalized slash-separated names emitted by a
// trusted Git diff reader. It intentionally rejects names that could be
// interpreted differently by a verifier on another operating system.
func validRepoPath(value string) bool {
	if value == "" || len(value) > MaxPathBytes || !utf8.ValidString(value) {
		return false
	}
	if strings.IndexByte(value, 0) >= 0 || strings.ContainsRune(value, '\\') {
		return false
	}
	if strings.HasPrefix(value, "/") || (len(value) >= 2 && isASCIIAlpha(value[0]) && value[1] == ':') {
		return false
	}
	if strings.HasSuffix(value, "/") || strings.Contains(value, "//") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

// ValidateRepoPath exposes the same fail-closed path contract used by the
// engine for callers that normalize a diff before constructing observations.
func ValidateRepoPath(value string) bool { return validRepoPath(value) }

// MatchGlob performs a Git-compatible, repository-relative glob match. It
// returns false for an invalid pattern or path. Supported syntax is the Git
// wildmatch subset needed for scope policies: *, ?, bracket classes, and
// component-aware **. Backslash escapes are deliberately not supported so a
// policy cannot smuggle an OS path separator into a supposedly normalized
// pattern.
func MatchGlob(pattern, value string) bool {
	return matchGlob(pattern, value)
}

func matchGlob(pattern, value string) bool {
	if !validRepoPath(value) {
		return false
	}
	compiled, err := compileGlob(pattern)
	if err != nil {
		return false
	}
	return compiled.match([]rune(value))
}

type compiledGlob struct {
	pattern []rune
}

func compileGlob(pattern string) (compiledGlob, error) {
	if pattern == "" || len(pattern) > MaxPatternBytes || !utf8.ValidString(pattern) {
		return compiledGlob{}, fmt.Errorf("invalid scope glob")
	}
	if strings.IndexByte(pattern, 0) >= 0 || strings.ContainsRune(pattern, '\\') {
		return compiledGlob{}, fmt.Errorf("scope glob contains a forbidden character")
	}
	if strings.HasPrefix(pattern, "/") || (len(pattern) >= 2 && isASCIIAlpha(pattern[0]) && pattern[1] == ':') || strings.HasSuffix(pattern, "/") || strings.Contains(pattern, "//") {
		return compiledGlob{}, fmt.Errorf("scope glob is not repository-relative")
	}
	if strings.HasPrefix(pattern, "!") {
		return compiledGlob{}, fmt.Errorf("scope glob negation is not supported")
	}
	parts := strings.Split(pattern, "/")
	for _, part := range parts {
		if part == "." || part == ".." || part == "" {
			return compiledGlob{}, fmt.Errorf("scope glob contains traversal")
		}
	}
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '[':
			end, ok := classEnd(runes, i)
			if !ok {
				return compiledGlob{}, fmt.Errorf("unterminated scope glob class")
			}
			i = end
		case ']':
			// A standalone closing bracket is a literal in Git-style globs.
		case '*', '?', '/':
		default:
			if runes[i] == '\x00' {
				return compiledGlob{}, fmt.Errorf("scope glob contains NUL")
			}
		}
	}
	return compiledGlob{pattern: runes}, nil
}

func (g compiledGlob) match(value []rune) bool {
	type state struct{ pattern, value int }
	memo := make(map[state]bool)
	seen := make(map[state]bool)
	var walk func(int, int) bool
	walk = func(patternIndex, valueIndex int) bool {
		key := state{pattern: patternIndex, value: valueIndex}
		if seen[key] {
			return memo[key]
		}
		seen[key] = true
		result := false
		defer func() { memo[key] = result }()

		if patternIndex == len(g.pattern) {
			result = valueIndex == len(value)
			return result
		}
		if g.pattern[patternIndex] == '*' {
			starEnd := patternIndex
			for starEnd < len(g.pattern) && g.pattern[starEnd] == '*' {
				starEnd++
			}
			double := starEnd-patternIndex >= 2 && doubleStarComponent(g.pattern, patternIndex, starEnd)
			if double && starEnd < len(g.pattern) && g.pattern[starEnd] == '/' {
				// A component-aware **/ matches zero or more complete
				// path components, including the separator after each one.
				if walk(starEnd+1, valueIndex) {
					result = true
					return result
				}
				for next := valueIndex; next < len(value); next++ {
					if value[next] == '/' && walk(starEnd+1, next+1) {
						result = true
						return result
					}
				}
				return result
			}
			if double {
				// A terminal or component-boundary ** may cross slash
				// separators. Try the shortest match first for stable,
				// bounded backtracking.
				for next := valueIndex; next <= len(value); next++ {
					if walk(starEnd, next) {
						result = true
						return result
					}
				}
				return result
			}
			// A non-component ** is Git's ordinary single-segment *.
			for next := valueIndex; next <= len(value) && (next == valueIndex || value[next-1] != '/'); next++ {
				if walk(starEnd, next) {
					result = true
					return result
				}
			}
			return result
		}

		if valueIndex >= len(value) {
			return result
		}
		switch token := g.pattern[patternIndex]; token {
		case '?':
			if value[valueIndex] != '/' {
				result = walk(patternIndex+1, valueIndex+1)
			}
		case '[':
			end, ok := classEnd(g.pattern, patternIndex)
			if ok && value[valueIndex] != '/' && classMatches(g.pattern[patternIndex+1:end], value[valueIndex]) {
				result = walk(end+1, valueIndex+1)
			}
		default:
			if token == value[valueIndex] {
				result = walk(patternIndex+1, valueIndex+1)
			}
		}
		return result
	}
	return walk(0, 0)
}

func doubleStarComponent(pattern []rune, start, end int) bool {
	leftBoundary := start == 0 || pattern[start-1] == '/'
	rightBoundary := end == len(pattern) || pattern[end] == '/'
	return leftBoundary && rightBoundary
}

func classEnd(pattern []rune, start int) (int, bool) {
	if start >= len(pattern) || pattern[start] != '[' {
		return 0, false
	}
	index := start + 1
	negated := false
	if index < len(pattern) && (pattern[index] == '!' || pattern[index] == '^') {
		negated = true
		index++
	}
	if index < len(pattern) && pattern[index] == ']' {
		// A leading ] is a class member only when another ] closes the
		// class. Thus [!] is rejected while []] and [!]] work as
		// expected.
		closing := index + 1
		for closing < len(pattern) && pattern[closing] != ']' {
			closing++
		}
		if closing == len(pattern) {
			return 0, false
		}
		index++
	}
	for ; index < len(pattern); index++ {
		if pattern[index] == ']' {
			contentStart := start + 1
			if negated {
				contentStart++
			}
			return index, index > contentStart
		}
	}
	return 0, false
}

func classMatches(class []rune, value rune) bool {
	if len(class) == 0 {
		return false
	}
	negated := class[0] == '!' || class[0] == '^'
	if negated {
		class = class[1:]
	}
	if len(class) > 0 && class[0] == ']' {
		if value == ']' {
			return !negated
		}
		class = class[1:]
	}
	found := false
	for index := 0; index < len(class); index++ {
		if index+2 < len(class) && class[index+1] == '-' && class[index+2] != ']' {
			if class[index] <= value && value <= class[index+2] {
				found = true
			}
			index += 2
			continue
		}
		if class[index] == value {
			found = true
		}
	}
	if negated {
		return !found
	}
	return found
}

func isASCIIAlpha(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}
