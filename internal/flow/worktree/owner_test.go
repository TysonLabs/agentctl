package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveOwnerOrder(t *testing.T) {
	env := map[string]string{"CLAUDE_JOB_DIR": "/Users/x/.claude/jobs/a8dac91d/", "CLAUDE_CODE_SESSION_ID": "sess"}
	get := func(k string) string { return env[k] }
	if o, s := ResolveOwner("me", get); o != "me" || s != "--owner" {
		t.Fatalf("flag: %q %q", o, s)
	}
	if o, s := ResolveOwner("", get); o != "a8dac91d" || s != "CLAUDE_JOB_DIR" {
		t.Fatalf("job dir: %q %q", o, s)
	}
	env["AGENTFLOW_OWNER"] = "bot"
	if o, _ := ResolveOwner("", get); o != "bot" {
		t.Fatalf("AGENTFLOW_OWNER first: %q", o)
	}
	if o, _ := ResolveOwner("", func(string) string { return "" }); o != "" {
		t.Fatalf("none: %q", o)
	}
	for _, bad := range []string{"", "a b", "x\n", string(make([]byte, 129))} {
		if ValidOwner(bad) == nil {
			t.Errorf("ValidOwner(%q) accepted", bad)
		}
	}
}

func TestNewRecordsOwner(t *testing.T) {
	f := newFixture(t)
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	c, err := f.newWT(NewOptions{Branch: "feat/owned", Owner: "job-1", OwnerSource: "--owner", Now: now})
	if err != nil || c.Owner != "job-1" {
		t.Fatalf("new: %+v %v", c, err)
	}
	list, _ := List(context.Background(), f.env, f.repo)
	w, _ := Find(list, f.repo, "feat/owned")
	rec, err := ReadOwner(context.Background(), f.env, f.repo, w)
	if err != nil || rec == nil || rec.Owner != "job-1" || rec.Via != "new" || rec.Source != "--owner" || !rec.Recorded.Equal(now) {
		t.Fatalf("owner record: %+v %v", rec, err)
	}
	// The record lives outside the tree: the worktree stays clean.
	if st := git(t, c.Path, "status", "--porcelain", "--untracked-files=all"); st != "" {
		t.Fatalf("owner record dirtied the tree: %q", st)
	}
	if _, err := f.newWT(NewOptions{Branch: "feat/bad-owner", Owner: "a b"}); !refusedWith(err, "owner") {
		t.Fatalf("invalid owner: %v", err)
	}
}

func TestAdoptStampsPlainWorktreeAndRefusesAnotherOwner(t *testing.T) {
	f := newFixture(t)
	// Created by plain `git worktree add` beside the main checkout.
	path := filepath.Join(f.root, "repo-sibling")
	git(t, f.repo, "worktree", "add", "-q", "-b", "feat/plain", path, "origin/main")
	ctx := context.Background()
	list, _ := List(ctx, f.env, f.repo)
	w, _ := Find(list, f.repo, "feat/plain")
	if rec, err := ReadOwner(ctx, f.env, f.repo, w); err != nil || rec != nil {
		t.Fatalf("plain worktree has an owner: %+v %v", rec, err)
	}
	a, err := Adopt(ctx, f.env, f.repo, w, "job-a", "--owner", time.Now())
	if err != nil || a.Owner.Owner != "job-a" || a.Owner.Via != "adopt" || a.Unchanged {
		t.Fatalf("adopt: %+v %v", a, err)
	}
	if a, err := Adopt(ctx, f.env, f.repo, w, "job-a", "--owner", time.Now()); err != nil || !a.Unchanged {
		t.Fatalf("same owner again must be a no-op: %+v %v", a, err)
	}
	if _, err := Adopt(ctx, f.env, f.repo, w, "job-b", "--owner", time.Now()); !refusedWith(err, "already owned by job-a") {
		t.Fatalf("another owner: %v", err)
	}
	if _, err := Adopt(ctx, f.env, f.repo, list[0], "job-a", "", time.Now()); !refusedWith(err, "main working tree") {
		t.Fatalf("main tree: %v", err)
	}
	// Readable after the directory is gone (for sweep's stale list).
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if rec, err := ReadOwner(ctx, f.env, f.repo, w); err != nil || rec == nil || rec.Owner != "job-a" {
		t.Fatalf("stale owner: %+v %v", rec, err)
	}
}

func TestUnreadableOwnerRecordIsNotOverwritten(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.root, "wt-junk")
	git(t, f.repo, "worktree", "add", "-q", "-b", "feat/junk", path, "origin/main")
	ctx := context.Background()
	dir, err := adminDir(ctx, f.env, f.repo, path)
	if err != nil || dir == "" {
		t.Fatal(dir, err)
	}
	writeFile(t, dir, ownerFile, "{not json")
	list, _ := List(ctx, f.env, f.repo)
	w, _ := Find(list, f.repo, "feat/junk")
	if _, err := Adopt(ctx, f.env, f.repo, w, "job-a", "", time.Now()); !refusedWith(err, "not overwritten") {
		t.Fatalf("got %v", err)
	}
}
