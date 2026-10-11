package coderabbit

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Retry configures RunRetrying's handling of a rate-limited run.
type Retry struct {
	// Retries is how many extra runs a rate limit may cost (0: none).
	Retries int
	// MaxWait caps one wait. A stated wait longer than MaxWait is not
	// waited out: a retry before CodeRabbit's own wait ends would only be
	// refused again.
	MaxWait time.Duration
	// DefaultWait is used when the CLI names no wait, or one that does not
	// parse.
	DefaultWait time.Duration
	// Jitter returns a small extra delay, so runs that hit one limit together
	// do not retry together. nil means none.
	Jitter func() time.Duration
	// Sleep waits for d or until ctx ends (it then returns ctx's error).
	// nil means a real timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Log receives one line per wait. nil discards it.
	Log func(msg string)
}

// DefaultJitter returns a random delay of up to 15 seconds.
func DefaultJitter() time.Duration { return rand.N(15 * time.Second) }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RunRetrying runs the review and, while the result is rate_limited and
// retries are left, waits the time CodeRabbit names (plus jitter) and runs
// it again. Each earlier attempt's events and stderr are kept as
// events.attemptN.jsonl and stderr.attemptN.log. The result is the last
// attempt's, with Attempts and RateLimitWaitS set. Cancelling ctx during a
// wait ends with status interrupted.
func RunRetrying(ctx context.Context, o Options, r Retry) (Result, error) {
	sleep := r.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	log := r.Log
	if log == nil {
		log = func(string) {}
	}
	var waited time.Duration
	var lastRateLimit struct {
		wait       string
		retryAfter *float64
		guidance   string
		set        bool
	}
	applyLastRateLimit := func(res *Result) {
		if !lastRateLimit.set || res.Status == StatusRateLimited {
			return
		}
		res.RateLimitWait = lastRateLimit.wait
		res.RetryAfterS = lastRateLimit.retryAfter
		res.RateLimitGuidance = lastRateLimit.guidance
	}
	for attempt := 1; ; attempt++ {
		res, err := Run(ctx, o)
		res.RateLimitWaitS = waited.Seconds()
		if err != nil {
			// Run returns an error only when CodeRabbit never started, so this
			// failed start is not an attempt.
			res.Attempts = attempt - 1
			applyLastRateLimit(&res)
			return res, err
		}
		res.Attempts = attempt
		if res.Status != StatusRateLimited {
			applyLastRateLimit(&res)
			return res, nil
		}
		lastRateLimit.wait = res.RateLimitWait
		lastRateLimit.retryAfter = res.RetryAfterS
		lastRateLimit.guidance = res.RateLimitGuidance
		lastRateLimit.set = true
		if attempt > r.Retries {
			return res, nil
		}
		wait, ok := ParseWait(res.RateLimitWait)
		if !ok {
			wait = r.DefaultWait
		}
		if wait > r.MaxWait {
			res.Error = fmt.Sprintf("%s; not retried: the wait (%s) is longer than the rate-limit max wait (%s)", res.Error, wait, r.MaxWait)
			return res, nil
		}
		if r.Jitter != nil {
			wait = min(wait+r.Jitter(), r.MaxWait)
		}
		// Keep this attempt's logs; the next attempt writes fresh ones.
		res.Events = keepAttempt(res.Events, attempt)
		res.Stderr = keepAttempt(res.Stderr, attempt)
		stated := res.RateLimitWait
		if stated == "" {
			stated = "none stated"
		}
		log(fmt.Sprintf("rate limited (attempt %d of %d; CodeRabbit wait: %s): retrying in %s", attempt, r.Retries+1, stated, wait.Round(time.Second)))
		start := time.Now()
		if err := sleep(ctx, wait); err != nil {
			res.RateLimitWaitS = (waited + time.Since(start)).Seconds()
			res.Status = StatusInterrupted
			res.Error = "interrupted during the rate-limit wait"
			return res, nil
		}
		waited += wait
	}
}

// keepAttempt renames path (events.jsonl) to events.attemptN.jsonl and
// returns the new path, or path itself if the rename fails.
func keepAttempt(path string, attempt int) string {
	ext := filepath.Ext(path)
	kept := strings.TrimSuffix(path, ext) + ".attempt" + strconv.Itoa(attempt) + ext
	if os.Rename(path, kept) != nil {
		return path
	}
	return kept
}

var (
	waitPartRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)\b`)
	waitJoinRe = regexp.MustCompile(`(?i)^(?:\s|,|and)*$`)
	numberRe   = regexp.MustCompile(`^\d+(?:\.\d+)?$`)
)

// ParseWait reads a CodeRabbit waitTime: "12 minutes and 13 seconds",
// "45 minutes", "1 hour, 2 minutes", "30 seconds", "12m13s" or a bare number
// of seconds. Any text it does not fully understand is a failed parse (ok
// false), never a guess.
func ParseWait(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if numberRe.MatchString(s) {
		f, err := strconv.ParseFloat(s, 64)
		return time.Duration(f * float64(time.Second)), err == nil
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return d, true
	}
	matches := waitPartRe.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return 0, false
	}
	var total time.Duration
	prev := 0
	for _, m := range matches {
		// Only separators may stand between the parts.
		if !waitJoinRe.MatchString(s[prev:m[0]]) {
			return 0, false
		}
		prev = m[1]
		f, err := strconv.ParseFloat(s[m[2]:m[3]], 64)
		if err != nil {
			return 0, false
		}
		unit := time.Second
		switch u := strings.ToLower(s[m[4]:m[5]]); {
		case strings.HasPrefix(u, "h"):
			unit = time.Hour
		case strings.HasPrefix(u, "m"):
			unit = time.Minute
		}
		total += time.Duration(f * float64(unit))
	}
	if !waitJoinRe.MatchString(s[prev:]) {
		return 0, false
	}
	return total, true
}
