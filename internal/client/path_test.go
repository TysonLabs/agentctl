package client

import "testing"

func TestNormalizePath(t *testing.T) {
	cases := []struct {
		in        string
		wantPath  string
		wantQuery string
		wantErr   bool
	}{
		{in: "version", wantPath: "/agent/version"},
		{in: "/version", wantPath: "/agent/version"},
		{in: "agent/version", wantPath: "/agent/version"},
		{in: "/agent/version", wantPath: "/agent/version"},
		{in: "/agent", wantPath: "/agent"},
		{in: "agent", wantPath: "/agent"},
		{in: "/", wantPath: "/agent"},
		{in: "", wantPath: "/agent"},
		{in: "logs?limit=5", wantPath: "/agent/logs", wantQuery: "limit=5"},
		{in: "logs#frag", wantPath: "/agent/logs"},
		{in: "a/b/c", wantPath: "/agent/a/b/c"},
		{in: "//evil.com/x", wantPath: "/agent/evil.com/x"},
		{in: "../x", wantErr: true},
		{in: "a/../../x", wantErr: true},
		{in: "..", wantErr: true},
		{in: "%2e%2e%2fx", wantErr: true},
		{in: "%2E%2E/x", wantErr: true},
		{in: "a%2fb", wantErr: true},
		{in: "a%5cb", wantErr: true},
		{in: `a\b`, wantErr: true},
		{in: "https://evil.com/agent", wantErr: true},
	}
	for _, c := range cases {
		p, q, err := NormalizePath(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("NormalizePath(%q): want error, got %q", c.in, p)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizePath(%q): unexpected error %v", c.in, err)
			continue
		}
		if p != c.wantPath || q != c.wantQuery {
			t.Errorf("NormalizePath(%q) = (%q, %q), want (%q, %q)", c.in, p, q, c.wantPath, c.wantQuery)
		}
	}
}
