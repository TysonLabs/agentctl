package render

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// LogEntry is one /agent/logs record, normalized across the shapes services
// emit today:
//
//	{"ts", "level", "target", "message"}           (recursivecx)
//	{"time", "level", "message", "fields": {...}}  (vector-dialer)
//	{"dt", "level", "msg", "fields": {...}}        (agentassist-callcenter)
type LogEntry struct {
	Time    string         `json:"time"`
	Level   string         `json:"level"`
	Source  string         `json:"source,omitempty"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// ParseLogs reads the "entries" array of an /agent/logs body. ok is false
// when the body is not that shape (the caller then shows it raw).
func ParseLogs(body []byte) (entries []LogEntry, ok bool) {
	var doc struct {
		Entries []map[string]any `json:"entries"`
	}
	if json.Unmarshal(body, &doc) != nil || doc.Entries == nil {
		return nil, false
	}
	for _, raw := range doc.Entries {
		e := LogEntry{
			Time:    firstString(raw, "ts", "time", "dt", "timestamp"),
			Level:   strings.ToUpper(firstString(raw, "level", "lvl", "severity")),
			Source:  firstString(raw, "target", "logger", "source"),
			Message: firstString(raw, "message", "msg"),
		}
		if f, isMap := raw["fields"].(map[string]any); isMap && len(f) > 0 {
			e.Fields = f
		}
		entries = append(entries, e)
	}
	return entries, true
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// LogLines writes one readable line per entry:
//
//	2026-10-03T14:40:36.547Z INFO  http.access  method=POST status=200 | app=x k=v
//
// Every field is stripped of control characters, so a log message cannot
// inject terminal escapes.
func LogLines(w io.Writer, entries []LogEntry) {
	for _, e := range entries {
		var b strings.Builder
		b.WriteString(clean(e.Time))
		b.WriteString(" ")
		fmt.Fprintf(&b, "%-5s", clean(e.Level))
		if e.Source != "" {
			b.WriteString(" ")
			b.WriteString(clean(e.Source))
		}
		b.WriteString("  ")
		b.WriteString(clean(e.Message))
		if len(e.Fields) > 0 {
			keys := make([]string, 0, len(e.Fields))
			for k := range e.Fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteString(" |")
			for _, k := range keys {
				v := e.Fields[k]
				var s string
				if str, ok := v.(string); ok {
					s = str
				} else {
					j, _ := json.Marshal(v)
					s = string(j)
				}
				fmt.Fprintf(&b, " %s=%s", clean(k), clean(s))
			}
		}
		b.WriteString("\n")
		_, _ = io.WriteString(w, b.String())
	}
}

// clean flattens newlines and drops other control characters, C1 included
// (U+0080..U+009F: some terminals treat U+009B as an escape introducer).
func clean(s string) string {
	s = strings.NewReplacer("\r\n", " ⏎ ", "\n", " ⏎ ", "\r", " ", "\t", " ").Replace(s)
	s = strings.Map(func(r rune) rune {
		if r >= 0x80 && r <= 0x9f {
			return -1
		}
		return r
	}, s)
	return string(StripControl([]byte(s)))
}
