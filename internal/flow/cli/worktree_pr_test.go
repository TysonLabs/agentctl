package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeGH writes a gh stand-in that applies the caller's --jq to the
// JSON body (as gh does) and returns its path.
func writeFakeGH(t *testing.T, dir, body string) string {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed: the fake gh needs it to apply --jq")
	}
	data := filepath.Join(dir, "gh.json")
	if err := os.WriteFile(data, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gh := filepath.Join(dir, "fake-gh")
	script := "#!/bin/sh\nexpr=''\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --jq ]; then shift; expr=\"$1\"; fi; shift; done\nexec jq -r \"$expr\" \"" + data + "\"\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return gh
}

func clearOwnerEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"AGENTFLOW_OWNER", "CLAUDE_JOB_DIR", "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID"} {
		t.Setenv(k, "")
	}
}

type prFake struct {
	Number      int     `json:"number"`
	State       string  `json:"state"`
	BaseRefName string  `json:"baseRefName"`
	HeadRefName string  `json:"headRefName"`
	HeadRefOid  string  `json:"headRefOid"`
	HeadRepo    *string `json:"headRepository"`
}

// cliRepo is an origin with main and development, and a clone under test.
func cliRepo(t *testing.T) (root, seed, repo string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(root, "origin.git")
	seed = filepath.Join(root, "seed")
	repo = filepath.Join(root, "repo")
	cliGit(t, root, "init", "-q", "--bare", "-b", "main", origin)
	cliGit(t, root, "clone", "-q", origin, seed)
	cliGit(t, seed, "commit", "--allow-empty", "-qm", "base")
	cliGit(t, seed, "push", "-q", "origin", "main", "main:development")
	cliGit(t, root, "clone", "-q", origin, repo)
	lsof := filepath.Join(root, "lsof")
	if err := os.WriteFile(lsof, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_LSOF", lsof)
	clearOwnerEnv(t)
	return root, seed, repo
}

// branchWorktree makes a sibling worktree (plain git worktree add, as the
// vector-payments agents did) with one pushed commit; it returns path, tip.
func branchWorktree(t *testing.T, root, repo, branch string) (string, string) {
	t.Helper()
	path := filepath.Join(root, "repo-"+strings.ReplaceAll(branch, "/", "-"))
	cliGit(t, repo, "worktree", "add", "-q", "-b", branch, path, "origin/development")
	f := strings.ReplaceAll(branch, "/", "-") + ".txt"
	if err := os.WriteFile(filepath.Join(path, f), []byte(branch+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, path, "add", f)
	cliGit(t, path, "commit", "-qm", branch)
	cliGit(t, path, "push", "-q", "origin", branch)
	return path, cliGit(t, path, "rev-parse", "HEAD")
}

func squashIntoDevelopment(t *testing.T, seed, branch string) {
	t.Helper()
	cliGit(t, seed, "fetch", "-q", "origin")
	cliGit(t, seed, "switch", "-q", "-C", "dev", "origin/development")
	cliGit(t, seed, "merge", "-q", "--squash", "origin/"+branch)
	cliGit(t, seed, "commit", "-qm", "squash "+branch)
	cliGit(t, seed, "push", "-q", "origin", "dev:development")
}

func setFakePRs(t *testing.T, root string, prs ...prFake) {
	t.Helper()
	b, _ := json.Marshal(prs)
	t.Setenv("AGENTFLOW_GH", writeFakeGH(t, root, string(b)))
}

// The 2026-10-11 failure: done refused a branch whose PR was merged into
// development (not origin/HEAD) unless --into origin/development was given.
func TestDoneRemovesBranchMergedIntoNonDefaultBaseWithoutInto(t *testing.T) {
	root, seed, repo := cliRepo(t)
	path, tip := branchWorktree(t, root, repo, "tgeorge/bench-hosted-modal")
	squashIntoDevelopment(t, seed, "tgeorge/bench-hosted-modal")
	setFakePRs(t, root, prFake{113, "MERGED", "development", "tgeorge/bench-hosted-modal", tip, nil})
	if code, _, _ := run(t, "worktree", "adopt", path, "--owner", "job-7", "--repo", repo); code != 0 {
		t.Fatalf("adopt exit %d", code)
	}
	marker := filepath.Join(root, "cleanup.out")
	hook := filepath.Join(root, "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"$2\" >> "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cliGit(t, repo, "config", "--add", "agentflow.worktreeCleanup", hook+" paytiva_{slug_snake}_test {dir}")
	cliGit(t, repo, "config", "--add", "agentflow.worktreeCleanup", "false")

	code, out, errOut := run(t, "worktree", "done", "tgeorge/bench-hosted-modal", "--repo", repo)
	if code != 0 {
		t.Fatalf("done: exit %d out %s err %s", code, out, errOut)
	}
	var got struct {
		MergedVia string `json:"merged_via"`
		PRNumber  int    `json:"pr_number"`
		PRBase    string `json:"pr_base"`
		Owner     string `json:"owner"`
		Into      string `json:"into"`
		Removed   *struct {
			BranchDeleted bool `json:"branch_deleted"`
			RemoteDeleted bool `json:"remote_deleted"`
		} `json:"removed"`
		Cleanup []struct {
			Command  []string `json:"command"`
			OK       bool     `json:"ok"`
			ExitCode int      `json:"exit_code"`
		} `json:"cleanup"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.MergedVia != "PR #113 into development" || got.PRNumber != 113 || got.PRBase != "development" || got.Owner != "job-7" || got.Into != "" {
		t.Fatalf("result: %s", out)
	}
	if got.Removed == nil || !got.Removed.BranchDeleted || !got.Removed.RemoteDeleted {
		t.Fatalf("not cleaned up: %s", out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	// The hook ran without a shell, with placeholders filled; the failing
	// second command is reported but does not change the exit code.
	if b, _ := os.ReadFile(marker); string(b) != "paytiva_tgeorge_bench_hosted_modal_test repo-tgeorge-bench-hosted-modal\n" {
		t.Fatalf("hook args = %q", b)
	}
	if len(got.Cleanup) != 2 || !got.Cleanup[0].OK || got.Cleanup[1].OK || got.Cleanup[1].ExitCode != 1 || !strings.Contains(errOut, "cleanup failed") {
		t.Fatalf("cleanup: %s / %s", out, errOut)
	}
}

func TestDoneRefusesOpenPRAndNeverGuessesTheDefaultBranch(t *testing.T) {
	root, seed, repo := cliRepo(t)
	path, tip := branchWorktree(t, root, repo, "feat/open")
	setFakePRs(t, root, prFake{119, "OPEN", "development", "feat/open", tip, nil})
	code, out, errOut := run(t, "worktree", "done", "feat/open", "--repo", repo)
	if code != 2 || !strings.Contains(errOut, "PR #119 is OPEN into development: not merged") || !strings.Contains(out, `"pr_state": "OPEN"`) {
		t.Fatalf("open PR: exit %d out %s err %s", code, out, errOut)
	}
	// Merged into main by ancestry, but no PR and no --into: refused.
	cliGit(t, seed, "fetch", "-q", "origin")
	cliGit(t, seed, "switch", "-q", "main")
	cliGit(t, seed, "merge", "-q", "--no-ff", "-m", "m", "origin/feat/open")
	cliGit(t, seed, "push", "-q", "origin", "main")
	setFakePRs(t, root)
	code, _, errOut = run(t, "worktree", "done", "feat/open", "--repo", repo)
	if code != 2 || !strings.Contains(errOut, "pass --into") {
		t.Fatalf("no PR, no --into: exit %d err %s", code, errOut)
	}
	code, out, errOut = run(t, "worktree", "done", "feat/open", "--into", "main", "--dry-run", "--repo", repo)
	if code != 0 || !strings.Contains(out, `"merged_via": "contained in origin/main"`) {
		t.Fatalf("--into main: exit %d out %s err %s", code, out, errOut)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestSweepOwnerFilterAndStaleRecords(t *testing.T) {
	root, seed, repo := cliRepo(t)
	mine, tipMine := branchWorktree(t, root, repo, "feat/mine")
	theirs, tipTheirs := branchWorktree(t, root, repo, "feat/theirs")
	_, tipGone := branchWorktree(t, root, repo, "feat/gone")
	for _, b := range []string{"feat/mine", "feat/theirs", "feat/gone"} {
		squashIntoDevelopment(t, seed, b)
	}
	setFakePRs(t, root,
		prFake{1, "MERGED", "development", "feat/mine", tipMine, nil},
		prFake{2, "MERGED", "development", "feat/theirs", tipTheirs, nil},
		prFake{3, "MERGED", "development", "feat/gone", tipGone, nil})
	if code, _, errOut := run(t, "worktree", "adopt", "feat/mine", "--owner", "job-a", "--repo", repo); code != 0 {
		t.Fatalf("adopt: %s", errOut)
	}
	t.Setenv("AGENTFLOW_OWNER", "job-b") // adopt reads the owner from the environment
	if code, _, errOut := run(t, "worktree", "adopt", "feat/theirs", "--repo", repo); code != 0 {
		t.Fatalf("adopt env: %s", errOut)
	}
	if code, _, errOut := run(t, "worktree", "adopt", "feat/theirs", "--owner", "job-a", "--repo", repo); code != 2 || !strings.Contains(errOut, "already owned by job-b") {
		t.Fatalf("re-adopt: exit %d err %s", code, errOut)
	}
	gonePath := filepath.Join(root, "repo-feat-gone")
	if err := os.RemoveAll(gonePath); err != nil {
		t.Fatal(err)
	}

	type sweepOut struct {
		Removable int `json:"removable"`
		Removed   int `json:"removed"`
		Pruned    int `json:"pruned_stale_records"`
		Worktrees []struct {
			Path     string   `json:"path"`
			OK       bool     `json:"ok"`
			Owner    string   `json:"owner"`
			PRNumber int      `json:"pr_number"`
			PRState  string   `json:"pr_state"`
			PRBase   string   `json:"pr_base"`
			Refusals []string `json:"refusals"`
		} `json:"worktrees"`
		Stale []struct {
			Path string `json:"path"`
		} `json:"stale"`
		StalePruned []string `json:"stale_pruned"`
	}
	code, out, errOut := run(t, "worktree", "sweep", "--repo", repo)
	var s sweepOut
	if code != 0 || json.Unmarshal([]byte(out), &s) != nil {
		t.Fatalf("sweep: exit %d out %s err %s", code, out, errOut)
	}
	owners := map[string]string{}
	for _, w := range s.Worktrees {
		owners[w.Path] = w.Owner
		if w.Path == mine && (w.PRNumber != 1 || w.PRState != "MERGED" || w.PRBase != "development" || !w.OK) {
			t.Fatalf("mine: %+v", w)
		}
	}
	if owners[mine] != "job-a" || owners[theirs] != "job-b" {
		t.Fatalf("owners: %v", owners)
	}
	if len(s.Stale) != 1 || s.Stale[0].Path != gonePath || len(s.StalePruned) != 0 {
		t.Fatalf("stale listing: %s", out)
	}

	code, out, errOut = run(t, "worktree", "sweep", "--yes", "--owner", "job-a", "--repo", repo)
	if code != 0 || json.Unmarshal([]byte(out), &s) != nil {
		t.Fatalf("sweep --yes: exit %d out %s err %s", code, out, errOut)
	}
	if s.Removed != 1 {
		t.Fatalf("removed %d, want only job-a's: %s", s.Removed, out)
	}
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Fatalf("job-a's worktree not removed: %v", err)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Fatalf("job-b's worktree was removed: %v", err)
	}
	if !strings.Contains(out, "owned by job-b, not job-a (--owner)") {
		t.Fatalf("filtered worktree not explained: %s", out)
	}
	if len(s.StalePruned) != 1 || s.StalePruned[0] != gonePath || s.Pruned != 1 {
		t.Fatalf("stale not pruned: %s", out)
	}
	if list := cliGit(t, repo, "worktree", "list"); strings.Contains(list, gonePath) {
		t.Fatalf("stale record left: %s", list)
	}
	code, out, _ = run(t, "worktree", "sweep", "--format", "text", "--repo", repo)
	if code != 0 || !strings.Contains(out, "owner: job-b") || !strings.Contains(out, "pr: #2 MERGED into development") {
		t.Fatalf("text sweep: %s", out)
	}
}

func TestWorktreeAdoptUsage(t *testing.T) {
	clearOwnerEnv(t)
	t.Chdir(t.TempDir())
	if code, _, errOut := run(t, "worktree", "adopt"); code != 1 || !strings.Contains(errOut, "exactly one") {
		t.Fatalf("no target: %d %s", code, errOut)
	}
	if code, _, errOut := run(t, "worktree", "adopt", "x"); code != 1 || !strings.Contains(errOut, "no owner") {
		t.Fatalf("no owner: %d %s", code, errOut)
	}
	if code, _, errOut := run(t, "worktree", "sweep", "--owner", "a b"); code != 1 || !strings.Contains(errOut, "--owner") {
		t.Fatalf("bad owner: %d %s", code, errOut)
	}
}
