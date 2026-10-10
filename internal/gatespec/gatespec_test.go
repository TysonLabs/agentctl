package gatespec

import (
	"strings"
	"testing"
	"time"
)

func TestFromTableShapes(t *testing.T) {
	// An inline array of inline tables decodes as []any.
	sp, err := FromTable("p", map[string]any{"steps": []any{
		map[string]any{"name": "a", "run": "true"},
		map[string]any{"name": "b", "run": "make", "timeout": "1h", "stop_on_fail": true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sp.Steps) != 2 || sp.Steps[0].Timeout != DefaultTimeout || sp.Steps[1].Timeout != time.Hour || !sp.Steps[1].StopOnFail {
		t.Fatalf("spec %+v", sp)
	}
	for name, tbl := range map[string]any{
		"not a table":  "x",
		"steps scalar": map[string]any{"steps": "x"},
		"step scalar":  map[string]any{"steps": []any{"x"}},
		"lock int":     map[string]any{"lock": 1},
		"timeout int":  map[string]any{"steps": []any{map[string]any{"name": "a", "run": "x", "timeout": 5}}},
		"timeout big":  map[string]any{"steps": []any{map[string]any{"name": "a", "run": "x", "timeout": "25h"}}},
		"timeout zero": map[string]any{"steps": []any{map[string]any{"name": "a", "run": "x", "timeout": "0s"}}},
		"name missing": map[string]any{"steps": []any{map[string]any{"run": "x"}}},
		"dash name":    map[string]any{"steps": []any{map[string]any{"name": "-a", "run": "x"}}},
		"dot name":     map[string]any{"steps": []any{map[string]any{"name": ".a", "run": "x"}}},
		"control run":  map[string]any{"steps": []any{map[string]any{"name": "a", "run": "x\ry"}}},
		"long run":     map[string]any{"steps": []any{map[string]any{"name": "a", "run": strings.Repeat("x", MaxRunBytes+1)}}},
		"dot-dot lock": map[string]any{"lock": ".."},
		"unknown key":  map[string]any{"lokc": "x"},
	} {
		if _, err := FromTable("p", tbl); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := CheckRun("echo\ta"); err != nil {
		t.Errorf("tab refused: %v", err)
	}
}
