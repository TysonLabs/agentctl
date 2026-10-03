package render

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseLogsPreservesJSONNumbers(t *testing.T) {
	entries, ok := ParseLogs([]byte(`{"entries":[{"time":"t","level":"info","message":"m","fields":{"id":9007199254740993}}]}`))
	if !ok || len(entries) != 1 {
		t.Fatalf("ParseLogs = (%v, %v)", entries, ok)
	}
	if got, ok := entries[0].Fields["id"].(json.Number); !ok || got.String() != "9007199254740993" {
		t.Fatalf("id = %#v, want exact json.Number", entries[0].Fields["id"])
	}
}

func TestSanitizeLogEntriesRecurses(t *testing.T) {
	entries := SanitizeLogEntries([]LogEntry{{
		Time:    "t\u2028forged",
		Level:   "info\u007f",
		Source:  "src\u202e",
		Message: "line1\nline2",
		Fields: map[string]any{
			"key\u001b": []any{"value\u009b", map[string]any{"nested\u2066": "safe\u2069"}},
		},
	}})
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []rune{'\u001b', '\u007f', '\u009b', '\u2028', '\u202e', '\u2066', '\u2069'} {
		if strings.ContainsRune(string(b), forbidden) {
			t.Fatalf("dangerous rune %U survived: %q", forbidden, b)
		}
	}
	if !strings.Contains(string(b), "line1 ⏎ line2") || !strings.Contains(string(b), "t ⏎ forged") {
		t.Fatalf("line separators were not visibly flattened: %s", b)
	}

	var out bytes.Buffer
	LogLines(&out, entries)
	if strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("one entry rendered as multiple lines: %q", out.String())
	}
}
