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
	if !strings.Contains(body, "let base = state.version;") {
		t.Fatal("the step dialog does not keep the version its step list came from")
	}
	for _, w := range []string{
		`write("/api/gate/step", { name, original: "", step: form.step, at }, base)`,
		`write("/api/gate/step", { name, original: st.name, step: form.step }, base)`,
		`write("/api/gate/move", { name, step: form.step.name, to }, moveVersion)`,
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

// A successful repo write advances the dialog's version. If the following
// step is rejected, correcting the step and submitting again must not reuse
// the version from before the repo write and conflict with its own change.
func TestNewGateProjectRetryUsesCommittedRepoVersion(t *testing.T) {
	js, err := fs.ReadFile(assets, "assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	body := gateStepDialog(t, string(js))
	meta := strings.Index(body, `const next = await write("/api/meta"`)
	advance := strings.Index(body, "base = next.version;")
	step := strings.Index(body, `write("/api/gate/step", { name: form.proj, original: "", step: form.step }, base)`)
	if meta < 0 || advance < meta || step < advance {
		t.Fatal("a repo-first gate create does not advance the retry version before writing the step")
	}
}

// A 409 reload invalidates the values and gate ordering shown by a dialog.
// It must close so retrying starts from a newly rendered list and version.
func TestStaleWriteClosesTheDialog(t *testing.T) {
	js, err := fs.ReadFile(assets, "assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	if !strings.Contains(src, "conflict.status = 409;") {
		t.Fatal("write hides the 409 status from the dialog submit handler")
	}
	submit := strings.Index(src, `$("dlg-form").addEventListener("submit"`)
	cancel := strings.Index(src, `$("dlg-cancel").addEventListener`)
	if submit < 0 || cancel < submit {
		t.Fatal("could not find dialog submit handler")
	}
	body := src[submit:cancel]
	catch := strings.Index(body, "} catch (e) {")
	if catch < 0 {
		t.Fatal("could not find dialog submit error path")
	}
	errorPath := body[catch:]
	stale := strings.Index(errorPath, "if (e.status === 409) {")
	if stale < 0 {
		t.Fatal("dialog submit handler does not distinguish a stale write")
	}
	closeDialog := strings.Index(errorPath[stale:], `$("dlg").close();`)
	showError := strings.Index(errorPath[stale:], `$("dlg-error").textContent = e.message;`)
	if closeDialog < 0 || showError < closeDialog {
		t.Fatal("a stale dialog remains open on the list and version that were reloaded")
	}
}

// Removing the only gate data can remove the whole project card. Focus must
// then fall back to the persistent Add gate project button, not the body.
func TestGateRemovalHasPersistentFocusFallback(t *testing.T) {
	js, err := fs.ReadFile(assets, "assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	html, err := fs.ReadFile(assets, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	if !strings.Contains(string(html), `id="add-gate" type="button" data-focus="add-gate"`) {
		t.Fatal("the surviving Add gate project button has no focus key")
	}
	checks := []struct {
		start string
		end   string
		want  string
	}{
		{"function renderGate(", "function renderGateStep(", `["gate-add:" + name, "gate-h:" + name, "add-gate"]`},
		{"function renderGateStep(", "function renderLock(", `"gate-add:" + name, "add-gate"],`},
		{"function renderLock(", "function armedButton(", `["gate-lock:" + name, "gate-add:" + name, "add-gate"]`},
		{"function openGateStep(", "function openMeta(", `if (!dialogReturn.includes("add-gate")) dialogReturn.push("add-gate");`},
	}
	for _, check := range checks {
		start := strings.Index(src, check.start)
		end := strings.Index(src, check.end)
		if start < 0 || end < start {
			t.Fatalf("could not find %s", check.start)
		}
		if !strings.Contains(src[start:end], check.want) {
			t.Fatalf("%s lacks the persistent focus fallback %q", check.start, check.want)
		}
	}
}

// An inline lock draft belongs to the current gate. If removing a last step
// or the gate itself makes that gate disappear, recreating it must not revive
// the old unsaved lock editor.
func TestMissingGateDiscardsInlineLockDraft(t *testing.T) {
	js, err := fs.ReadFile(assets, "assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	start := strings.Index(src, "function renderPage(")
	end := strings.Index(src, "function plural(")
	if start < 0 || end < start {
		t.Fatal("could not find renderPage")
	}
	body := src[start:end]
	if !strings.Contains(body, "for (const name of lockEdit.keys())") ||
		!strings.Contains(body, "if (!gateNames.has(name)) lockEdit.delete(name);") {
		t.Fatal("a removed gate leaves its unsaved inline lock draft behind")
	}
}

func gateStepDialog(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "function openGateStep(")
	end := strings.Index(src, "function openMeta(")
	if start < 0 || end < start {
		t.Fatal("could not find openGateStep")
	}
	return src[start:end]
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
	for _, want := range []string{`"gate gate-off"`, `text: "not set up"`, `"gate-add:" + name`, "agentflow gate runs them", `"+ Add step"`, "agentflow gate --check --rev HEAD"} {
		if !strings.Contains(body, want) {
			t.Fatalf("gate block lacks %q", want)
		}
	}
	// A gate is optional: no gate is one muted line, not a call-to-action block.
	if strings.Contains(body, "Add first step") || strings.Contains(body, "btn primary") {
		t.Fatal("a project without a gate must show one muted line, not a primary button")
	}
	if strings.Contains(src, `text: "Gate step"`) || !strings.Contains(src, "onclick: () => focusGate(g.name)") {
		t.Fatal(`the card action must be "Gate", going to the block`)
	}
}
