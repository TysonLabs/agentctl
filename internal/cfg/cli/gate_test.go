package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGateCommand(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "services.toml")
	c := func(args ...string) (int, string, string) {
		return run(t, "", append(args, "--config", cfgPath)...)
	}
	file := func() string {
		b, _ := os.ReadFile(cfgPath)
		return string(b)
	}
	ok := func(args ...string) string {
		t.Helper()
		code, out, errs := c(args...)
		if code != 0 {
			t.Fatalf("%q: exit %d: %s", args, code, errs)
		}
		return out
	}
	ok("meta", "tool", "repo=~/src/tool")
	ok("gate", "tool", "--add", "test", "--run", "go test ./...")
	ok("gate", "tool", "--add", "fmt", "--run", "gofmt -l .", "--at", "1", "--timeout", "5m")
	ok("gate", "tool", "--add", "build", "--run", "make build", "--stop-on-fail")
	ok("gate", "tool", "--lock", "tool-build")
	ok("gate", "tool", "--move", "build", "--to", "1")
	out := ok("gate", "tool")
	for _, want := range []string{"lock: tool-build", "repo: ~/src/tool", "1  build", "2  fmt    5m", "3  test   30m (default)", "make build"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing lacks %q:\n%s", want, out)
		}
	}
	ok("gate", "tool", "--edit", "test", "--timeout", "45m", "--stop-on-fail")
	ok("gate", "tool", "--edit", "build", "--no-stop-on-fail")
	f := file()
	if !strings.Contains(f, "timeout = \"45m\"") || strings.Count(f, "stop_on_fail = true") != 1 || !strings.Contains(f, "run = \"go test ./...\"") {
		t.Fatalf("edit did not keep unnamed fields:\n%s", f)
	}
	if i, j := strings.Index(f, "make build"), strings.Index(f, "gofmt"); i < 0 || j < 0 || i > j {
		t.Fatalf("order lost:\n%s", f)
	}
	ok("gate", "tool", "--rm", "fmt")
	if strings.Contains(file(), "gofmt") {
		t.Fatal("--rm left the step")
	}
	ok("gate", "tool", "--lock=")
	if strings.Contains(file(), "tool-build") {
		t.Fatal("--lock= did not clear the lock")
	}
	if out := ok("ls"); !strings.Contains(out, "GATE") || !strings.Contains(out, "build,test") {
		t.Fatalf("ls:\n%s", out)
	}

	for _, args := range [][]string{
		{"gate"},
		{"gate", "bad.name"},
		{"gate", "tool", "--add", "x"}, // no --run
		{"gate", "tool", "--add", "x", "--run", "y", "--rm", "test"}, // two actions
		{"gate", "tool", "--run", "y"},                               // no action
		{"gate", "tool", "--move", "test"},                           // no --to
		{"gate", "tool", "--move", "test", "--to", "0"},              // bad position
		{"gate", "tool", "--rm", "test", "--to", "1"},                // flag of another action
		{"gate", "tool", "--edit", "test", "--stop-on-fail", "--no-stop-on-fail"},
		{"gate", "tool", "--add", "x", "--run", "y", "--channel", "#c"}, // foreign flag
	} {
		if code, _, _ := c(args...); code != 2 {
			t.Errorf("%q: exit %d, want usage exit 2", args, code)
		}
	}
	before := file()
	for _, args := range [][]string{
		{"gate", "tool", "--add", "test", "--run", "dup"},
		{"gate", "tool", "--add", "x", "--run", "y", "--timeout", "forever"},
		{"gate", "tool", "--edit", "nope", "--run", "y"},
		{"gate", "tool", "--rm", "nope"},
		{"gate", "tool", "--lock", "a/b"},
	} {
		if code, _, _ := c(args...); code != 1 {
			t.Errorf("%q: exit %d, want 1", args, code)
		}
	}
	if file() != before {
		t.Fatal("a refused edit changed the file")
	}
	ok("gate", "tool", "--remove")
	if strings.Contains(file(), "tool.gate") {
		t.Fatalf("--remove left the gate:\n%s", file())
	}
	if out := ok("gate", "tool"); !strings.Contains(out, "agentcfg gate tool --add") {
		t.Fatalf("listing without a gate: %s", out)
	}
}
