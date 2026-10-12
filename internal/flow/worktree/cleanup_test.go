package worktree

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSplitCommand(t *testing.T) {
	cases := map[string][]string{
		`drop.sh {slug}`:               {"drop.sh", "{slug}"},
		`  a   'b c'  "d \" e" f'g'h `: {"a", "b c", `d " e`, "fgh"},
		`mysql -e "DROP DATABASE IF EXISTS paytiva_{slug_snake}_test"`: {"mysql", "-e", "DROP DATABASE IF EXISTS paytiva_{slug_snake}_test"},
	}
	for in, want := range cases {
		if got, err := SplitCommand(in); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("SplitCommand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "   ", `a 'b`, `a "b`} {
		if _, err := SplitCommand(bad); err == nil {
			t.Errorf("SplitCommand(%q) accepted", bad)
		}
	}
}

func TestRunCleanupSuccessFailureTimeoutAndNoWordSplitting(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "args.txt")
	writeFile(t, dir, "rec.sh", "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > "+out+"\nprintf 'env=%s\\n' \"$AGENTFLOW_WORKTREE_BRANCH\" >> "+out+"\n")
	writeFile(t, dir, "fail.sh", "#!/bin/sh\necho 'db busy' >&2\nexit 4\n")
	writeFile(t, dir, "hang.sh", "#!/bin/sh\nsleep 30 &\nwait\n")
	for _, s := range []string{"rec.sh", "fail.sh", "hang.sh"} {
		if err := os.Chmod(filepath.Join(dir, s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A branch value with shell metacharacters stays one argument.
	vars := CleanupVars("/x/.claude/worktrees/feat-a", "feat/a;rm")
	start := time.Now()
	res := RunCleanup(context.Background(), dir, []string{
		"./rec.sh {branch} paytiva_{slug_snake}_test {dir}",
		"./fail.sh",
		"./hang.sh",
		"'unterminated",
	}, vars, 500*time.Millisecond)
	if time.Since(start) > 10*time.Second {
		t.Fatalf("cleanup was not bounded: %s", time.Since(start))
	}
	if len(res) != 4 {
		t.Fatalf("results: %+v", res)
	}
	if !res[0].OK || res[0].ExitCode != 0 {
		t.Fatalf("success: %+v", res[0])
	}
	got, _ := os.ReadFile(out)
	if want := "feat/a;rm\npaytiva_feat_a_rm_test\nfeat-a\nenv=feat/a;rm\n"; string(got) != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
	if res[1].OK || res[1].ExitCode != 4 || !strings.Contains(res[1].Output, "db busy") {
		t.Fatalf("failure: %+v", res[1])
	}
	if res[2].OK || !res[2].TimedOut || res[2].ExitCode != -1 {
		t.Fatalf("timeout: %+v", res[2])
	}
	if res[3].OK || !strings.Contains(res[3].Error, "not run") {
		t.Fatalf("parse error: %+v", res[3])
	}
}

func TestCleanupCommandsReadsLocalGitConfig(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	global := filepath.Join(t.TempDir(), "global.gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	git(t, f.repo, "config", "--global", "--add", CleanupKey, "must-not-run")
	if cmds, err := CleanupCommands(ctx, f.env, f.repo); err != nil || len(cmds) != 0 {
		t.Fatalf("global config must not enable a per-repository hook: %q %v", cmds, err)
	}
	git(t, f.repo, "config", "--add", CleanupKey, "a {slug}")
	git(t, f.repo, "config", "--add", CleanupKey, "b")
	if cmds, err := CleanupCommands(ctx, f.env, f.repo); err != nil || !reflect.DeepEqual(cmds, []string{"a {slug}", "b"}) {
		t.Fatalf("set: %q %v", cmds, err)
	}
}
