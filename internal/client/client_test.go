package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TysonLabs/agentctl/internal/registry"
)

const testToken = "at_test_token_abcdef"

func newClient(t *testing.T, baseURL string, timeout time.Duration) *Client {
	t.Helper()
	c, err := New(baseURL, registry.NewSecret(testToken), timeout, "test")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAuthHeaderSentOnce(t *testing.T) {
	var gotAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Values("Authorization")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL, 5*time.Second)
	resp, err := c.Get(context.Background(), "/agent/version", "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 {
		t.Fatalf("status %d", resp.Status)
	}
	if len(gotAuth) != 1 || gotAuth[0] != "Bearer "+testToken {
		t.Fatalf("Authorization = %v", gotAuth)
	}
}

func TestSameHostRedirectFollowed(t *testing.T) {
	var mux http.ServeMux
	srv := httptest.NewServer(&mux)
	defer srv.Close()
	mux.HandleFunc("/agent/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/agent/new", http.StatusFound)
	})
	mux.HandleFunc("/agent/new", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "landed")
	})
	c := newClient(t, srv.URL, 5*time.Second)
	resp, err := c.Get(context.Background(), "/agent/old", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "landed" {
		t.Fatalf("body %q", resp.Body)
	}
}

func TestCrossHostRedirectBlocked(t *testing.T) {
	var otherHit atomic.Bool
	var otherSawAuth atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHit.Store(true)
		if r.Header.Get("Authorization") != "" {
			otherSawAuth.Store(true)
		}
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/agent/steal", http.StatusFound)
	}))
	defer srv.Close()

	c := newClient(t, srv.URL, 5*time.Second)
	_, err := c.Get(context.Background(), "/agent/version", "")
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want TransportError, got %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token in error: %v", err)
	}
	if otherHit.Load() {
		t.Fatal("second host was contacted")
	}
	if otherSawAuth.Load() {
		t.Fatal("second host saw the Authorization header")
	}
}

func TestRedirectOutsideAgentBlocked(t *testing.T) {
	var mux http.ServeMux
	srv := httptest.NewServer(&mux)
	defer srv.Close()
	mux.HandleFunc("/agent/x", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/admin/delete", http.StatusFound)
	})
	c := newClient(t, srv.URL, 5*time.Second)
	_, err := c.Get(context.Background(), "/agent/x", "")
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want TransportError, got %v", err)
	}
}

// TestEncodedTraversalRedirectBlocked is a regression test: a Location of
// /agent/%2e%2e/secret decodes to /agent/../secret (which passes a naive
// prefix check on req.URL.Path) while the encoded form goes on the wire and a
// normalizing target resolves it outside /agent. The client must refuse it —
// and never send the follow-up request.
func TestEncodedTraversalRedirectBlocked(t *testing.T) {
	cases := []string{
		"/agent/%2e%2e/internal/metrics", // encoded dots
		"/agent/%2E%2E/admin",            // uppercase encoding
		"/agent/..%2fsecret",             // encoded slash
		"/agent/..%5csecret",             // encoded backslash
		"/agent/%2e/x",                   // encoded single dot
	}
	for _, loc := range cases {
		t.Run(loc, func(t *testing.T) {
			var followed atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/agent/start" {
					// Set Location raw to keep the encoding intact.
					w.Header().Set("Location", loc)
					w.WriteHeader(http.StatusFound)
					return
				}
				followed.Store(true)
			}))
			defer srv.Close()
			c := newClient(t, srv.URL, 5*time.Second)
			_, err := c.Get(context.Background(), "/agent/start", "")
			var te *TransportError
			if !errors.As(err, &te) {
				t.Fatalf("want TransportError for %s, got %v", loc, err)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatalf("token in error: %v", err)
			}
			if followed.Load() {
				t.Fatalf("encoded traversal redirect %s was followed", loc)
			}
		})
	}
}

// TestDotDotSegmentRedirectBlocked covers a decoded traversal segment that
// somehow survives into req.URL.Path (belt-and-braces alongside the encoded
// check above).
func TestDotDotSegmentRedirectBlocked(t *testing.T) {
	var followed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/start" {
			w.Header().Set("Location", "/agent/../secret")
			w.WriteHeader(http.StatusFound)
			return
		}
		followed.Store(true)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL, 5*time.Second)
	_, err := c.Get(context.Background(), "/agent/start", "")
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want TransportError, got %v", err)
	}
	if followed.Load() {
		t.Fatal("traversal redirect was followed")
	}
}

func TestTooManyHopsBlocked(t *testing.T) {
	var mux http.ServeMux
	srv := httptest.NewServer(&mux)
	defer srv.Close()
	for i := 0; i < 5; i++ {
		i := i
		mux.HandleFunc(fmt.Sprintf("/agent/hop%d", i), func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, fmt.Sprintf("%s/agent/hop%d", srv.URL, i+1), http.StatusFound)
		})
	}
	c := newClient(t, srv.URL, 5*time.Second)
	_, err := c.Get(context.Background(), "/agent/hop0", "")
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want TransportError on 4-hop chain, got %v", err)
	}
}

