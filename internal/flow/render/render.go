// Package render turns agentflow results into short, stable, grep-friendly
// text, and makes untrusted text (review comments, finding text, error
// messages from other tools) safe to print on a terminal.
//
// The text layout is:
//
//	<command>: <status> (exit N)
//	key: value                 one field per line; empty fields are left out
//	- item                     lists (findings, threads, refusals, candidates)
//	    indented body          multi-line text under an item
//
// Every value passes through Line or Block, so no control character, ANSI
// escape or bidirectional override from the data reaches the terminal: each
// one is shown escaped (\x1b, \u202e) instead.
package render

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Text builds one text rendering. The zero value is not usable; call New.
type Text struct {
	b strings.Builder
}

// New starts a rendering with the header line "<command>: <status> (exit N)".
func New(command, status string, exit int) *Text {
	t := &Text{}
	t.b.WriteString(Line(command) + ": " + Line(status) + " (exit " + strconv.Itoa(exit) + ")\n")
	return t
}

// Field adds "key: value". An empty string or a nil pointer is left out;
// pointers are dereferenced. Other zero values (0, false) are printed,
// because they carry meaning.
func (t *Text) Field(key string, v any) *Text {
	s, ok := value(v)
	if !ok {
		return t
	}
	t.b.WriteString(Line(key) + ": " + Line(s) + "\n")
	return t
}

// Item adds a list line "- <text>".
func (t *Text) Item(format string, a ...any) *Text {
	t.b.WriteString("- " + Line(fmt.Sprintf(format, a...)) + "\n")
	return t
}

// Indent adds multi-line text, each line prefixed by indent. Empty text adds
// nothing. The text passes through Block, so newlines are kept and every
// other control character is escaped.
func (t *Text) Indent(indent, text string) *Text {
	text = strings.TrimRight(Block(text), "\n")
	if strings.TrimSpace(text) == "" {
		return t
	}
	for _, l := range strings.Split(text, "\n") {
		if l == "" {
			t.b.WriteString("\n")
			continue
		}
		t.b.WriteString(indent + l + "\n")
	}
	return t
}

// String returns the rendering, ending in a newline.
func (t *Text) String() string { return t.b.String() }

func value(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, x != ""
	case *string:
		if x == nil {
			return "", false
		}
		return *x, *x != ""
	case *int:
		if x == nil {
			return "", false
		}
		return strconv.Itoa(*x), true
	case *bool:
		if x == nil {
			return "", false
		}
		return strconv.FormatBool(*x), true
	case *float64:
		if x == nil {
			return "", false
		}
		return strconv.FormatFloat(*x, 'f', -1, 64), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case fmt.Stringer:
		s := x.String()
		return s, s != ""
	default:
		s := fmt.Sprint(x)
		return s, s != ""
	}
}

// Line makes untrusted text safe for a single output line: CR, LF, tab and
// the Unicode line separators become spaces, every other control, format
// or bidirectional-override character is escaped, and invalid UTF-8 becomes
// U+FFFD.
func Line(s string) string {
	return clean(s, false)
}

// Block is Line for multi-line text: LF and tab are kept, CRLF becomes LF.
// A tab is layout, not a terminal command, and review suggestions carry
// code indented with tabs.
func Block(s string) string {
	return clean(s, true)
}

func clean(s string, multiline bool) string {
	if multiline {
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	if safe(s, multiline) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		i += size
		switch {
		case r == '\n' || r == '\t':
			if multiline {
				b.WriteRune(r)
			} else {
				b.WriteByte(' ')
			}
		case r == '\r' || r == '\u2028' || r == '\u2029':
			if multiline {
				b.WriteByte('\n')
			} else {
				b.WriteByte(' ')
			}
		case unsafeRune(r):
			if r < 0x100 {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				fmt.Fprintf(&b, `\u%04x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// safe is the fast path: true when clean would return s unchanged.
func safe(s string, multiline bool) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			if !multiline {
				return false
			}
		case r == '\r' || r == '\u2028' || r == '\u2029' || unsafeRune(r):
			return false
		}
	}
	return true
}

// unsafeRune reports C0 and C1 controls, DEL, and the invisible characters
// that reorder or hide text on a terminal (bidi embeddings, overrides,
// isolates and marks).
func unsafeRune(r rune) bool {
	switch {
	case r < 0x20, r >= 0x7f && r < 0xa0:
		return true
	case r == '\u061c', r == '\u200e', r == '\u200f',
		r >= '\u202a' && r <= '\u202e',
		r >= '\u2066' && r <= '\u2069':
		return true
	}
	return false
}

// Cap shortens s to at most max runes in total, including the note it
// appends when it cuts ("… [truncated: N more characters]"). It reports
// whether it cut. A max too small for the note returns the note alone.
func Cap(s string, max int) (string, bool) {
	n := utf8.RuneCountInString(s)
	if n <= max {
		return s, false
	}
	// The note's own length depends on the digits of the cut count, so size
	// it from the largest count it can show (all of s).
	reserve := utf8.RuneCountInString(truncNote(n))
	keep := max - reserve
	if keep < 0 {
		keep = 0
	}
	cut := 0
	for i := range s {
		if cut == keep {
			return s[:i] + truncNote(n-keep), true
		}
		cut++
	}
	return s, false // unreachable: n > max >= keep
}

func truncNote(more int) string {
	return "\n… [truncated: " + strconv.Itoa(more) + " more characters]"
}
