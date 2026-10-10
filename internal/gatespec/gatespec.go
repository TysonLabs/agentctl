// Package gatespec is the schema of a project's [name.gate] table in
// services.toml: the ordered steps `agentflow gate` runs and the named lock
// it holds. agentcfg (which writes the table) and agentflow (which runs it)
// share these rules, so the two can never disagree about what is valid. The
// package holds no secrets and imports nothing from either binary.
package gatespec

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// DefaultTimeout bounds a step that sets no timeout.
const DefaultTimeout = 30 * time.Minute

// MaxTimeout is the largest timeout a step may set.
const MaxTimeout = 24 * time.Hour

// MaxSteps caps the number of steps in one gate.
const MaxSteps = 64

// MaxRunBytes caps one step's command.
const MaxRunBytes = 4096

var (
	// StepNameRe is a step name. It becomes a log file name, so it has no
	// path separators or leading dot.
	StepNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,63}$`)
	// LockNameRe is a named lock. It becomes part of a file name in the
	// temp dir, shared with `agentflow lock run`.
	LockNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// Step is one [[name.gate.steps]] entry.
type Step struct {
	Name       string        `json:"name"`
	Run        string        `json:"run"`
	Timeout    time.Duration `json:"-"`
	TimeoutRaw string        `json:"timeout,omitempty"` // as written; "" means DefaultTimeout
	StopOnFail bool          `json:"stop_on_fail,omitempty"`
}

// Spec is one validated [name.gate] table.
type Spec struct {
	Lock  string `json:"lock,omitempty"`
	Steps []Step `json:"steps"`
}

var gateKeys = map[string]bool{"lock": true, "steps": true}
var stepKeys = map[string]bool{"name": true, "run": true, "timeout": true, "stop_on_fail": true}

// CheckStepName validates a step name.
func CheckStepName(name string) error {
	if !StepNameRe.MatchString(name) {
		return fmt.Errorf("step name %q must be 1-64 letters, digits, _ or - (not starting with -)", name)
	}
	return nil
}

// CheckLockName validates a lock name ("" means no lock).
func CheckLockName(name string) error {
	if name == "" {
		return nil
	}
	if !LockNameRe.MatchString(name) || name == "." || name == ".." {
		return fmt.Errorf("lock name %q must be 1-64 letters, digits, ., _ or -", name)
	}
	return nil
}

// CheckRun validates a step command: non-empty, one line, no control
// characters other than tab.
func CheckRun(run string) error {
	if strings.TrimSpace(run) == "" {
		return fmt.Errorf("run is empty")
	}
	if len(run) > MaxRunBytes {
		return fmt.Errorf("run is longer than %d bytes", MaxRunBytes)
	}
	if strings.ContainsFunc(run, func(r rune) bool { return r != '\t' && unicode.IsControl(r) }) {
		return fmt.Errorf("run must be one line without control characters")
	}
	return nil
}

// ParseTimeout parses a step timeout; "" is DefaultTimeout.
func ParseTimeout(raw string) (time.Duration, error) {
	if raw == "" {
		return DefaultTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 || d > MaxTimeout {
		return 0, fmt.Errorf("timeout %q must be a positive duration up to 24h, like 30m", raw)
	}
	return d, nil
}

// FromTable validates a decoded [name.gate] table (the generic shape a TOML
// decoder produces) and returns it in order. Unknown keys are errors: a
// misspelt stop_on_fail must not silently run every step.
func FromTable(name string, raw any) (Spec, error) {
	where := "[" + name + ".gate]"
	tbl, ok := raw.(map[string]any)
	if !ok {
		return Spec{}, fmt.Errorf("%s must be a table", where)
	}
	for k := range tbl {
		if !gateKeys[k] {
			return Spec{}, fmt.Errorf("%s has an unknown key %q (want lock, steps)", where, k)
		}
	}
	var sp Spec
	if v, ok := tbl["lock"]; ok {
		s, isStr := v.(string)
		if !isStr {
			return Spec{}, fmt.Errorf("%s lock must be a string", where)
		}
		if err := CheckLockName(s); err != nil {
			return Spec{}, fmt.Errorf("%s %v", where, err)
		}
		sp.Lock = s
	}
	steps, err := StepTables(tbl["steps"])
	if err != nil {
		return Spec{}, fmt.Errorf("%s %v", where, err)
	}
	if len(steps) > MaxSteps {
		return Spec{}, fmt.Errorf("%s has more than %d steps", where, MaxSteps)
	}
	seen := map[string]bool{}
	sp.Steps = []Step{}
	for i, st := range steps {
		at := fmt.Sprintf("%s step %d", where, i+1)
		for k := range st {
			if !stepKeys[k] {
				return Spec{}, fmt.Errorf("%s has an unknown key %q (want name, run, timeout, stop_on_fail)", at, k)
			}
		}
		var s Step
		var isStr bool
		if s.Name, isStr = st["name"].(string); !isStr {
			return Spec{}, fmt.Errorf("%s needs a name (a string)", at)
		}
		if err := CheckStepName(s.Name); err != nil {
			return Spec{}, fmt.Errorf("%s: %v", at, err)
		}
		if seen[s.Name] {
			return Spec{}, fmt.Errorf("%s: step name %q is used twice", where, s.Name)
		}
		seen[s.Name] = true
		at = fmt.Sprintf("%s step %q", where, s.Name)
		if s.Run, isStr = st["run"].(string); !isStr {
			return Spec{}, fmt.Errorf("%s needs run (a string)", at)
		}
		if err := CheckRun(s.Run); err != nil {
			return Spec{}, fmt.Errorf("%s: %v", at, err)
		}
		if v, ok := st["timeout"]; ok {
			if s.TimeoutRaw, isStr = v.(string); !isStr {
				return Spec{}, fmt.Errorf("%s timeout must be a string such as \"30m\"", at)
			}
		}
		if s.Timeout, err = ParseTimeout(s.TimeoutRaw); err != nil {
			return Spec{}, fmt.Errorf("%s: %v", at, err)
		}
		if v, ok := st["stop_on_fail"]; ok {
			b, isBool := v.(bool)
			if !isBool {
				return Spec{}, fmt.Errorf("%s stop_on_fail must be true or false", at)
			}
			s.StopOnFail = b
		}
		sp.Steps = append(sp.Steps, s)
	}
	return sp, nil
}

// StepTables returns the steps array in order. A TOML decoder yields an
// array of tables as []map[string]any; an inline array of inline tables may
// come back as []any. A missing steps key is an empty gate.
func StepTables(raw any) ([]map[string]any, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case []map[string]any:
		return v, nil
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, e := range v {
			m, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("steps must be an array of tables ([[name.gate.steps]])")
			}
			out = append(out, m)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("steps must be an array of tables ([[name.gate.steps]])")
	}
}
