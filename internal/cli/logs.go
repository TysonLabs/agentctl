package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/TysonLabs/agentctl/internal/client"
	"github.com/TysonLabs/agentctl/internal/render"
)

// logOpts are the flags only `logs` accepts.
type logOpts struct {
	used     bool
	q        string
	level    string
	since    string
	limit    int
	json     bool
	wait     time.Duration
	interval time.Duration
}

var logLevels = map[string]bool{"error": true, "warn": true, "info": true, "debug": true, "trace": true}

func (l *logOpts) set(name string, hasVal bool, val string, take func() (string, error)) error {
	l.used = true
	if name == "json" {
		if hasVal {
			return usageError("--json takes no value")
		}
		l.json = true
		return nil
	}
	v, err := take()
	if err != nil {
		return err
	}
	switch name {
	case "q":
		l.q = v
	case "level":
		v = strings.ToLower(v)
		if v == "warning" {
			v = "warn"
		}
		if !logLevels[v] {
			return usageError("invalid --level " + strconv.Quote(v) + ": want error, warn, info, debug or trace")
		}
		l.level = v
	case "since":
		l.since = v
	case "limit":
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return usageError("invalid --limit value: " + v)
		}
		l.limit = n
	case "wait", "interval":
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return usageError("invalid --" + name + " value: " + v)
		}
		if name == "wait" {
			l.wait = d
		} else {
			l.interval = d
		}
	}
	return nil
}

// waitTimeoutError: --wait ended without a matching entry (exit 4).
type waitTimeoutError struct{ msg string }

func (e *waitTimeoutError) Error() string { return e.msg }

// sinceTime resolves --since: a duration back from now, or an RFC 3339 time.
func sinceTime(v string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(v); err == nil {
		if d <= 0 {
			return time.Time{}, usageError("--since duration must be positive: " + v)
		}
		return now.Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, usageError("invalid --since " + strconv.Quote(v) + ": want a duration (30m, 2h) or an RFC 3339 time")
	}
	return t, nil
}

func (a *app) cmdLogs(args []string) error {
	if len(args) != 1 {
		return usageError("usage: agentctl logs <service.env> [--q TEXT] [--level LEVEL] [--since WHEN] [--limit N] [--json] [--wait DUR]")
	}
	name := args[0]
	lo := a.opts.logs
	if lo.interval != 0 && lo.wait == 0 {
		return usageError("--interval only applies with --wait")
	}
	if lo.interval == 0 {
		lo.interval = 15 * time.Second
	}

	reg, err := a.loadRegistry()
	if err != nil {
		return err
	}
	svc, err := a.requireWired(reg, name)
	if err != nil {
		return err
	}
	c, err := a.newClient(svc, a.opts.timeout)
	if err != nil {
		return err
	}

	start := time.Now()
	var since time.Time
	if lo.since != "" {
		if since, err = sinceTime(lo.since, start); err != nil {
			return err
		}
	} else if lo.wait > 0 {
		// Only entries logged after the wait began count as a match.
		since = start
	}
	query := url.Values{}
	if lo.q != "" {
		query.Set("q", lo.q)
	}
	if lo.level != "" {
		query.Set("level", lo.level)
	}
	if !since.IsZero() {
		query.Set("since", since.UTC().Format(time.RFC3339Nano))
	}
	if lo.limit > 0 {
		query.Set("limit", strconv.Itoa(lo.limit))
	}
	p, _, err := client.NormalizePath("/agent/logs")
	if err != nil {
		return usageError(err.Error())
	}

	fetch := func() ([]render.LogEntry, []byte, error) {
		resp, err := c.Get(context.Background(), p, query.Encode())
		if err != nil {
			return nil, nil, err
		}
		if resp.Status >= 400 {
			return nil, resp.Body, &httpError{status: resp.Status, msg: fmt.Sprintf("HTTP %d from %s %s", resp.Status, name, p)}
		}
		entries, ok := render.ParseLogs(resp.Body)
		if !ok {
			return nil, resp.Body, usageError(name + " /agent/logs did not return an entries list; see: agentctl get " + name + " logs")
		}
		return entries, resp.Body, nil
	}

	if lo.wait == 0 {
		entries, body, err := fetch()
		if err != nil {
			if body != nil {
				a.printBody(body)
			}
			return err
		}
		return a.printLogs(entries)
	}

	deadline := start.Add(lo.wait)
	var lastErr error
	for {
		entries, _, err := fetch()
		switch {
		case err == nil && len(entries) > 0:
			return a.printLogs(entries)
		case err == nil:
			lastErr = nil
		case retryableWait(err):
			lastErr = err // the service may be restarting
		default:
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			msg := fmt.Sprintf("no matching entry in %s after %s", name, lo.wait)
			if lastErr != nil {
				msg += " (last error: " + lastErr.Error() + ")"
			}
			return &waitTimeoutError{msg: msg}
		}
		// The last poll lands on the deadline, not an interval short of it.
		time.Sleep(min(lo.interval, remaining))
	}
}

// retryableWait: transport errors and 5xx can be a restart mid-deploy;
// anything else (401, 403, 404, bad config) will not fix itself.
func retryableWait(err error) bool {
	var te *client.TransportError
	var he *httpError
	return errors.As(err, &te) || (errors.As(err, &he) && he.status >= 500)
}

func (a *app) printLogs(entries []render.LogEntry) error {
	if a.opts.logs.json {
		if entries == nil {
			entries = []render.LogEntry{}
		}
		out, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return err
		}
		a.stdout.Write(append(out, '\n'))
		return nil
	}
	if len(entries) == 0 {
		a.errf("no matching entries")
		return nil
	}
	render.LogLines(a.stdout, entries)
	return nil
}
