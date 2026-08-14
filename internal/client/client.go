package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TysonLabs/agentctl/internal/registry"
)

// MaxBodyBytes caps response bodies at 10 MiB.
const MaxBodyBytes = 10 << 20

// TransportError wraps DNS/dial/TLS/timeout/redirect-policy/body-cap failures (exit 3).
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string { return "transport: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// Response is a completed HTTP exchange (any status).
type Response struct {
	Status int
	Body   []byte
}

// Client is the single HTTP path in agentctl. GET only, by construction.
type Client struct {
	base    *url.URL
	token   registry.Secret
	hc      *http.Client
	version string
}

// New builds a client pinned to a registered base URL.
func New(baseURL string, token registry.Secret, timeout time.Duration, version string) (*Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %v", err)
	}
	c := &Client{base: base, token: token, version: version}
	c.hc = &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return fmt.Errorf("refusing redirect: more than 3 hops")
			}
			if req.URL.Scheme != base.Scheme || req.URL.Host != base.Host {
				return fmt.Errorf("refusing redirect off registered host %s://%s to %s://%s",
					base.Scheme, base.Host, req.URL.Scheme, req.URL.Host)
			}
			agentRoot := strings.TrimRight(base.Path, "/") + "/agent"
			if req.URL.Path != agentRoot && !strings.HasPrefix(req.URL.Path, agentRoot+"/") {
				return fmt.Errorf("refusing redirect outside %s: %s", agentRoot, req.URL.Path)
			}
			// The prefix check above sees the percent-DECODED path, but the
			// still-ENCODED form goes on the wire. A Location like
			// /agent/%2e%2e/secret decodes to /agent/../secret (passes the
			// prefix check) yet a normalizing target server resolves the
			// encoded form outside /agent. Reject any redirect whose escaped
			// path smuggles encoded dots/slashes/backslashes or whose decoded
			// path contains traversal segments.
			esc := strings.ToLower(req.URL.EscapedPath())
			if strings.Contains(esc, "%2e") || strings.Contains(esc, "%2f") || strings.Contains(esc, "%5c") {
				return fmt.Errorf("refusing redirect with encoded path characters: %s", req.URL.EscapedPath())
			}
			for _, seg := range strings.Split(req.URL.Path, "/") {
				if seg == ".." || seg == "." {
					return fmt.Errorf("refusing redirect with traversal segment: %s", req.URL.Path)
				}
			}
			return nil
		},
	}
	return c, nil
}

// Get performs the one and only HTTP verb agentctl knows. p and rawQuery
// must come from NormalizePath.
func (c *Client) Get(ctx context.Context, p, rawQuery string) (*Response, error) {
	u := *c.base
	// Join onto any path prefix in the registered base URL (e.g. a service
	// mounted behind a reverse-proxy subpath) instead of overwriting it.
	// p may contain percent-encoded characters (NormalizePath passes through
	// everything except %2e/%2f/%5c); treat it as the ESCAPED form so valid
	// sequences like %20 go on the wire untouched instead of being
	// double-encoded to %2520 by URL.String(). If p contains a stray '%'
	// that is not a valid escape, fall back to treating it as a decoded
	// path (the '%' is then correctly encoded as %25).
	escaped := strings.TrimRight(c.base.EscapedPath(), "/") + p
	if rel, perr := url.Parse(escaped); perr == nil && rel.Opaque == "" {
		u.Path = rel.Path
		u.RawPath = rel.RawPath
	} else {
		u.Path = strings.TrimRight(c.base.Path, "/") + p
		u.RawPath = ""
	}
	u.RawQuery = rawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	// The single Reveal() call site in agentctl.
	req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	req.Header.Set("User-Agent", "agentctl/"+c.version)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, &TransportError{Err: scrubErr(err, c.token)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, &TransportError{Err: scrubErr(err, c.token)}
	}
	if len(body) > MaxBodyBytes {
		return nil, &TransportError{Err: fmt.Errorf("response body exceeds %d MiB cap", MaxBodyBytes>>20)}
	}
	return &Response{Status: resp.StatusCode, Body: body}, nil
}

func scrubErr(err error, sec registry.Secret) error {
	return fmt.Errorf("%s", Scrub(err.Error(), sec))
}
