package redcheck

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// DefaultTestPatterns name the files kept at the "after" state. A pattern
// with no slash matches the file's base name; a pattern with a slash matches
// the whole repository-relative path, where ** stands for any number of
// directories (including none).
var DefaultTestPatterns = []string{
	"*_test.go",
	"test_*.py", "*_test.py", "conftest.py",
	"*.test.js", "*.test.jsx", "*.test.ts", "*.test.tsx",
	"*.spec.js", "*.spec.jsx", "*.spec.ts", "*.spec.tsx",
	"**/tests/**", "**/test/**", "**/__tests__/**", "**/spec/**",
	"**/testdata/**",
}

// NormalizePattern trims a leading "./" and turns a trailing "/" into
// "/**", and rejects a malformed glob.
func NormalizePattern(p string) (string, error) {
	p = strings.TrimPrefix(strings.TrimSpace(p), "./")
	if strings.HasSuffix(p, "/") {
		p += "**"
	}
	if p == "" {
		return "", fmt.Errorf("empty pattern")
	}
	for _, seg := range strings.Split(p, "/") {
		if _, err := path.Match(seg, ""); err != nil {
			return "", fmt.Errorf("pattern %q: %v", p, err)
		}
	}
	return p, nil
}

// Match reports whether the repository-relative, slash-separated path p
// matches pattern (see DefaultTestPatterns for the syntax).
func Match(pattern, p string) bool {
	if !strings.Contains(pattern, "/") {
		ok, _ := path.Match(pattern, path.Base(p))
		return ok
	}
	return matchSegs(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegs(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

func matchAny(patterns []string, p string) bool {
	for _, pat := range patterns {
		if Match(pat, p) {
			return true
		}
	}
	return false
}

// testMarkers find test code inside a file that is otherwise source. Go
// needs none: a Go test function can only live in a _test.go file.
var (
	rustMarker = regexp.MustCompile(`^\s*(?:#\[\s*(?:[\w:]+::)?(?:test|rstest|test_case)\b|#\[\s*cfg\s*\(\s*test\s*\)\s*\]|(?:pub(?:\([^)]*\))?\s+)?mod\s+tests?\b|proptest!)`)
	pyMarker   = regexp.MustCompile(`^\s*(?:async\s+)?def\s+test_|^\s*class\s+Test\w*\s*[(:]`)
	jsMarker   = regexp.MustCompile(`^\s*(?:it|test|describe)(?:\.\w+)?\s*\(`)
	// rustCfgTest starts a Rust inline test region: #[cfg(test)] followed by a mod item.
	rustCfgTest = regexp.MustCompile(`^\s*#\[\s*cfg\s*\(\s*test\s*\)\s*\]\s*$`)
	rustModItem = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?mod\s+\w+`)
)

func markerFor(p string) *regexp.Regexp {
	switch strings.ToLower(path.Ext(p)) {
	case ".rs":
		return rustMarker
	case ".py":
		return pyMarker
	case ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts":
		return jsMarker
	}
	return nil
}

// splitLines keeps each line's terminator so a splice reproduces the bytes.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.SplitAfter(strings.TrimSuffix(s, "\n"), "\n")
}

// addedMarkers lists marker lines in after that before does not have as
// often (a multiset difference, so a moved test still counts as unchanged).
func addedMarkers(re *regexp.Regexp, before, after []string) []string {
	count := map[string]int{}
	for _, l := range before {
		if re.MatchString(l) {
			count[strings.TrimSpace(l)]++
		}
	}
	var added []string
	for _, l := range after {
		if !re.MatchString(l) {
			continue
		}
		k := strings.TrimSpace(l)
		if count[k] > 0 {
			count[k]--
			continue
		}
		added = append(added, k)
	}
	return added
}

// changedTestBlocks reports a body-only change to an existing test. Marker
// comparison alone cannot see an edited assertion beneath an unchanged test
// declaration.
func changedTestBlocks(p, before, after string) bool {
	count := map[string]int{}
	for _, block := range testBlocks(p, before) {
		count[block]++
	}
	for _, block := range testBlocks(p, after) {
		if count[block] == 0 {
			return true
		}
		count[block]--
	}
	return false
}

func testBlocks(p, content string) []string {
	lines := splitLines(content)
	re := markerFor(p)
	if re == nil {
		return nil
	}
	ext := strings.ToLower(path.Ext(p))
	var blocks []string
	offset := 0
	for i, line := range lines {
		start := offset
		offset += len(line)
		if !re.MatchString(line) {
			continue
		}
		end := offset
		switch ext {
		case ".py":
			indent := leadingWhitespace(line)
			for j := i + 1; j < len(lines); j++ {
				t := strings.TrimSpace(lines[j])
				if t != "" && !strings.HasPrefix(t, "#") && leadingWhitespace(lines[j]) <= indent {
					break
				}
				end += len(lines[j])
			}
		case ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts":
			if open := strings.IndexByte(content[start:], '('); open >= 0 {
				if close := matchingDelimiter(content, start+open, '(', ')'); close >= 0 {
					end = close + 1
				}
			}
		case ".rs":
			if open := strings.IndexByte(content[start:], '{'); open >= 0 {
				if close := matchingDelimiter(content, start+open, '{', '}'); close >= 0 {
					end = close + 1
				}
			}
		}
		blocks = append(blocks, strings.TrimSpace(content[start:end]))
	}
	return blocks
}

func leadingWhitespace(s string) int {
	return len(s) - len(strings.TrimLeft(s, " \t"))
}

func matchingDelimiter(s string, start int, open, close byte) int {
	depth := 0
	blockComments := 0
	var quote byte
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if blockComments > 0 {
			if i+1 < len(s) && c == '/' && s[i+1] == '*' {
				blockComments++
				i++
			} else if i+1 < len(s) && c == '*' && s[i+1] == '/' {
				blockComments--
				i++
			}
			continue
		}
		if quote != 0 {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == quote {
				quote = 0
			}
			continue
		}
		if i+1 < len(s) && c == '/' && s[i+1] == '/' {
			if nl := strings.IndexByte(s[i+2:], '\n'); nl >= 0 {
				i += nl + 2
				continue
			}
			return -1
		}
		if i+1 < len(s) && c == '/' && s[i+1] == '*' {
			blockComments = 1
			i++
			continue
		}
		if c == '"' || c == '`' || (c == '\'' && looksLikeCharLiteral(s[i:])) {
			quote = c
			continue
		}
		switch c {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func looksLikeCharLiteral(s string) bool {
	if len(s) >= 3 && s[2] == '\'' {
		return true
	}
	return len(s) >= 4 && s[1] == '\\' && s[3] == '\''
}

// rustTestRegion returns the index of the line that opens the file's trailing
// inline test module (#[cfg(test)] followed by a mod item), or -1. A module
// followed by production code is not safe to splice.
func rustTestRegion(lines []string) int {
	start, end, content := rustTestModule(lines)
	if start >= 0 && rustTriviaOnly(content[end+1:]) {
		return start
	}
	return -1
}

// rustTestModule finds the first inline #[cfg(test)] module, whether or not
// it is trailing. end is the byte offset of its closing brace.
func rustTestModule(lines []string) (start, end int, content string) {
	content = strings.Join(lines, "")
	offset := 0
	for i, l := range lines {
		offset += len(l)
		if !rustCfgTest.MatchString(l) {
			continue
		}
		nextOffset := offset
		for _, next := range lines[i+1:] {
			t := strings.TrimSpace(next)
			if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#[") {
				nextOffset += len(next)
				continue
			}
			if rustModItem.MatchString(next) {
				rest := content[nextOffset:]
				open := strings.IndexByte(rest, '{')
				if semi := strings.IndexByte(rest, ';'); open < 0 || (semi >= 0 && semi < open) {
					break
				}
				close := matchingDelimiter(content, nextOffset+open, '{', '}')
				if close >= 0 {
					return i, close, content
				}
			}
			break
		}
	}
	return -1, -1, content
}

func rustTriviaOnly(s string) bool {
	for {
		s = strings.TrimSpace(s)
		switch {
		case s == "":
			return true
		case strings.HasPrefix(s, "//"):
			if nl := strings.IndexByte(s, '\n'); nl >= 0 {
				s = s[nl+1:]
				continue
			}
			return true
		case strings.HasPrefix(s, "/*"):
			if end := strings.Index(s[2:], "*/"); end >= 0 {
				s = s[end+4:]
				continue
			}
		}
		return false
	}
}

// sourceVerdict says what reverting one source file does to its tests.
type sourceVerdict struct {
	splice  string // non-empty: write this "before code + after tests" content instead of reverting
	reasons []string
}

// judgeSource inspects a source file whose change will be reverted. status
// is the git name-status letter. allowSplice permits the Rust splice.
func judgeSource(p string, status byte, before, after string, allowSplice bool) sourceVerdict {
	re := markerFor(p)
	if re == nil {
		return sourceVerdict{}
	}
	bl, al := splitLines(before), splitLines(after)
	if strings.ToLower(path.Ext(p)) != ".rs" {
		if added := addedMarkers(re, bl, al); len(added) > 0 {
			return sourceVerdict{reasons: []string{fmt.Sprintf("adds or changes test code: %s", added[0])}}
		}
		if changedTestBlocks(p, before, after) {
			return sourceVerdict{reasons: []string{"changes the body of test code in the source file"}}
		}
		return sourceVerdict{}
	}
	ta := rustTestRegion(al)
	tb := rustTestRegion(bl)
	beforeHasNonTrailingModule := false
	if tb < 0 {
		any, _, _ := rustTestModule(bl)
		beforeHasNonTrailingModule = any >= 0
		tb = len(bl)
	}
	if ta < 0 {
		if added := addedMarkers(re, bl, al); len(added) > 0 {
			return sourceVerdict{reasons: []string{fmt.Sprintf("adds or changes test code: %s", added[0])}}
		}
		if changedTestBlocks(p, before, after) {
			return sourceVerdict{reasons: []string{"changes the body of test code in the source file"}}
		}
		return sourceVerdict{}
	}
	var reasons []string
	if beforeHasNonTrailingModule {
		reasons = append(reasons, "its before state has a non-trailing #[cfg(test)] module that cannot be spliced safely")
	}
	if added := addedMarkers(re, bl[:tb], al[:ta]); len(added) > 0 {
		reasons = append(reasons, fmt.Sprintf("adds or changes test code outside its #[cfg(test)] module: %s", added[0]))
	}
	if changedTestBlocks(p, strings.Join(bl[:tb], ""), strings.Join(al[:ta], "")) {
		reasons = append(reasons, "changes the body of test code outside its #[cfg(test)] module")
	}
	if strings.Join(bl[tb:], "") == strings.Join(al[ta:], "") {
		return sourceVerdict{reasons: reasons}
	}
	// The inline test module changed.
	switch {
	case status != 'M':
		reasons = append(reasons, "the file is new (or not a plain modification) and holds an inline test module")
	case !allowSplice:
		reasons = append(reasons, "changes its inline #[cfg(test)] module (splicing is off)")
	}
	if len(reasons) > 0 {
		return sourceVerdict{reasons: reasons}
	}
	splice := strings.Join(bl[:tb], "")
	if splice != "" && !strings.HasSuffix(splice, "\n") {
		splice += "\n"
	}
	splice += strings.Join(al[ta:], "")
	if strings.HasSuffix(after, "\n") && !strings.HasSuffix(splice, "\n") {
		splice += "\n"
	}
	return sourceVerdict{splice: splice}
}
