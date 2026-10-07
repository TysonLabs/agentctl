// Package ui serves agentcfg's settings page on 127.0.0.1.
//
// The server holds write access to tokens, so it trusts nothing about the
// browser it talks to:
//   - it binds 127.0.0.1 only, on a random port;
//   - every /api call carries a per-launch key (32 random bytes) in the
//     X-Agentcfg-Key header. The key reaches the page in the URL fragment,
//     which browsers never send to a server or in a Referer;
//   - the Host header must be exactly 127.0.0.1:<port>, which defeats DNS
//     rebinding (a rebound name arrives with its own Host);
//   - a write must carry a JSON content type and, when present, our Origin;
//     cross-site pages cannot set the key header without a CORS preflight,
//     and the server answers no preflight;
//   - responses never hold a token, only fingerprints; CSP allows only the
//     page's own scripts and styles;
//   - it exits after Idle without an /api call, or on Quit.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os/exec"
	"path"
	"runtime"
	"sync"
	"time"

	"github.com/TysonLabs/agentctl/internal/cfg"
)

//go:embed assets
var assets embed.FS

// maxBody caps request bodies; the largest is a token.
const maxBody = 64 << 10

// Options configures Serve.
type Options struct {
	Store   *cfg.Store
	Version string
	Idle    time.Duration
	Open    bool      // launch the default browser
	Stdout  io.Writer // where the URL is printed
	// Ready, when set, receives the page URL once the server listens (tests).
	Ready func(url string)
}

type server struct {
	store   *cfg.Store
	version string
	key     string
	host    string // "127.0.0.1:<port>"
	mu      sync.Mutex
	last    time.Time
	quit    chan struct{}
	once    sync.Once
}

// Serve runs the settings page until it is idle for opts.Idle or the page
// asks it to quit.
func Serve(opts Options) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listening on 127.0.0.1: %v", err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		ln.Close()
		return errors.New("refusing to serve on a non-loopback address")
	}
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		ln.Close()
		return fmt.Errorf("generating the session key: %v", err)
	}
	s := &server{
		store:   opts.Store,
		version: opts.Version,
		key:     hex.EncodeToString(keyBytes),
		host:    addr.String(),
		last:    time.Now(),
		quit:    make(chan struct{}),
	}
	hs := &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}
	url := "http://" + s.host + "/#k=" + s.key
	fmt.Fprintf(opts.Stdout, "agentcfg: settings page at %s\n", url)
	fmt.Fprintf(opts.Stdout, "agentcfg: editing %s; stops after %s idle or when you press Done (Ctrl-C also stops it)\n", s.store.Path, opts.Idle)
	if opts.Ready != nil {
		opts.Ready(url)
	}
	if opts.Open {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(opts.Stdout, "agentcfg: could not open a browser (%v); open the URL above\n", err)
		}
	}

	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	tick := time.NewTicker(min(opts.Idle/4, 30*time.Second))
	defer tick.Stop()
	for {
		select {
		case err := <-errc:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-s.quit:
			fmt.Fprintln(opts.Stdout, "agentcfg: done")
			return shutdown(hs)
		case <-tick.C:
			s.mu.Lock()
			idle := time.Since(s.last)
			s.mu.Unlock()
			if idle >= opts.Idle {
				fmt.Fprintf(opts.Stdout, "agentcfg: idle for %s, stopping\n", opts.Idle)
				return shutdown(hs)
			}
		}
	}
}

func shutdown(hs *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(ctx)
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Run()
	case "linux":
		return exec.Command("xdg-open", url).Run()
	default:
		return fmt.Errorf("no browser launcher for %s", runtime.GOOS)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "assets")
	mux.HandleFunc("GET /{$}", s.asset(static, "index.html"))
	mux.HandleFunc("GET /app.js", s.asset(static, "app.js"))
	mux.HandleFunc("GET /app.css", s.asset(static, "app.css"))
	mux.HandleFunc("GET /favicon.svg", s.asset(static, "favicon.svg"))

	mux.HandleFunc("GET /api/state", s.api(s.handleState))
	mux.HandleFunc("POST /api/env", s.api(s.handleEnv))
	mux.HandleFunc("POST /api/meta", s.api(s.handleMeta))
	mux.HandleFunc("POST /api/token", s.api(s.handleToken))
	mux.HandleFunc("POST /api/remove", s.api(s.handleRemove))
	mux.HandleFunc("POST /api/migrate", s.api(s.handleMigrate))
	mux.HandleFunc("POST /api/test", s.api(s.handleTest))
	mux.HandleFunc("POST /api/quit", s.api(s.handleQuit))
	return s.guard(mux)
}

