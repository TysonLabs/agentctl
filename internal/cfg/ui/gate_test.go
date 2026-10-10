package ui

import (
	"encoding/json"
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
