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
	rustMarker = regexp.MustCompile(`#\[\s*(?:[\w:]+::)?(?:test|rstest|test_case)\b|#\[\s*cfg\s*\(\s*test\s*\)\s*\]|^\s*(?:pub(?:\([^)]*\))?\s+)?mod\s+tests?\b|\bproptest!`)
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

// rustTestRegion returns the index of the line that opens the file's inline
// test module (#[cfg(test)] followed by a mod item), or -1. By convention
// that module runs to the end of the file.
func rustTestRegion(lines []string) int {
	for i, l := range lines {
		if !rustCfgTest.MatchString(l) {
			continue
		}
		for _, next := range lines[i+1:] {
			t := strings.TrimSpace(next)
			if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#[") {
				continue
			}
			if rustModItem.MatchString(next) {
				return i
			}
			break
		}
	}
	return -1
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
		return sourceVerdict{}
	}
	ta := rustTestRegion(al)
	tb := rustTestRegion(bl)
	if tb < 0 {
		tb = len(bl)
	}
	if ta < 0 {
		if added := addedMarkers(re, bl, al); len(added) > 0 {
			return sourceVerdict{reasons: []string{fmt.Sprintf("adds or changes test code: %s", added[0])}}
		}
		return sourceVerdict{}
	}
	var reasons []string
	if added := addedMarkers(re, bl[:tb], al[:ta]); len(added) > 0 {
		reasons = append(reasons, fmt.Sprintf("adds or changes test code outside its #[cfg(test)] module: %s", added[0]))
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