func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL, 100*time.Millisecond)
	_, err := c.Get(context.Background(), "/agent/slow", "")
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want TransportError on timeout, got %v", err)
	}
}

func TestBodyOverCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := strings.Repeat("a", 1<<20)
		for i := 0; i < 11; i++ {
			fmt.Fprint(w, chunk)
		}
	}))
	defer srv.Close()
	c := newClient(t, srv.URL, 30*time.Second)
	_, err := c.Get(context.Background(), "/agent/huge", "")
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("want TransportError on oversized body, got %v", err)
	}
}

func TestErrorStatusPassedThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL, 5*time.Second)
	resp, err := c.Get(context.Background(), "/agent/health", "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 500 || !strings.Contains(string(resp.Body), "boom") {
		t.Fatalf("resp %d %q", resp.Status, resp.Body)
	}
}

func TestQueryAttached(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		fmt.Fprint(w, "{}")
	}))
	defer srv.Close()
	c := newClient(t, srv.URL, 5*time.Second)
	if _, err := c.Get(context.Background(), "/agent/logs", "limit=5"); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "limit=5" {
		t.Fatalf("query %q", gotQuery)
	}
}

func TestBasePathPrefixJoined(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL+"/payments", 5*time.Second)
	resp, err := c.Get(context.Background(), "/agent/version", "")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 {
		t.Fatalf("status %d", resp.Status)
	}
	if gotPath != "/payments/agent/version" {
		t.Fatalf("request path = %q, want /payments/agent/version", gotPath)
	}
}

func TestBasePathTrailingSlashJoined(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	c := newClient(t, srv.URL+"/payments/", 5*time.Second)
	if _, err := c.Get(context.Background(), "/agent/version", ""); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/payments/agent/version" {
		t.Fatalf("request path = %q, want /payments/agent/version", gotPath)
	}
}

func TestRedirectWithinBasePathAgentAllowed(t *testing.T) {
	var mux http.ServeMux
	srv := httptest.NewServer(&mux)
	defer srv.Close()
	mux.HandleFunc("/payments/agent/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/payments/agent/new", http.StatusFound)
	})
	mux.HandleFunc("/payments/agent/new", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "landed")
	})
	c := newClient(t, srv.URL+"/payments", 5*time.Second)
	resp, err := c.Get(context.Background(), "/agent/old", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != "landed" {
		t.Fatalf("body %q", resp.Body)
	}
}

func TestRedirectOutsideBasePathAgentBlocked(t *testing.T) {
	var mux http.ServeMux
	srv := httptest.NewServer(&mux)
	defer srv.Close()
	var rootAgentHit atomic.Bool
	mux.HandleFunc("/payments/agent/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/agent/escape", http.StatusFound)
	})
	mux.HandleFunc("/agent/escape", func(w http.ResponseWriter, r *http.Request) {
		rootAgentHit.Store(true)
	})
	c := newClient(t, srv.URL+"/payments", 5*time.Second)
	_, err := c.Get(context.Background(), "/agent/old", "")
	var te *TransportError
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "refusing redirect outside /payments/agent") {
		t.Fatalf("err = %v, want refusal outside /payments/agent", err)
	}
	if rootAgentHit.Load() {
		t.Fatal("redirect target outside base path was requested")
	}
}

func TestPercentEncodedPathNotDoubleEncoded(t *testing.T) {
	var gotRequestURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestURI = r.RequestURI
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	c := newClient(t, srv.URL, 5*time.Second)
	if _, err := c.Get(context.Background(), "/agent/trace-handoff/a%20b", ""); err != nil {
		t.Fatal(err)
	}
	if gotRequestURI != "/agent/trace-handoff/a%20b" {
		t.Fatalf("wire path = %q, want %q (double-encoding regression)", gotRequestURI, "/agent/trace-handoff/a%20b")
	}
}

func TestBarePercentInPathEncodedOnce(t *testing.T) {
	var gotRequestURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestURI = r.RequestURI
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	c := newClient(t, srv.URL, 5*time.Second)
	// "100%" is not a valid escape sequence; the literal '%' must be encoded
	// exactly once on the wire.
	if _, err := c.Get(context.Background(), "/agent/logs-100%", ""); err != nil {
		t.Fatal(err)
	}
	if gotRequestURI != "/agent/logs-100%25" {
		t.Fatalf("wire path = %q, want %q", gotRequestURI, "/agent/logs-100%25")
	}
}

func TestPercentEncodedPathWithBasePrefix(t *testing.T) {
	var gotRequestURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestURI = r.RequestURI
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()
	c := newClient(t, srv.URL+"/svc", 5*time.Second)
	if _, err := c.Get(context.Background(), "/agent/x%20y", "limit=5"); err != nil {
		t.Fatal(err)
	}
	if gotRequestURI != "/svc/agent/x%20y?limit=5" {
		t.Fatalf("wire = %q, want %q", gotRequestURI, "/svc/agent/x%20y?limit=5")
	}
}
