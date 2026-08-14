package client

import (
	"fmt"
	"path"
	"strings"
)

// NormalizePath maps user input to a safe request path under /agent.
// Accepted spellings: "", "/", "version", "/version", "agent/version",
// "/agent/version". A query string ("?limit=5") is split off and returned
// separately. Fragments are stripped. Traversal, encoded traversal,
// backslashes, and scheme smuggling are rejected.
func NormalizePath(in string) (reqPath, rawQuery string, err error) {
	raw := in
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		rawQuery = raw[i+1:]
		raw = raw[:i]
	}
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	low := strings.ToLower(raw)
	switch {
	case strings.Contains(raw, "\\"):
		return "", "", fmt.Errorf("invalid path %q: backslashes not allowed", in)
	case strings.Contains(raw, "://"):
		return "", "", fmt.Errorf("invalid path %q: URLs not allowed, give a path under /agent", in)
	case strings.Contains(low, "%2e"), strings.Contains(low, "%2f"), strings.Contains(low, "%5c"):
		return "", "", fmt.Errorf("invalid path %q: percent-encoded separators not allowed", in)
	}
	for _, seg := range strings.Split(raw, "/") {
		if seg == ".." {
			return "", "", fmt.Errorf("invalid path %q: traversal not allowed", in)
		}
	}
	raw = strings.TrimPrefix(raw, "/")
	if raw == "agent" {
		raw = ""
	}
	raw = strings.TrimPrefix(raw, "agent/")
	p := path.Join("/agent", raw)
	if p != "/agent" && !strings.HasPrefix(p, "/agent/") {
		return "", "", fmt.Errorf("invalid path %q: escapes /agent", in)
	}
	return p, rawQuery, nil
}
