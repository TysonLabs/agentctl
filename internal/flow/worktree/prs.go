package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultGHTimeout bounds one gh call when Env.GHTimeout is unset.
const DefaultGHTimeout = 30 * time.Second

// PR is one GitHub pull request whose head branch is a worktree's branch.
type PR struct {
	Number   int
	State    string // OPEN, CLOSED or MERGED
	Base     string // the PR's base branch now (after any retarget)
	HeadRef  string
	HeadOID  string
	HeadRepo string // owner/name of the head repository; "" = unknown
}

// prJQ prints one tab-separated line per PR. Every field is always present,
// so a line with a different field count is malformed output.
const prJQ = `.[] | "\(.number)\t\(.state)\t\(.baseRefName // "")\t\(.headRefName // "")\t\(.headRefOid // "")\t\(.headRepository.nameWithOwner // "")"`

// branchPRs lists the PRs of remote's repository (any state, any base)
// whose head branch is exactly branch. gh runs with stdin closed and a
// bounded timeout. Every failure is an error: callers must treat it as
// "unknown", never as "no PR".
func branchPRs(ctx context.Context, env Env, dir, remote, branch string) ([]PR, error) {
	if env.GH == "" {
		return nil, errors.New("gh is not installed")
	}
	repo, _, ok := githubRemote(ctx, env, dir, remote)
	if !ok {
		// A non-GitHub remote: pass the raw URL so a real gh fails closed
		// instead of guessing a repository from the directory.
		out, err := run(ctx, dir, env.Git, "remote", "get-url", remote)
		if err != nil {
			return nil, err
		}
		repo = strings.TrimSpace(out)
	}
	timeout := env.GHTimeout
	if timeout <= 0 {
		timeout = DefaultGHTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := []string{"pr", "list", "--repo", repo, "--head", branch, "--state", "all", "--limit", "100",
		"--json", "number,state,baseRefName,headRefName,headRefOid,headRepository", "--jq", prJQ}
	cmd := exec.CommandContext(cctx, env.GH, args...)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second // stdin is nil (/dev/null); output pipes close on kill
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("gh pr list timed out after %s", timeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s pr list: %s", filepath.Base(env.GH), msg)
	}
	var prs []PR
	for _, line := range nonEmptyLines(stdout.String()) {
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			return nil, fmt.Errorf("unexpected gh pr list output %q", line)
		}
		n, err := strconv.Atoi(f[0])
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("unexpected PR number in gh output %q", line)
		}
		state := strings.ToUpper(f[1])
		switch state {
		case "OPEN", "CLOSED", "MERGED":
		default:
			return nil, fmt.Errorf("unexpected PR state in gh output %q", line)
		}
		if f[2] == "" {
			return nil, fmt.Errorf("missing PR base in gh output %q", line)
		}
		if !isHexOID(f[4]) {
			return nil, fmt.Errorf("unexpected PR head OID in gh output %q", line)
		}
		// gh's --head filter is the authority on the branch, but check it
		// again: the proof must be about exactly this branch.
		if f[3] != branch {
			continue
		}
		prs = append(prs, PR{Number: n, State: state, Base: f[2], HeadRef: f[3], HeadOID: f[4], HeadRepo: f[5]})
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number > prs[j].Number })
	return prs, nil
}

// prVerdict picks the merged PR whose head is exactly head (the proof), or
// else the PR that best explains why there is none, with a one-line reason.
// prs must be sorted newest first.
func prVerdict(prs []PR, head string) (proof, related *PR, detail string) {
	for i := range prs {
		if prs[i].State == "MERGED" && strings.EqualFold(prs[i].HeadOID, head) {
			return &prs[i], nil, ""
		}
	}
	for _, want := range []string{"OPEN", "MERGED", "CLOSED"} {
		for i := range prs {
			if prs[i].State != want {
				continue
			}
			p := &prs[i]
			switch want {
			case "OPEN":
				return nil, p, fmt.Sprintf("PR #%d is OPEN into %s: not merged", p.Number, p.Base)
			case "MERGED":
				return nil, p, fmt.Sprintf("PR #%d was MERGED into %s at %s, but this worktree is at %s: its later commits are not merged",
					p.Number, p.Base, short(p.HeadOID), short(head))
			default:
				return nil, p, fmt.Sprintf("PR #%d into %s was CLOSED without merging", p.Number, p.Base)
			}
		}
	}
	return nil, nil, ""
}
