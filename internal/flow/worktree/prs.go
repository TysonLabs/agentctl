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

// prMergeProof looks for a merged PR that proves w's commits are merged,
// and fills c's PR fields. It reports (true, "", nil) when proven; (false,
// reason, nil) when not, reason naming the PR that explains it ("" when
// there is none or GitHub could not be checked: c.PRError says which).
//
// It asks GitHub for PRs from w.Branch and, only when there is none at all,
// from the branch's upstream on t.Remote (a branch taken over under another
// local name, `git checkout -B takeover/155 origin/feat/x`). A merged PR
// proves the merge when its head is w.Head, or when its head contains
// w.Head: the worktree is behind the merged head and holds no commit of its
// own (a worktree left at an older PR head after a squash merge). The PR
// head is fetched from refs/pull/<n>/head when it is not local.
//
// Only an exact head on w.Branch itself sets c.prProof, the one proof that
// lets Remove delete the remote branch: the other proofs remove the
// worktree and keep every remote branch.
func prMergeProof(ctx context.Context, env Env, w Worktree, t Target, c *Check) (bool, string, error) {
	prs, err := branchPRs(ctx, env, w.Path, t.Remote, w.Branch)
	if err != nil {
		c.PRError = err.Error()
		return false, "", nil
	}
	via := ""
	if len(prs) == 0 {
		if up := upstreamBranch(ctx, env, w.Path, w.Branch, t.Remote); up != "" && up != w.Branch {
			if prs, err = branchPRs(ctx, env, w.Path, t.Remote, up); err != nil {
				c.PRError = err.Error()
				return false, "", nil
			}
			via = up
		}
	}
	proof, related, why := prVerdict(prs, w.Head)
	if proof != nil {
		c.PRNumber, c.PRState, c.PRBase = proof.Number, proof.State, proof.Base
		if via == "" {
			c.prProof, c.prHeadRepo = true, proof.HeadRepo
			c.MergedVia = fmt.Sprintf("PR #%d into %s", proof.Number, proof.Base)
		} else {
			c.MergedVia = fmt.Sprintf("PR #%d into %s (head branch %s, this branch's upstream)", proof.Number, proof.Base, via)
		}
		return true, "", nil
	}
	if related == nil {
		return false, "", nil
	}
	c.PRNumber, c.PRState, c.PRBase = related.Number, related.State, related.Base
	if related.State != "MERGED" {
		return false, why, nil
	}
	// prVerdict gives OPEN PRs precedence, so reaching here means every
	// remaining possible proof is a merged PR. A reused branch can have
	// several of them; do not let a newer, unrelated merge hide an older
	// merged head that contains this worktree's commit.
	for i := range prs {
		p := &prs[i]
		if p.State != "MERGED" {
			continue
		}
		contained, checkErr := containedInPRHead(ctx, env, w.Path, t.Remote, w.Head, *p)
		mismatch := fmt.Sprintf("PR #%d was MERGED into %s at %s, but this worktree is at %s",
			p.Number, p.Base, short(p.HeadOID), short(w.Head))
		switch {
		case checkErr != nil:
			c.PRNumber, c.PRState, c.PRBase = p.Number, p.State, p.Base
			return false, mismatch + ": could not check whether the merged head contains it (" + checkErr.Error() + ")", nil
		case !contained:
			continue
		}
		c.PRNumber, c.PRState, c.PRBase = p.Number, p.State, p.Base
		c.MergedVia = fmt.Sprintf("PR #%d into %s (%s is contained in its merged head %s)",
			p.Number, p.Base, short(w.Head), short(p.HeadOID))
		if via != "" {
			c.MergedVia += fmt.Sprintf("; head branch %s, this branch's upstream", via)
		}
		return true, "", nil
	}
	return false, why + ": its later commits are not merged", nil
}

// upstreamBranch is branch's upstream branch name on remote (from
// branch.<name>.remote and branch.<name>.merge), or "" when it has none or
// it tracks another remote.
func upstreamBranch(ctx context.Context, env Env, dir, branch, remote string) string {
	r, err := run(ctx, dir, env.Git, "config", "--get", "branch."+branch+".remote")
	if err != nil || strings.TrimSpace(r) != remote {
		return ""
	}
	m, err := run(ctx, dir, env.Git, "config", "--get", "branch."+branch+".merge")
	if err != nil {
		return ""
	}
	name, ok := strings.CutPrefix(strings.TrimSpace(m), "refs/heads/")
	if !ok {
		return ""
	}
	return name
}

// containedInPRHead reports whether head is an ancestor of (or is) p's head
// commit. A head commit that is not local is fetched from remote's
// refs/pull/<n>/head (GitHub keeps it after the branch is deleted), bounded
// like a gh call; the fetched object must be p.HeadOID itself.
func containedInPRHead(ctx context.Context, env Env, dir, remote, head string, p PR) (bool, error) {
	if !isHexOID(p.HeadOID) {
		return false, fmt.Errorf("PR head %q is not a commit id", p.HeadOID)
	}
	if _, err := resolveCommit(ctx, env, dir, p.HeadOID); err != nil {
		timeout := env.GHTimeout
		if timeout <= 0 {
			timeout = DefaultGHTimeout
		}
		fctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		ref := fmt.Sprintf("refs/pull/%d/head", p.Number)
		if _, err := run(fctx, dir, env.Git, "-c", "credential.interactive=never", "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", remote, ref); err != nil {
			return false, fmt.Errorf("fetch %s: %v", ref, err)
		}
		if _, err := resolveCommit(ctx, env, dir, p.HeadOID); err != nil {
			return false, fmt.Errorf("%s did not bring PR head %s", ref, short(p.HeadOID))
		}
	}
	cmd := exec.CommandContext(ctx, env.Git, "merge-base", "--is-ancestor", head, p.HeadOID)
	cmd.Dir = dir
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return false, nil
	default:
		return false, fmt.Errorf("git merge-base: %v", err)
	}
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
				// The caller checks whether the merged head contains head
				// and completes the reason (prMergeProof).
				return nil, p, fmt.Sprintf("PR #%d was MERGED into %s at %s, but this worktree is at %s",
					p.Number, p.Base, short(p.HeadOID), short(head))
			default:
				return nil, p, fmt.Sprintf("PR #%d into %s was CLOSED without merging", p.Number, p.Base)
			}
		}
	}
	return nil, nil, ""
}
