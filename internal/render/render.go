// Package render formats agentctl output: pretty JSON, tables, status lines.
package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// PrettyJSON indents b if it is valid JSON; ok=false means pass bytes through.
func PrettyJSON(b []byte) (out []byte, ok bool) {
	if !json.Valid(bytes.TrimSpace(b)) {
		return nil, false
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, bytes.TrimSpace(b), "", "  "); err != nil {
		return nil, false
	}
	buf.WriteByte('\n')
	return buf.Bytes(), true
}

// StripControl removes C0 control characters except \n and \t.
func StripControl(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c < 0x20 && c != '\n' && c != '\t' {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Endpoint is one descriptor from a /agent index following the loose convention.
type Endpoint struct {
	Path        string `json:"path"`
	Description string `json:"description"`
}

// ParseEndpoints tries the loose index convention; ok=false means fall back.
func ParseEndpoints(body []byte) (eps []Endpoint, ok bool) {
	var idx struct {
		Endpoints []Endpoint `json:"endpoints"`
	}
	if err := json.Unmarshal(body, &idx); err != nil || len(idx.Endpoints) == 0 {
		return nil, false
	}
	return idx.Endpoints, true
}

// EndpointsTable renders a two-column PATH/DESCRIPTION table.
func EndpointsTable(w io.Writer, eps []Endpoint) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PATH\tDESCRIPTION")
	for _, e := range eps {
		fmt.Fprintf(tw, "%s\t%s\n", oneLine(e.Path), oneLine(e.Description))
	}
	tw.Flush()
}

// LsRow is one line of `agentctl ls`.
type LsRow struct {
	Name    string
	Wired   bool
	Reason  string
	BaseURL string
	Meta    map[string]string
}

// LsTable renders the service listing. No token material, ever.
func LsTable(w io.Writer, rows []LsRow) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE.ENV\tSTATUS\tBASE_URL\tMETA")
	for _, r := range rows {
		status := "wired"
		base := r.BaseURL
		if !r.Wired {
			status = "NOT WIRED"
			base += "   (" + r.Reason + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, status, base, formatMeta(r.Meta))
	}
	tw.Flush()
}

// formatMeta renders a meta map as sorted "k=v" pairs on one line.
func formatMeta(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, oneLine(k)+"="+oneLine(m[k]))
	}
	return strings.Join(parts, " ")
}

// VersionFromBody extracts a short version string from a /agent/version body.
func VersionFromBody(body []byte) string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		for _, k := range []string{"version", "sha", "commit", "build"} {
			if s, ok := m[k].(string); ok && s != "" {
				return oneLine(s)
			}
		}
	}
	s := oneLine(strings.TrimSpace(string(body)))
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return string(StripControl([]byte(s)))
}
