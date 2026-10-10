package gate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitEnvBlock are variables that point git at another repository or index.
// They leak in from git hooks; neither our git calls nor the steps want them.
var gitEnvBlock = []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_COMMON_DIR=", "GIT_OBJECT_DIRECTORY=", "GIT_PREFIX="}

func cleanEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		blocked := false
		for _, b := range gitEnvBlock {
			if strings.HasPrefix(kv, b) {
				blocked = true
				break
			}
		}
		if !blocked {
			out = append(out, kv)
		}
	}
	return out
}

// git runs git in dir with a clean environment plus extra, returning stdout.
func git(ctx context.Context, dir string, extra []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cleanEnv(os.Environ()), extra...)
	cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// gitDirs returns the canonical work tree root and git common dir of dir.
// Every worktree of one repository shares the common dir.
func gitDirs(ctx context.Context, dir string) (root, common string, err error) {
	out, err := git(ctx, dir, nil, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
		return "", "", fmt.Errorf("git rev-parse: unexpected output (a bare repository?)")
	}
	if root, err = filepath.EvalSymlinks(lines[0]); err != nil {
		return "", "", err
	}
	if common, err = filepath.EvalSymlinks(lines[1]); err != nil {
		return "", "", err
	}
	return root, common, nil
}

// treeState is the content identity of a work tree.
type treeState struct {
	head  string // HEAD commit, "" before the first commit
	tree  string // git tree of the work tree as it is on disk
	dirty bool   // tree differs from HEAD's tree
}

// workTree hashes the work tree exactly as `git add -A && git write-tree`
// would, but into a throwaway copy of the index, so the real index, HEAD and
// refs are untouched. Tracked edits, staged or not, and untracked files that
// are not ignored all change the hash; a clean checkout hashes to
// HEAD^{tree}, so a tree gated before its commit still matches after it.
// The only write is content-addressed blobs in the object store, the same
// ones `git add` would create.
func workTree(ctx context.Context, root string) (treeState, error) {
	var ts treeState
	// One call for the index path, HEAD and HEAD's tree; it fails in a repo
	// with no commit yet, and then only the index path is asked for.
	var index, headTree string
	out, err := git(ctx, root, nil, "rev-parse", "--path-format=absolute", "--git-path", "index", "HEAD^{commit}", "HEAD^{tree}", "--")
	if err == nil {
		lines := strings.Split(out, "\n")
		if len(lines) != 4 || lines[3] != "--" {
			return ts, fmt.Errorf("git rev-parse: unexpected output")
		}
		index, ts.head, headTree = lines[0], lines[1], lines[2]
	} else if index, err = git(ctx, root, nil, "rev-parse", "--path-format=absolute", "--git-path", "index"); err != nil {
		return ts, err
	}
	tmpDir, err := os.MkdirTemp("", "agentflow-gate-index-")
	if err != nil {
		return ts, err
	}
	defer os.RemoveAll(tmpDir)
	tmpIndex := filepath.Join(tmpDir, "index")
	// Starting from the real index keeps git's stat cache, so unchanged
	// files are not rehashed. A missing index (a fresh repo) starts empty.
	if err := copyFile(index, tmpIndex); err != nil && !os.IsNotExist(err) {
		return ts, fmt.Errorf("copying the git index: %v", err)
	}
	env := []string{"GIT_INDEX_FILE=" + tmpIndex}
	if _, err := git(ctx, root, env, "add", "--all", "--", ":/"); err != nil {
		return ts, err
	}
	if ts.tree, err = git(ctx, root, env, "write-tree"); err != nil {
		return ts, err
	}
	ts.dirty = ts.head == "" || headTree != ts.tree
	return ts, nil
}

// revTree resolves rev to its tree.
func revTree(ctx context.Context, root, rev string) (string, error) {
	out, err := git(ctx, root, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("cannot resolve %q to a tree", rev)
	}
	return out, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
