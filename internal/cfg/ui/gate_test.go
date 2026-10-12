package ui

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
)

type gateState struct {
	Version string `json:"version"`
	Gates   []struct {
		Name  string `json:"name"`
		Lock  string `json:"lock"`
		Steps []struct {
			Name       string `json:"name"`
			Run        string `json:"run"`
			Timeout    string `json:"timeout"`
			StopOnFail bool   `json:"stop_on_fail"`
		} `json:"steps"`
	} `json:"gates"`
}

func TestGateAPI(t *testing.T) {
	s, ts := newServer(t)
	var st gateState
	post := func(path, body string, want int) {
		t.Helper()
		body = strings.Replace(body, "{", `{"version":"`+st.Version+`",`, 1)
		code, resp := do(t, s, ts, call{path: path, body: body})
		if code != want {
			t.Fatalf("%s %s: %d, want %d: %s", path, body, code, want, resp)
		}
		if code == 200 {
			st = gateState{}
			if err := json.Unmarshal([]byte(resp), &st); err != nil {
				t.Fatal(err)
			}
		}
	}
	names := func() string {
		var n []string
		for _, g := range st.Gates {
			for _, s := range g.Steps {
				n = append(n, s.Name)
			}
		}
		return strings.Join(n, ",")
	}
	_, body := do(t, s, ts, call{method: "GET", path: "/api/state"})
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}

	post("/api/gate/step", `{"name":"pay","step":{"name":"test","run":"go test ./...","timeout":"","stop_on_fail":false}}`, 200)
	post("/api/gate/step", `{"name":"pay","step":{"name":"fmt","run":"gofmt -l .","timeout":"5m","stop_on_fail":true},"at":1}`, 200)
	if names() != "fmt,test" || !st.Gates[0].Steps[0].StopOnFail || st.Gates[0].Steps[0].Timeout != "5m" {
		t.Fatalf("after adds: %+v", st.Gates)
	}
	post("/api/gate/step", `{"name":"pay","original":"fmt","step":{"name":"format","run":"gofmt -l .","timeout":"","stop_on_fail":false}}`, 200)
	if names() != "format,test" || st.Gates[0].Steps[0].StopOnFail {
		t.Fatalf("after edit: %+v", st.Gates)
	}
	post("/api/gate/move", `{"name":"pay","step":"test","to":1}`, 200)
	if names() != "test,format" {
		t.Fatalf("after move: %s", names())
	}
	post("/api/gate/lock", `{"name":"pay","lock":"pay-build"}`, 200)
	if st.Gates[0].Lock != "pay-build" {
		t.Fatalf("lock %q", st.Gates[0].Lock)
	}
	post("/api/gate/step/remove", `{"name":"pay","step":"format"}`, 200)
	if names() != "test" {
		t.Fatalf("after delete: %s", names())
	}

	// Refusals.
	post("/api/gate/step", `{"name":"pay","step":{"name":"test","run":"x","timeout":"","stop_on_fail":false}}`, 400)
	post("/api/gate/step", `{"name":"pay","step":{"name":"y","run":"","timeout":"","stop_on_fail":false}}`, 400)
	post("/api/gate/step", `{"name":"pay","original":"test","at":2,"step":{"name":"test","run":"x","timeout":"","stop_on_fail":false}}`, 400)
	post("/api/gate/step", `{"name":"pay","step":{"name":"y","run":"x","timeout":"","stop_on_fail":false},"admin":true}`, 400)
	post("/api/gate/move", `{"name":"pay","step":"test","to":7}`, 400)
	post("/api/gate/lock", `{"name":"pay","lock":"../x"}`, 400)

	// A stale version is a conflict and changes nothing.
	for _, c := range []struct{ path, body string }{
		{"/api/gate/step", `{"version":"stale","name":"pay","step":{"name":"z","run":"x","timeout":"","stop_on_fail":false}}`},
		{"/api/gate/move", `{"version":"stale","name":"pay","step":"test","to":1}`},
		{"/api/gate/step/remove", `{"version":"stale","name":"pay","step":"test"}`},
		{"/api/gate/lock", `{"version":"stale","name":"pay","lock":"x"}`},
		{"/api/gate/remove", `{"version":"stale","name":"pay"}`},
	} {
		if code, _ := do(t, s, ts, call{path: c.path, body: c.body}); code != 409 {
			t.Errorf("%s stale: %d, want 409", c.path, code)
		}
	}
	// Every gate route needs the key, our Origin and JSON.
	for _, path := range []string{"/api/gate/step", "/api/gate/move", "/api/gate/step/remove", "/api/gate/lock", "/api/gate/remove"} {
		if code, _ := do(t, s, ts, call{path: path, body: `{}`, key: "-"}); code != 401 {
			t.Errorf("%s without key: %d", path, code)
		}
		if code, _ := do(t, s, ts, call{path: path, body: `{}`, origin: "https://evil.example.com"}); code != 403 {
			t.Errorf("%s cross-origin: %d", path, code)
		}
		if code, _ := do(t, s, ts, call{path: path, body: `a=b`, ctype: "application/x-www-form-urlencoded"}); code != 415 {
			t.Errorf("%s form post: %d", path, code)
		}
	}

	post("/api/gate/remove", `{"name":"pay"}`, 200)
	if len(st.Gates) != 0 {
		t.Fatalf("gate not removed: %+v", st.Gates)
	}
}

