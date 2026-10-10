package render

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLineEscapesControlAndBidi(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"red \x1b[31mX\x1b[0m", `red \x1b[31mX\x1b[0m`},
		{"csi \u009b31m", `csi \x9b31m`},
		{"del \x7f bell \a", `del \x7f bell \x07`},
		{"a\r\nb\tc\nd", "a  b c d"},
		{"over\rwrite", "over write"},
		{"evil \u202egnp.exe", `evil \u202egnp.exe`},
		{"iso \u2066x\u2069 mark \u200f", `iso \u2066x\u2069 mark \u200f`},
		{"sep\u2028line", "sep line"},
		{"bad \xff utf8", "bad � utf8"},
		{"emoji ⚠️ ok", "emoji ⚠️ ok"},
	}
	for _, c := range cases {
		if got := Line(c.in); got != c.want {
			t.Errorf("Line(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBlockKeepsNewlinesOnly(t *testing.T) {
	in := "title\r\n\tcode\x1b]8;;http://x\x07link\rnext\n"
	want := "title\n\tcode\\x1b]8;;http://x\\x07link\nnext\n"
	if got := Block(in); got != want {
		t.Errorf("Block = %q, want %q", got, want)
	}
}

func TestCapCountsTheNote(t *testing.T) {
	if s, cut := Cap("short", 10); s != "short" || cut {
		t.Errorf("Cap within limit changed it: %q %v", s, cut)
	}
	long := strings.Repeat("é", 500) // multibyte: the cap is in runes
	s, cut := Cap(long, 100)
	if !cut || utf8.RuneCountInString(s) > 100 {
		t.Fatalf("Cap(500 runes, 100) = %d runes, cut %v", utf8.RuneCountInString(s), cut)
	}
	if !strings.HasSuffix(s, "more characters]") || !utf8.ValidString(s) {
		t.Errorf("Cap note missing or bad UTF-8: %q", s)
	}
	kept := strings.Count(s, "é")
	if !strings.Contains(s, "[truncated: "+strconv.Itoa(500-kept)+" more characters]") {
		t.Errorf("note does not count the cut runes (kept %d): %q", kept, s)
	}
	if s, _ := Cap(long, 3); utf8.RuneCountInString(s) > utf8.RuneCountInString(truncNote(500)) || strings.Contains(s, "é") {
		t.Errorf("tiny cap should give the note alone: %q", s)
	}
}

func TestTextLayout(t *testing.T) {
	n := 0
	var nilInt *int
	got := New("pr wait", "open_threads", 10).
		Field("repo", "o/r").
		Field("empty", "").
		Field("zero", n).
		Field("nil", nilInt).
		Field("flag", false).
		Field("evil", "a\x1b[2Jb\nc").
		Item("PRRT_1 a.go:3 %s", "fix\x1b[31m it").
		Indent("    ", "line one\r\n\nline \u202ethree\n").
		String()
	want := "pr wait: open_threads (exit 10)\n" +
		"repo: o/r\n" +
		"zero: 0\n" +
		"flag: false\n" +
		`evil: a\x1b[2Jb c` + "\n" +
		`- PRRT_1 a.go:3 fix\x1b[31m it` + "\n" +
		"    line one\n" +
		"\n" +
		`    line \u202ethree` + "\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