// guard applies the Host check and the security headers to every request.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if r.Host != s.host {
			http.Error(w, "wrong host", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) asset(static fs.FS, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(static, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
		w.Write(data)
	}
}

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(err error) error { return &apiError{http.StatusBadRequest, err.Error()} }

// api checks the session key (and, for writes, the content type and Origin),
// records activity, and writes the handler's value or error as JSON.
func (s *server) api(fn func(r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Agentcfg-Key")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.key)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or wrong session key — reopen the URL agentcfg printed"})
			return
		}
		if r.Method != http.MethodGet {
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+s.host {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request refused"})
				return
			}
			if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
				writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "want application/json"})
				return
			}
		}
		s.mu.Lock()
		s.last = time.Now()
		s.mu.Unlock()
		v, err := fn(r)
		if err != nil {
			status := http.StatusBadRequest
			var ae *apiError
			switch {
			case errors.As(err, &ae):
				status = ae.status
			case errors.Is(err, cfg.ErrConflict):
				status = http.StatusConflict
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest(fmt.Errorf("bad request body: %v", err))
	}
	if dec.More() {
		return badRequest(errors.New("bad request body: trailing data"))
	}
	return nil
}

// stateReply is every write's answer: the fresh state plus any warnings.
type stateReply struct {
	*cfg.State
	AppVersion string   `json:"app_version"`
	Notes      []string `json:"notes,omitempty"`
}

func (s *server) reply(notes ...string) *stateReply {
	return &stateReply{State: s.store.State(), AppVersion: s.version, Notes: notes}
}

func (s *server) handleState(*http.Request) (any, error) { return s.reply(), nil }

func (s *server) handleEnv(r *http.Request) (any, error) {
	var req struct {
		Version string `json:"version"`
		Service string `json:"service"` // name.env
		BaseURL string `json:"base_url"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	res, err := s.store.SetBaseURL(req.Version, req.Service, req.BaseURL)
	if err != nil {
		return nil, err
	}
	return s.reply(res.Warnings...), nil
}

func (s *server) handleMeta(r *http.Request) (any, error) {
	var req struct {
		Version string            `json:"version"`
		Name    string            `json:"name"`
		Meta    map[string]string `json:"meta"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	res, err := s.store.SetMeta(req.Version, req.Name, req.Meta)
	if err != nil {
		return nil, err
	}
	return s.reply(res.Warnings...), nil
}

func (s *server) handleToken(r *http.Request) (any, error) {
	var req struct {
		Version string `json:"version"`
		Service string `json:"service"`
		Token   string `json:"token"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	res, err := s.store.SetToken(req.Version, req.Service, req.Token)
	if err != nil {
		return nil, err
	}
	return s.reply(res.Warnings...), nil
}

func (s *server) handleRemove(r *http.Request) (any, error) {
	var req struct {
		Version string `json:"version"`
		Service string `json:"service"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	res, err := s.store.Remove(req.Version, req.Service)
	if err != nil {
		return nil, err
	}
	return s.reply(res.Warnings...), nil
}

func (s *server) handleMigrate(r *http.Request) (any, error) {
	var req struct {
		Version string `json:"version"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	mr, err := s.store.Migrate(req.Version)
	if err != nil {
		return nil, err
	}
	var notes []string
	if len(mr.Moved) > 0 {
		notes = append(notes, fmt.Sprintf("Moved %d token(s) into the Keychain.", len(mr.Moved)))
		notes = append(notes, mr.Result.Warnings...)
	}
	for _, sk := range mr.Skipped {
		notes = append(notes, "Skipped "+sk)
	}
	return s.reply(notes...), nil
}

func (s *server) handleTest(r *http.Request) (any, error) {
	var req struct {
		Service string `json:"service"`
	}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	if _, _, err := cfg.SplitFull(req.Service); err != nil {
		return nil, badRequest(err)
	}
	return s.store.Test(req.Service, "agentcfg/"+s.version, 10*time.Second), nil
}

func (s *server) handleQuit(r *http.Request) (any, error) {
	var req struct{}
	if err := decode(r, &req); err != nil {
		return nil, err
	}
	s.once.Do(func() { close(s.quit) })
	return map[string]bool{"ok": true}, nil
}