func TestNewGateProjectWritesRepoFirst(t *testing.T) {
	js, err := fs.ReadFile(assets, "assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	start := strings.Index(src, "function openGateStep(")
	end := strings.Index(src, "function openMeta(")
	if start < 0 || end < start {
		t.Fatal("could not find openGateStep")
	}
	body := src[start:end]
	meta, step := strings.Index(body, `write("/api/meta"`), strings.Index(body, `write("/api/gate/step"`)
	if meta < 0 || step < 0 || meta > step {
		t.Fatal("a new gate project must save its repo before its first step")
	}
}

// The step dialog places a step relative to the list it showed. Every write
// it makes must be checked against that list's version, and nothing after
// the first write may read the live form (a re-render can change it).
func TestGateStepDialogWritesAgainstItsSnapshot(t *testing.T) {
	js, err := fs.ReadFile(assets, "assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	start := strings.Index(src, "function openGateStep(")
	end := strings.Index(src, "function openMeta(")
	if start < 0 || end < start {
		t.Fatal("could not find openGateStep")
	}
	body := src[start:end]
	if !strings.Contains(body, "const base = state.version;") {
		t.Fatal("the step dialog does not keep the version its step list came from")
	}
	for _, w := range []string{
		`write("/api/gate/step", { name, original: "", step: form.step, at }, base)`,
		`write("/api/gate/step", { name, original: st.name, step: form.step }, base)`,
		`write("/api/gate/move", { name, step: form.step.name, to }, changed ? undefined : base)`,
	} {
		if !strings.Contains(body, w) {
			t.Fatalf("step dialog write is not pinned to the dialog's version: %s", w)
		}
	}
	submit := strings.Index(body, "async () => {")
	first := strings.Index(body[submit:], "await write(")
	if submit < 0 || first < 0 {
		t.Fatal("could not find the submit handler")
	}
	after := body[submit+first:]
	for _, live := range []string{`val("f-`, `$("f-`, "sel.value"} {
		if i := strings.Index(after, live); i >= 0 && i < strings.Index(after, "if (preview) {") {
			t.Fatalf("step dialog reads live form state after its first write: %s", live)
		}
	}
}

// Every project card shows its gate block, with an empty state that says
// what a gate is; the card action goes to the block instead of a dialog.
func TestEveryCardShowsItsGateBlock(t *testing.T) {
	js, err := fs.ReadFile(assets, "assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	start := strings.Index(src, "function renderGate(")
	end := strings.Index(src, "function renderGateStep(")
	if start < 0 || end < start {
		t.Fatal("could not find renderGate")
	}
	body := src[start:end]
	if strings.Contains(body, "if (!gt) return null") {
		t.Fatal("a project without a gate shows no gate block")
	}
	for _, want := range []string{"Add first step", "agentflow gate runs them", `"+ Add step"`, "agentflow gate --check --rev HEAD"} {
		if !strings.Contains(body, want) {
			t.Fatalf("gate block lacks %q", want)
		}
	}
	if strings.Contains(src, `text: "Gate step"`) || !strings.Contains(src, "onclick: () => focusGate(g.name)") {
		t.Fatal(`the card action must be "Gate", going to the block`)
	}
}
