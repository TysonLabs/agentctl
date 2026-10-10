package cfg

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/TysonLabs/agentctl/internal/gatespec"
	"github.com/TysonLabs/agentctl/internal/registry"
)

const gateSample = `[pay.prod]
base_url = "https://pay.example.com"
token = "plain_prod_token_1"

[pay.meta]
repo = "~/src/pay"

[pay.announce]
webhook = "https://hooks.example.com/x"
channels = ["a", "b"]

[pay.gate]
lock = "pay-build"

[[pay.gate.steps]]
name = "fmt"
run = "gofmt -l ."

[[pay.gate.steps]]
name = "test"
run = "go test ./..."
timeout = "60m"
stop_on_fail = true

[pay.future]
base_url = "https://future.example.com"
nested = { a = 1 }
list = [[1, 2], ["x"]]

[[pay.future.items]]
k = "v"
[pay.future.items.sub]
deep = true

[[pay.future.items]]
k = "w"
`

func decodeTree(t *testing.T, content string) map[string]any {
	t.Helper()
	tree := map[string]any{}
	if _, err := toml.Decode(content, &tree); err != nil {
		t.Fatal(err)
	}
	return tree
}

func gateOf(t *testing.T, s *Store, name string) gatespec.Spec {
	t.Helper()
	tree := decodeTree(t, read(t, s))
	svc, _ := tree[name].(map[string]any)
	sp, err := gatespec.FromTable(name, svc["gate"])
	if err != nil {
		t.Fatalf("gate of %s: %v\n%s", name, err, read(t, s))
	}
	return sp
}

func stepNames(sp gatespec.Spec) string {
	var n []string
	for _, s := range sp.Steps {
		n = append(n, s.Name)
	}
	return strings.Join(n, ",")
}

// Every edit rewrites the whole file: arrays of tables (the gate's steps and
// tables agentcfg does not know) must come back exactly.
func TestGateAndUnknownTablesRoundTrip(t *testing.T) {
	s := newStore(t, gateSample)
	before := decodeTree(t, gateSample)
	if _, err := s.SetBaseURL("", "dial.dev", "https://dial.example.com"); err != nil {
		t.Fatal(err)
	}
	after := decodeTree(t, read(t, s))
	delete(after, "dial")
	if !sameTree(before, after) {
		t.Fatalf("round trip changed the tree:\n%s", read(t, s))
	}
	if got := stepNames(gateOf(t, s, "pay")); got != "fmt,test" {
		t.Fatalf("steps %s", got)
	}
	// agentctl still reads the file, and sees no env called gate.
	reg, err := registry.Parse(s.Path, []byte(read(t, s)))
	if err != nil {
		t.Fatal(err)
	}
	for _, svc := range reg.Services {
		if svc.Env == "gate" || svc.Env == "meta" || svc.Env == "announce" {
			t.Errorf("unexpected service %s", svc.FullName())
		}
	}
}

func TestGateOnlyProjectIsValid(t *testing.T) {
	s := newStore(t, "")
	if _, err := s.SetMeta("", "tool", map[string]string{"repo": "~/src/tool"}); err != nil {
		t.Fatalf("SetMeta must create a project: %v", err)
	}
	if _, err := s.AddGateStep("", "tool", GateStep{Name: "test", Run: "make test"}, 0); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Parse(s.Path, []byte(read(t, s)))
	if err != nil {
		t.Fatalf("agentctl rejects a gate-only project: %v", err)
	}
	if len(reg.Services) != 0 {
		t.Fatalf("services %v", reg.Services)
	}
	st := s.State()
	if st.Error != "" || len(st.Gates) != 1 || st.Gates[0].Meta["repo"] != "~/src/tool" || st.Gates[0].Steps[0].Run != "make test" {
		t.Fatalf("state %+v", st)
	}
	// Clearing the last meta key of an otherwise empty project removes it.
	if _, err := s.RemoveGate("", "tool"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read(t, s), "tool") {
		t.Fatalf("project left behind:\n%s", read(t, s))
	}
}

