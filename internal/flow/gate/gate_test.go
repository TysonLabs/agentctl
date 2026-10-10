package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// gitT runs git in dir and fails the test on error.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), dir, nil, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

// newRepo makes a repo with one commit.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "config", "user.email", "t@example.com")
	gitT(t, dir, "config", "user.name", "t")
	gitT(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(dir, "a.txt"), "one\n")
	gitT(t, dir, "add", "a.txt")
	gitT(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type stepDef struct {
	name, run, timeout string
	stop               bool
}

// writeConfig writes a services.toml with one project p whose repo is repo.
func writeConfig(t *testing.T, repo, lock string, steps ...stepDef) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "[p.meta]\nrepo = %q\n[p.gate]\n", repo)
	if lock != "" {
		fmt.Fprintf(&b, "lock = %q\n", lock)
	}
	for _, s := range steps {
		fmt.Fprintf(&b, "[[p.gate.steps]]\nname = %q\nrun = %q\n", s.name, s.run)
		if s.timeout != "" {
			fmt.Fprintf(&b, "timeout = %q\n", s.timeout)
		}
		if s.stop {
			b.WriteString("stop_on_fail = true\n")
		}
	}
	// An env table with a token: the gate must never need it.
	b.WriteString("[p.prod]\nbase_url = \"https://p.example.com\"\ntoken = \"tok_never_read_123\"\n")
	path := filepath.Join(t.TempDir(), "services.toml")
	writeFile(t, path, b.String())
	return path
}

func opts(t *testing.T, cfg, dir string) Options {
	return Options{ConfigPath: cfg, Dir: dir, DirGiven: true, OutDir: t.TempDir(), KillGrace: 200 * time.Millisecond}
}

func statuses(res *Result) string {
	var s []string
	for _, st := range res.Steps {
		s = append(s, st.Name+"="+st.Status)
	}
	return strings.Join(s, " ")
}

func TestRunAllStepsInOrderAndLogs(t *testing.T) {
	repo := newRepo(t)
	trace := filepath.Join(t.TempDir(), "trace")
	cfg := writeConfig(t, repo, "",
		stepDef{name: "first", run: "echo first >> " + trace + "; echo to-stdout; echo to-stderr >&2; exit 3"},
		stepDef{name: "second", run: "echo second >> " + trace + "; pwd"},
	)
	res := Run(context.Background(), opts(t, cfg, repo))
	if res.Status != StatusFailed || res.OK {
		t.Fatalf("status %s ok %v (%s), want failed", res.Status, res.OK, res.Error)
	}
	if got := statuses(res); got != "first=failed second=passed" {
		t.Fatalf("steps %s: a failed step without stop_on_fail must not stop the run", got)
	}
	if got := readFile(t, trace); got != "first\nsecond\n" {
		t.Fatalf("order %q", got)
	}
	if *res.Steps[0].Exit != 3 {
		t.Errorf("exit %v, want 3", *res.Steps[0].Exit)
	}
	log := readFile(t, res.Steps[0].Log)
	if !strings.Contains(log, "to-stdout") || !strings.Contains(log, "to-stderr") {
		t.Errorf("log %q lacks stdout or stderr", log)
	}
	// Steps run from the work tree root.
	if got := strings.TrimSpace(readFile(t, res.Steps[1].Log)); got != res.Dir {
		t.Errorf("pwd %q, want %q", got, res.Dir)
	}
	if filepath.Dir(res.Steps[0].Log) != res.LogDir {
		t.Errorf("log %s not in %s", res.Steps[0].Log, res.LogDir)
	}
}

func TestStopOnFailSkipsTheRest(t *testing.T) {
	repo := newRepo(t)
	cfg := writeConfig(t, repo, "",
		stepDef{name: "a", run: "true"},
		stepDef{name: "b", run: "false", stop: true},
		stepDef{name: "c", run: "true"},
	)
	res := Run(context.Background(), opts(t, cfg, repo))
	if got := statuses(res); got != "a=passed b=failed c=skipped" {
		t.Fatalf("steps %s", got)
	}
	if res.Status != StatusFailed {
		t.Fatalf("status %s", res.Status)
	}
}

