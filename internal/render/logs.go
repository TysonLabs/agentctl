package render

import (
	"bytes"
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
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&doc) != nil || doc.Entries == nil {
		return nil, false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, false
	}
	for _, raw := range doc.Entries {
		e := LogEntry{
			Time:    firstString(raw, "ts", "time", "dt", "timestamp"),
			Level:   firstString(raw, "level", "lvl", "severity"),
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

// SanitizeLogEntries returns a copy safe for terminal and structured output.
// It flattens line separators and removes terminal and bidirectional controls
// from every string, including nested field keys and values.
func SanitizeLogEntries(entries []LogEntry) []LogEntry {
	out := make([]LogEntry, len(entries))
	for i, entry := range entries {
		out[i] = entry
		out[i].Time = clean(entry.Time)
		out[i].Level = strings.ToUpper(clean(entry.Level))
		out[i].Source = clean(entry.Source)
		out[i].Message = clean(entry.Message)
		if entry.Fields != nil {
			out[i].Fields = sanitizeLogMap(entry.Fields)
		}
	}
	return out
}

// SanitizeLogText makes arbitrary text from the logs endpoint safe to print.
func SanitizeLogText(s string) string { return clean(s) }

func sanitizeLogMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[clean(key)] = sanitizeLogValue(value)
	}
	return out
}

func sanitizeLogValue(value any) any {
	switch value := value.(type) {
	case string:
		return clean(value)
	case map[string]any:
		return sanitizeLogMap(value)
	case []any:
		out := make([]any, len(value))
		for i := range value {
			out[i] = sanitizeLogValue(value[i])
		}
		return out
	default:
		return value
	}
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
		fmt.Fprintf(&b, "%-5s", clean(strings.ToUpper(e.Level)))
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

// clean flattens line separators and drops other control characters, DEL,
// C1, and bidirectional formatting controls. Some terminals treat U+009B as
// an escape introducer, while bidi controls can visually reorder a log line.
func clean(s string) string {
	s = strings.NewReplacer("\r\n", " ⏎ ", "\n", " ⏎ ", "\r", " ", "\t", " ", "\u2028", " ⏎ ", "\u2029", " ⏎ ").Replace(s)
	s = strings.Map(func(r rune) rune {
		if r == 0x7f || (r >= 0x80 && r <= 0x9f) || isBidiControl(r) {
			return -1
		}
		return r
	}, s)
	return string(StripControl([]byte(s)))
}

func isBidiControl(r rune) bool {
	return r == '\u061c' ||
		(r >= '\u200e' && r <= '\u200f') ||
		(r >= '\u202a' && r <= '\u202e') ||
		(r >= '\u2066' && r <= '\u206f')
}
