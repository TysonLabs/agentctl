package cfg

import (
	"fmt"
	"strings"

	"github.com/TysonLabs/agentctl/internal/gatespec"
)

// GateStep is a step as agentcfg writes it. Timeout "" means the default.
type GateStep struct {
	Name       string `json:"name"`
	Run        string `json:"run"`
	Timeout    string `json:"timeout"`
	StopOnFail bool   `json:"stop_on_fail"`
}

func (st GateStep) check() error {
	if err := gatespec.CheckStepName(st.Name); err != nil {
		return err
	}
	if err := gatespec.CheckRun(st.Run); err != nil {
		return err
	}
	_, err := gatespec.ParseTimeout(st.Timeout)
	return err
}

func (st GateStep) table() map[string]any {
	m := map[string]any{"name": st.Name, "run": st.Run}
	if st.Timeout != "" {
		m["timeout"] = st.Timeout
	}
	if st.StopOnFail {
		m["stop_on_fail"] = true
	}
	return m
}

// gateEdit runs fn on name's [name.gate] table and its steps, then validates
// the whole table with gatespec (agentflow's own rules) before anything is
// written. create allows a new project and a new gate table. A gate left
// with no lock and no steps is removed.
func (s *Store) gateEdit(expect, name string, create bool, fn func(gate map[string]any, steps []map[string]any) ([]map[string]any, error)) (*Result, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	return s.Edit(expect, func(d *Doc) error {
		svc, err := d.service(name, create)
		if err != nil {
			return err
		}
		gate, ok := svc["gate"].(map[string]any)
		if !ok {
			if _, exists := svc["gate"]; exists {
				return fmt.Errorf("%s.gate is not a table; fix the file by hand", name)
			}
			if !create {
				return fmt.Errorf("no [%s.gate] table — add a step first: agentcfg gate %s --add NAME --run CMD", name, name)
			}
			gate = map[string]any{}
		}
		steps, err := gatespec.StepTables(gate["steps"])
		if err != nil {
			return fmt.Errorf("[%s.gate] %v; fix the file by hand", name, err)
		}
		// Work on a copy so a refused edit leaves the decoded tree as it was.
		steps = append([]map[string]any(nil), steps...)
		steps, err = fn(gate, steps)
		if err != nil {
			return err
		}
		if len(steps) == 0 {
			delete(gate, "steps")
		} else {
			gate["steps"] = steps
		}
		if _, err := gatespec.FromTable(name, gate); err != nil {
			return err
		}
		if len(gate) == 0 {
			delete(svc, "gate")
		} else {
			svc["gate"] = gate
		}
		if len(svc) == 0 {
			delete(d.Tree, name)
		}
		return nil
	})
}

func stepIndex(steps []map[string]any, name string) int {
	for i, st := range steps {
		if st["name"] == name {
			return i
		}
	}
	return -1
}

// AddGateStep inserts a step at 1-based position at (0 appends), creating
// the project and its gate if needed.
func (s *Store) AddGateStep(expect, name string, st GateStep, at int) (*Result, error) {
	if err := st.check(); err != nil {
		return nil, err
	}
	return s.gateEdit(expect, name, true, func(_ map[string]any, steps []map[string]any) ([]map[string]any, error) {
		if stepIndex(steps, st.Name) >= 0 {
			return nil, fmt.Errorf("%s already has a step %q; edit or remove it", name, st.Name)
		}
		i := len(steps)
		if at != 0 {
			if at < 1 || at > len(steps)+1 {
				return nil, fmt.Errorf("position %d is out of range 1-%d", at, len(steps)+1)
			}
			i = at - 1
		}
		return append(steps[:i], append([]map[string]any{st.table()}, steps[i:]...)...), nil
	})
}

// EditGateStep replaces step old with st (which may rename it), keeping its
// position.
func (s *Store) EditGateStep(expect, name, old string, st GateStep) (*Result, error) {
	if err := st.check(); err != nil {
		return nil, err
	}
	return s.gateEdit(expect, name, false, func(_ map[string]any, steps []map[string]any) ([]map[string]any, error) {
		i := stepIndex(steps, old)
		if i < 0 {
			return nil, fmt.Errorf("%s has no step %q", name, old)
		}
		if j := stepIndex(steps, st.Name); j >= 0 && j != i {
			return nil, fmt.Errorf("%s already has a step %q", name, st.Name)
		}
		steps[i] = st.table()
		return steps, nil
	})
}

// MoveGateStep moves a step to 1-based position to.
func (s *Store) MoveGateStep(expect, name, step string, to int) (*Result, error) {
	return s.gateEdit(expect, name, false, func(_ map[string]any, steps []map[string]any) ([]map[string]any, error) {
		i := stepIndex(steps, step)
		if i < 0 {
			return nil, fmt.Errorf("%s has no step %q", name, step)
		}
		if to < 1 || to > len(steps) {
			return nil, fmt.Errorf("position %d is out of range 1-%d", to, len(steps))
		}
		moved := steps[i]
		rest := append(append([]map[string]any{}, steps[:i]...), steps[i+1:]...)
		j := to - 1
		return append(rest[:j], append([]map[string]any{moved}, rest[j:]...)...), nil
	})
}

// RemoveGateStep deletes one step.
func (s *Store) RemoveGateStep(expect, name, step string) (*Result, error) {
	return s.gateEdit(expect, name, false, func(_ map[string]any, steps []map[string]any) ([]map[string]any, error) {
		i := stepIndex(steps, step)
		if i < 0 {
			return nil, fmt.Errorf("%s has no step %q", name, step)
		}
		return append(steps[:i], steps[i+1:]...), nil
	})
}

// SetGateLock sets (or, with "", clears) the gate's named lock.
func (s *Store) SetGateLock(expect, name, lock string) (*Result, error) {
	lock = strings.TrimSpace(lock)
	if err := gatespec.CheckLockName(lock); err != nil {
		return nil, err
	}
	return s.gateEdit(expect, name, lock != "", func(gate map[string]any, steps []map[string]any) ([]map[string]any, error) {
		if lock == "" {
			delete(gate, "lock")
		} else {
			gate["lock"] = lock
		}
		return steps, nil
	})
}

// RemoveGate deletes the whole [name.gate] table. meta stays unless nothing
// else uses it (no env left).
func (s *Store) RemoveGate(expect, name string) (*Result, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	return s.Edit(expect, func(d *Doc) error {
		svc, err := d.service(name, false)
		if err != nil {
			return err
		}
		if _, ok := svc["gate"]; !ok {
			return fmt.Errorf("no [%s.gate] table", name)
		}
		delete(svc, "gate")
		hasEnv := false
		for k := range svc {
			if !reservedEnvs[k] {
				hasEnv = true
			}
		}
		if _, hasAnnounce := svc["announce"]; !hasEnv && !hasAnnounce {
			delete(svc, "meta")
		}
		if len(svc) == 0 {
			delete(d.Tree, name)
		}
		return nil
	})
}