func TestTimeoutKillsTheProcessGroup(t *testing.T) {
	repo := newRepo(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	cfg := writeConfig(t, repo, "",
		stepDef{name: "slow", run: "sleep 30 & echo $! > " + pidFile + "; wait", timeout: "500ms"},
		stepDef{name: "after", run: "true"},
	)
	start := time.Now()
	res := Run(context.Background(), opts(t, cfg, repo))
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the timeout did not end the step")
	}
	if got := statuses(res); got != "slow=timeout after=passed" {
		t.Fatalf("steps %s", got)
	}
	if res.Status != StatusTimeout {
		t.Fatalf("status %s, want timeout", res.Status)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(readFile(t, pidFile)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("background child %d survived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStopStepPrefersCompletedProcess(t *testing.T) {
	waitCh := make(chan error, 1)
	waitCh <- nil
	status, exit, natural := stopStep(999999, time.Millisecond, waitCh)
	if !natural || status != StepPassed || exit == nil || *exit != 0 {
		t.Fatalf("natural=%v status=%s exit=%v, want a natural pass", natural, status, exit)
	}
}

func TestStdinIsDevNull(t *testing.T) {
	repo := newRepo(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString("LEAKED-STDIN\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close() })
	cfg := writeConfig(t, repo, "", stepDef{name: "read", run: "cat", timeout: "10s"})
	res := Run(context.Background(), opts(t, cfg, repo))
	if res.Status != StatusPassed {
		t.Fatalf("status %s (%s)", res.Status, res.Error)
	}
	if got := readFile(t, res.Steps[0].Log); got != "" {
		t.Fatalf("the step read the caller's stdin: %q", got)
	}
}

func TestInterruptKillsStepAndRecordsNothing(t *testing.T) {
	repo := newRepo(t)
	started := filepath.Join(t.TempDir(), "started")
	cfg := writeConfig(t, repo, "", stepDef{name: "slow", run: "touch " + started + "; sleep 30"}, stepDef{name: "next", run: "true"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Interrupt once the step is really running.
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(started); err == nil {
				cancel()
				return
			}
		}
	}()
	begin := time.Now()
	res := Run(ctx, opts(t, cfg, repo))
	if time.Since(begin) > 25*time.Second {
		t.Fatal("the interrupt did not end the step")
	}
	if res.Status != StatusInterrupted || statuses(res) != "slow=interrupted next=skipped" {
		t.Fatalf("status %s steps %s", res.Status, statuses(res))
	}
	rf, err := readReceipts(res.Receipts)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rf.lookup("p", res.Tree, "slow", runHash("touch "+started+"; sleep 30")); ok {
		t.Fatal("an interrupted step left a receipt")
	}
}

// counter returns a step command that counts its runs, and a reader.
func counter(t *testing.T) (string, func() int) {
	f := filepath.Join(t.TempDir(), "count")
	return "echo x >> " + f, func() int {
		b, err := os.ReadFile(f)
		if os.IsNotExist(err) {
			return 0
		}
		return strings.Count(string(b), "x")
	}
}

func TestReceiptsCacheByTreeAndRunString(t *testing.T) {
	repo := newRepo(t)
	run, runs := counter(t)
	cfg := writeConfig(t, repo, "", stepDef{name: "s", run: run})
	o := opts(t, cfg, repo)
	gate := func(want string, wantRuns int) *Result {
		t.Helper()
		res := Run(context.Background(), o)
		if res.Status != StatusPassed || res.Steps[0].Status != want || runs() != wantRuns {
			t.Fatalf("status %s step %s runs %d, want %s with %d runs (%s)", res.Status, res.Steps[0].Status, runs(), want, wantRuns, res.Error)
		}
		return res
	}
	first := gate(StepPassed, 1)
	if first.Dirty || first.Tree != gitT(t, repo, "rev-parse", "HEAD^{tree}") {
		t.Fatalf("clean tree %s dirty %v, want HEAD^{tree}", first.Tree, first.Dirty)
	}
	gate(StepCached, 1)

	// An uncommitted edit to a tracked file is a new tree.
	writeFile(t, filepath.Join(repo, "a.txt"), "two\n")
	edited := gate(StepPassed, 2)
	if !edited.Dirty || edited.Tree == first.Tree {
		t.Fatalf("edit not seen: dirty %v tree %s", edited.Dirty, edited.Tree)
	}
	gate(StepCached, 2)

	// So is a new untracked file, and an edit to it.
	writeFile(t, filepath.Join(repo, "new.txt"), "n\n")
	gate(StepPassed, 3)
	writeFile(t, filepath.Join(repo, "new.txt"), "m\n")
	untracked := gate(StepPassed, 4)

	// Committing exactly what was gated keeps the tree: still cached.
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "gated")
	committed := gate(StepCached, 4)
	if committed.Dirty || committed.Tree != untracked.Tree {
		t.Fatalf("commit of the gated tree changed its key: %s vs %s", committed.Tree, untracked.Tree)
	}

	// A commit that changes content reruns.
	writeFile(t, filepath.Join(repo, "a.txt"), "three\n")
	gitT(t, repo, "commit", "-q", "-am", "change")
	gate(StepPassed, 5)

	// Ignored files do not count.
	writeFile(t, filepath.Join(repo, ".gitignore"), "build/\n")
	gitT(t, repo, "add", ".gitignore")
	gitT(t, repo, "commit", "-q", "-m", "ignore")
	gate(StepPassed, 6)
	if err := os.MkdirAll(filepath.Join(repo, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "build", "out"), "x")
	gate(StepCached, 6)

	// --force reruns.
	o.Force = true
	gate(StepPassed, 7)
	o.Force = false

	// A changed run string invalidates the receipt.
	writeConfig2 := writeConfig(t, repo, "", stepDef{name: "s", run: run + " # v2"})
	o.ConfigPath = writeConfig2
	gate(StepPassed, 8)
	gate(StepCached, 8)

	// The run hash is part of the receipt key, so reverting the command can
	// reuse its earlier passing receipt for this exact tree.
	o.ConfigPath = cfg
	gate(StepCached, 8)
}

func TestFailedStepIsNotCached(t *testing.T) {
	repo := newRepo(t)
	run, runs := counter(t)
	cfg := writeConfig(t, repo, "", stepDef{name: "s", run: run + "; false"})
	for i := 1; i <= 2; i++ {
		res := Run(context.Background(), opts(t, cfg, repo))
		if res.Steps[0].Status != StepFailed || runs() != i {
			t.Fatalf("run %d: step %s runs %d", i, res.Steps[0].Status, runs())
		}
	}
}

func TestOnlyRunsSelectedSteps(t *testing.T) {
	repo := newRepo(t)
	cfg := writeConfig(t, repo, "", stepDef{name: "a", run: "true"}, stepDef{name: "b", run: "true"}, stepDef{name: "c", run: "true"})
	o := opts(t, cfg, repo)
	o.Only = []string{"c", "a"}
	res := Run(context.Background(), o)
	if got := statuses(res); got != "a=passed c=passed" {
		t.Fatalf("steps %s (configured order, selected only)", got)
	}
	o.Only = []string{"zzz"}
	if res := Run(context.Background(), o); res.Status != StatusError || !strings.Contains(res.Error, "zzz") {
		t.Fatalf("unknown --only: %s %s", res.Status, res.Error)
	}
}

func TestCheck(t *testing.T) {
	repo := newRepo(t)
	cfg := writeConfig(t, repo, "", stepDef{name: "a", run: "true"}, stepDef{name: "b", run: "true"})
	o := opts(t, cfg, repo)
	check := func(tree, rev string) *CheckResult {
		t.Helper()
		return Check(context.Background(), o, tree, rev)
	}
	if res := check("", ""); res.OK || res.Status != StatusMissing || strings.Join(res.Missing, ",") != "a,b" {
		t.Fatalf("before any run: %+v", res)
	}
	o2 := o
	o2.Only = []string{"a"}
	Run(context.Background(), o2)
	if res := check("", ""); res.OK || strings.Join(res.Missing, ",") != "b" {
		t.Fatalf("after a only: %+v", res)
	}
	run := Run(context.Background(), o)
	if res := check("", ""); !res.OK || res.Status != StatusPassed || len(res.Missing) != 0 {
		t.Fatalf("after a full run: %+v", res)
	}
	if res := check(run.Tree, ""); !res.OK {
		t.Fatalf("--tree: %+v", res)
	}
	if res := check("", "HEAD"); !res.OK {
		t.Fatalf("--rev HEAD (clean): %+v", res)
	}
	// An uncommitted edit: the work tree is ungated, HEAD still is.
	writeFile(t, filepath.Join(repo, "a.txt"), "dirty\n")
	if res := check("", ""); res.OK {
		t.Fatalf("dirty work tree passed --check: %+v", res)
	}
	if res := check("", "HEAD"); !res.OK {
		t.Fatalf("--rev HEAD after an uncommitted edit: %+v", res)
	}
	// A changed run string makes the receipt stale.
	o.ConfigPath = writeConfig(t, repo, "", stepDef{name: "a", run: "true # changed"}, stepDef{name: "b", run: "true"})
	res := check("", "HEAD")
	if res.OK || res.Steps[0].Status != "stale" {
		t.Fatalf("changed run string: %+v", res)
	}
	// A failing receipt is not a pass.
	o.ConfigPath = writeConfig(t, repo, "", stepDef{name: "a", run: "false"})
	Run(context.Background(), o)
	if res := check("", ""); res.OK || res.Steps[0].Status != "failed" {
		t.Fatalf("failed receipt: %+v", res)
	}
	if res := check("abc", ""); res.Status != StatusError {
		t.Fatalf("short --tree accepted: %+v", res)
	}
	if res := check("", "no-such-rev"); res.Status != StatusError {
		t.Fatalf("bad --rev accepted: %+v", res)
	}
}

func TestCheckWorkTreeDoesNotWriteGitObjects(t *testing.T) {
	repo := newRepo(t)
	cfg := writeConfig(t, repo, "", stepDef{name: "a", run: "true"})
	writeFile(t, filepath.Join(repo, "untracked.txt"), "content unique to the work tree\n")
	before := gitT(t, repo, "count-objects", "-v")

	res := Check(context.Background(), opts(t, cfg, repo), "", "")
	if res.Status != StatusMissing {
		t.Fatalf("check status %s (%s), want missing", res.Status, res.Error)
	}
	if after := gitT(t, repo, "count-objects", "-v"); after != before {
		t.Fatalf("--check wrote into the repository object database:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestCheckRefusesCorruptReceipts(t *testing.T) {
	repo := newRepo(t)
	cfg := writeConfig(t, repo, "", stepDef{name: "a", run: "true"})
	o := opts(t, cfg, repo)
	res := Run(context.Background(), o)
	writeFile(t, res.Receipts, "{not json")
	if c := Check(context.Background(), o, "", ""); c.Status != StatusError || c.OK {
		t.Fatalf("corrupt receipts: %+v", c)
	}
	// A run treats it as empty, reruns, and replaces it.
	res = Run(context.Background(), o)
	if res.Steps[0].Status != StepPassed || len(res.Warnings) == 0 {
		t.Fatalf("run over corrupt receipts: %s %v", statuses(res), res.Warnings)
	}
	if c := Check(context.Background(), o, "", ""); !c.OK {
		t.Fatalf("after the rerun: %+v", c)
	}
	// Syntactically valid but contradictory receipt data must also fail closed.
	b, err := os.ReadFile(res.Receipts)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	projects := doc["projects"].(map[string]any)
	trees := projects["p"].(map[string]any)
	steps := trees[res.Tree].(map[string]any)
	runs := steps["a"].(map[string]any)
	receipt := runs[runHash("true")].(map[string]any)
	receipt["status"] = StepFailed // leave ok=true: impossible for a receipt we wrote
	b, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, res.Receipts, string(b))
	if c := Check(context.Background(), o, "", ""); c.Status != StatusError || c.OK {
		t.Fatalf("contradictory receipt passed check: %+v", c)
	}
}

func TestLockSerializesConcurrentRuns(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	repo := newRepo(t)
	trace := filepath.Join(t.TempDir(), "trace")
	cfg := writeConfig(t, repo, "test-lock",
		stepDef{name: "s", run: "echo start >> " + trace + "; sleep 0.4; echo end >> " + trace})
	var wg sync.WaitGroup
	results := make([]*Result, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := opts(t, cfg, repo)
			o.Force = true // both must really run
			results[i] = Run(context.Background(), o)
		}()
	}
	wg.Wait()
	for _, r := range results {
		if r.Status != StatusPassed {
			t.Fatalf("status %s (%s)", r.Status, r.Error)
		}
	}
	if got := readFile(t, trace); got != "start\nend\nstart\nend\n" {
		t.Fatalf("runs overlapped: %q", got)
	}
	waited := results[0].Lock.WaitedSecs + results[1].Lock.WaitedSecs
	if waited < 0.2 {
		t.Errorf("waited %.1fs in total; one run should have waited for the other", waited)
	}
	lockPath, holderPath := LockPaths("test-lock")
	if results[0].Lock.Path != lockPath {
		t.Errorf("lock path %s, want %s", results[0].Lock.Path, lockPath)
	}
	if _, err := os.Stat(holderPath); !os.IsNotExist(err) {
		t.Errorf("holder file left behind: %v", err)
	}
}

func TestLockReportsHolder(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	release, _, err := AcquireLock(context.Background(), "held", LockHolder{PID: 4242, Cmd: "other", Dir: "/x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, holderPath := LockPaths("held")
	if h := readHolder(holderPath); h == nil || h.PID != 4242 || h.Since == "" {
		t.Fatalf("holder file %+v", h)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	var seen *LockHolder
	_, _, err = AcquireLock(ctx, "held", LockHolder{PID: 1}, func(h *LockHolder) { seen = h })
	if err == nil {
		t.Fatal("acquired a held lock")
	}
	if seen == nil || seen.PID != 4242 || seen.Cmd != "other" {
		t.Fatalf("waiter saw holder %+v", seen)
	}
	release()
	r2, _, err := AcquireLock(context.Background(), "held", LockHolder{PID: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r2()
}

func TestLockRequiresHolderFile(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	_, holderPath := LockPaths("blocked-holder")
	if err := os.Mkdir(holderPath, 0o700); err != nil {
		t.Fatal(err)
	}
	release, _, err := AcquireLock(context.Background(), "blocked-holder", LockHolder{PID: 1}, nil)
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("lock succeeded without publishing its holder JSON")
	}
}

func TestResolveProject(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	cfg := writeConfig(t, repo, "", stepDef{name: "a", run: "true"})

	// The repo itself, from a subdirectory.
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	tg, err := resolve(ctx, cfg, "", sub, true)
	if err != nil || tg.project != "p" {
		t.Fatalf("subdir: %v %+v", err, tg)
	}
	wantCommon := tg.common

	// A linked worktree of the same repo.
	wt := filepath.Join(t.TempDir(), "wt")
	gitT(t, repo, "worktree", "add", "-q", wt, "-b", "side")
	tg, err = resolve(ctx, cfg, "", wt, true)
	if err != nil || tg.project != "p" {
		t.Fatalf("worktree: %v", err)
	}
	if real, _ := filepath.EvalSymlinks(wt); tg.root != real || tg.common != wantCommon {
		t.Fatalf("worktree root %s common %s", tg.root, tg.common)
	}

	// meta.repo through a symlink.
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	cfgLink := writeConfig(t, link, "", stepDef{name: "a", run: "true"})
	if tg, err := resolve(ctx, cfgLink, "", repo, true); err != nil || tg.project != "p" {
		t.Fatalf("symlinked meta.repo: %v", err)
	}
	// And meta.repo with ~.
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.Symlink(repo, filepath.Join(home, "proj")); err != nil {
		t.Fatal(err)
	}
	cfgHome := writeConfig(t, "~/proj", "", stepDef{name: "a", run: "true"})
	if tg, err := resolve(ctx, cfgHome, "", wt, true); err != nil || tg.project != "p" {
		t.Fatalf("~ meta.repo from a worktree: %v", err)
	}

	// A configured git common dir identifies every checkout of that repo too.
	cfgCommon := writeConfig(t, wantCommon, "", stepDef{name: "a", run: "true"})
	if tg, err := resolve(ctx, cfgCommon, "", wt, true); err != nil || tg.project != "p" {
		t.Fatalf("git common-dir meta.repo from a worktree: %v", err)
	}

	// Another repo matches nothing.
	other := newRepo(t)
	if _, err := resolve(ctx, cfg, "", other, true); err == nil || !strings.Contains(err.Error(), "agentcfg gate") {
		t.Fatalf("other repo: %v", err)
	}
	// --project with a checkout of another repo is refused.
	if _, err := resolve(ctx, cfg, "p", other, true); err == nil || !strings.Contains(err.Error(), "not a checkout") {
		t.Fatalf("--project in the wrong repo: %v", err)
	}
	// --project from outside any checkout runs in its repo.
	if tg, err := resolve(ctx, cfg, "p", t.TempDir(), false); err != nil || tg.root != wantRootOf(t, repo) {
		t.Fatalf("--project outside a checkout: %v %+v", err, tg)
	}
	if _, err := resolve(ctx, cfg, "nope", repo, true); err == nil || !strings.Contains(err.Error(), "agentcfg gate nope") {
		t.Fatalf("unknown project: %v", err)
	}
}

func wantRootOf(t *testing.T, dir string) string {
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestResolveRefusesBadConfig(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "services.toml")
		writeFile(t, p, fmt.Sprintf("[p.meta]\nrepo = %q\n", repo)+body)
		return p
	}
	cases := map[string]struct{ body, want string }{
		"no gate":       {"", "no [p.gate] table"},
		"no steps":      {"[p.gate]\nlock = \"x\"\n", "no steps"},
		"unknown key":   {"[p.gate]\n[[p.gate.steps]]\nname = \"a\"\nrun = \"true\"\nstop_on_failure = true\n", "unknown key"},
		"dup name":      {"[[p.gate.steps]]\nname = \"a\"\nrun = \"true\"\n[[p.gate.steps]]\nname = \"a\"\nrun = \"true\"\n", "used twice"},
		"empty run":     {"[[p.gate.steps]]\nname = \"a\"\nrun = \"  \"\n", "run is empty"},
		"bad timeout":   {"[[p.gate.steps]]\nname = \"a\"\nrun = \"true\"\ntimeout = \"soon\"\n", "timeout"},
		"bad name":      {"[[p.gate.steps]]\nname = \"../x\"\nrun = \"true\"\n", "step name"},
		"bad lock":      {"[p.gate]\nlock = \"a/b\"\n[[p.gate.steps]]\nname = \"a\"\nrun = \"true\"\n", "lock name"},
		"bad stop type": {"[[p.gate.steps]]\nname = \"a\"\nrun = \"true\"\nstop_on_fail = \"yes\"\n", "true or false"},
	}
	for name, c := range cases {
		if _, err := resolve(ctx, write(c.body), "", repo, true); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", name, err, c.want)
		}
	}
	// Two projects with gates on one repo are ambiguous.
	two := write("[[p.gate.steps]]\nname = \"a\"\nrun = \"true\"\n" + fmt.Sprintf("[q.meta]\nrepo = %q\n[[q.gate.steps]]\nname = \"a\"\nrun = \"true\"\n", repo))
	if _, err := resolve(ctx, two, "", repo, true); err == nil || !strings.Contains(err.Error(), "--project") {
		t.Errorf("ambiguous: %v", err)
	}
	if tg, err := resolve(ctx, two, "q", repo, true); err != nil || tg.project != "q" {
		t.Errorf("--project q: %v", err)
	}
	// A second project on the repo without a gate cannot run, so it does not
	// make the one gated project ambiguous.
	oneGate := write("[[p.gate.steps]]\nname = \"a\"\nrun = \"true\"\n" + fmt.Sprintf("[q.meta]\nrepo = %q\n", repo))
	if tg, err := resolve(ctx, oneGate, "", repo, true); err != nil || tg.project != "p" {
		t.Errorf("one gated project among two: %v", err)
	}

	// --project selects a config entry, not permission to run its commands in
	// an arbitrary checkout: meta.repo must still establish the identity.
	for name, meta := range map[string]string{
		"missing":      "",
		"not a string": "[p.meta]\nrepo = 42\n",
	} {
		path := filepath.Join(t.TempDir(), "services.toml")
		writeFile(t, path, meta+"[[p.gate.steps]]\nname = \"a\"\nrun = \"true\"\n")
		if _, err := resolve(ctx, path, "p", repo, true); err == nil || !strings.Contains(err.Error(), "meta.repo") {
			t.Errorf("%s meta.repo: %v", name, err)
		}
	}
}

func TestCanceledContextIsInterrupted(t *testing.T) {
	repo := newRepo(t)
	cfg := writeConfig(t, repo, "", stepDef{name: "a", run: "true"})
	o := opts(t, cfg, repo)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res := Run(ctx, o); res.Status != StatusInterrupted {
		t.Fatalf("run status %s (%s), want interrupted", res.Status, res.Error)
	}
	if res := Check(ctx, o, "", ""); res.Status != StatusInterrupted {
		t.Fatalf("check status %s (%s), want interrupted", res.Status, res.Error)
	}
}

func TestTreeHashLeavesIndexAlone(t *testing.T) {
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, "a.txt"), "staged\n")
	gitT(t, repo, "add", "a.txt")
	writeFile(t, filepath.Join(repo, "u.txt"), "untracked\n")
	before := gitT(t, repo, "status", "--porcelain")
	if _, err := workTree(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if after := gitT(t, repo, "status", "--porcelain"); after != before {
		t.Fatalf("hashing changed the index: %q -> %q", before, after)
	}
}