func TestGateStepOps(t *testing.T) {
	s := newStore(t, gateSample)
	step := func(name string) GateStep { return GateStep{Name: name, Run: "echo " + name} }
	mustNames := func(want string) {
		t.Helper()
		if got := stepNames(gateOf(t, s, "pay")); got != want {
			t.Fatalf("steps %s, want %s", got, want)
		}
	}
	if _, err := s.AddGateStep("", "pay", step("lint"), 1); err != nil {
		t.Fatal(err)
	}
	mustNames("lint,fmt,test")
	if _, err := s.AddGateStep("", "pay", step("build"), 0); err != nil {
		t.Fatal(err)
	}
	mustNames("lint,fmt,test,build")
	if _, err := s.AddGateStep("", "pay", step("mid"), 3); err != nil {
		t.Fatal(err)
	}
	mustNames("lint,fmt,mid,test,build")
	if _, err := s.MoveGateStep("", "pay", "lint", 5); err != nil {
		t.Fatal(err)
	}
	mustNames("fmt,mid,test,build,lint")
	if _, err := s.MoveGateStep("", "pay", "build", 1); err != nil {
		t.Fatal(err)
	}
	mustNames("build,fmt,mid,test,lint")
	if _, err := s.RemoveGateStep("", "pay", "mid"); err != nil {
		t.Fatal(err)
	}
	mustNames("build,fmt,test,lint")
	if _, err := s.EditGateStep("", "pay", "fmt", GateStep{Name: "format", Run: "gofmt -l .", Timeout: "5m", StopOnFail: true}); err != nil {
		t.Fatal(err)
	}
	mustNames("build,format,test,lint")
	sp := gateOf(t, s, "pay")
	if f := sp.Steps[1]; f.TimeoutRaw != "5m" || !f.StopOnFail || f.Run != "gofmt -l ." {
		t.Fatalf("edited step %+v", f)
	}
	if sp.Lock != "pay-build" {
		t.Fatalf("lock %q lost", sp.Lock)
	}
	if _, err := s.SetGateLock("", "pay", "other.lock"); err != nil || gateOf(t, s, "pay").Lock != "other.lock" {
		t.Fatalf("set lock: %v", err)
	}
	if _, err := s.SetGateLock("", "pay", ""); err != nil || gateOf(t, s, "pay").Lock != "" {
		t.Fatalf("clear lock: %v", err)
	}

	before := read(t, s)
	for name, bad := range map[string]func() error{
		"dup add":      func() error { _, err := s.AddGateStep("", "pay", step("test"), 0); return err },
		"rename onto":  func() error { _, err := s.EditGateStep("", "pay", "lint", step("test")); return err },
		"add at 0-ish": func() error { _, err := s.AddGateStep("", "pay", step("x"), 9); return err },
		"move out":     func() error { _, err := s.MoveGateStep("", "pay", "lint", 5); return err },
		"move missing": func() error { _, err := s.MoveGateStep("", "pay", "nope", 1); return err },
		"rm missing":   func() error { _, err := s.RemoveGateStep("", "pay", "nope"); return err },
		"empty run":    func() error { _, err := s.AddGateStep("", "pay", GateStep{Name: "x", Run: " "}, 0); return err },
		"newline run":  func() error { _, err := s.AddGateStep("", "pay", GateStep{Name: "x", Run: "a\nb"}, 0); return err },
		"bad timeout": func() error {
			_, err := s.AddGateStep("", "pay", GateStep{Name: "x", Run: "a", Timeout: "-1s"}, 0)
			return err
		},
		"path step name":  func() error { _, err := s.AddGateStep("", "pay", GateStep{Name: "../x", Run: "a"}, 0); return err },
		"bad lock":        func() error { _, err := s.SetGateLock("", "pay", "a/b"); return err },
		"stale version":   func() error { _, err := s.AddGateStep("deadbeef", "pay", step("x"), 0); return err },
		"edit no gate":    func() error { _, err := s.EditGateStep("", "dial", "a", step("a")); return err },
		"remove no gate":  func() error { _, err := s.RemoveGate("", "nope"); return err },
		"bad project":     func() error { _, err := s.AddGateStep("", "bad.name", step("x"), 0); return err },
		"clear lock none": func() error { _, err := s.SetGateLock("", "newproj", ""); return err },
	} {
		if err := bad(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if read(t, s) != before {
			t.Fatalf("%s: file changed", name)
		}
	}
	if _, err := s.AddGateStep("deadbeef", "pay", step("x"), 0); err != ErrConflict {
		t.Errorf("stale version: %v, want ErrConflict", err)
	}

	// Removing every step and the lock removes the gate table.
	for _, n := range []string{"build", "format", "test", "lint"} {
		if _, err := s.RemoveGateStep("", "pay", n); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(read(t, s), "pay.gate") {
		t.Fatalf("empty gate left behind:\n%s", read(t, s))
	}
}

func TestGateEditRefusesAnInvalidExistingGate(t *testing.T) {
	content := "[pay.meta]\nrepo = \"x\"\n[[pay.gate.steps]]\nname = \"a\"\nrun = \"true\"\nstop_on_failure = true\n"
	s := newStore(t, content)
	if _, err := s.AddGateStep("", "pay", GateStep{Name: "b", Run: "true"}, 0); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("err %v", err)
	}
	if read(t, s) != content {
		t.Fatal("file changed")
	}
	st := s.State()
	if len(st.Gates) != 1 || !strings.Contains(st.Gates[0].Error, "unknown key") || st.Gates[0].Steps[0].Name != "a" {
		t.Fatalf("state %+v", st.Gates)
	}
}

func TestRemoveLastEnvKeepsMetaForGate(t *testing.T) {
	s := newStore(t, gateSample)
	if _, err := s.Remove("", "pay.prod"); err != nil {
		t.Fatal(err)
	}
	tree := decodeTree(t, read(t, s))
	svc := tree["pay"].(map[string]any)
	if meta, _ := svc["meta"].(map[string]any); meta["repo"] != "~/src/pay" {
		t.Fatalf("meta.repo dropped while the gate needs it:\n%s", read(t, s))
	}
}
