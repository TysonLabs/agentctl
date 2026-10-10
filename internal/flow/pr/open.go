package pr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// OpenStatus is the outcome of one pr open. Every run ends in exactly one.
type OpenStatus string

const (
	OpenCreated     OpenStatus = "created"     // a new PR was opened and read back
	OpenExisting    OpenStatus = "existing"    // an open PR for this branch already had the base
	OpenRetargeted  OpenStatus = "retargeted"  // an open PR for this branch was moved to the base (--retarget)
	OpenRefused     OpenStatus = "refused"     // a precondition failed: reasons listed, nothing pushed or changed
	OpenError       OpenStatus = "error"       // git or gh failed; see error (a push may have happened)
	OpenUnverified  OpenStatus = "unverified"  // the PR exists, but its read-back or review request failed
	OpenInterrupted OpenStatus = "interrupted" // cancelled before anything was pushed
)

// DefaultReviewMention is the comment that asks CodeRabbit to review a PR it
// skipped: it does not review PRs whose base is not the default branch.
const DefaultReviewMention = "@coderabbitai review"

// MaxBodyRunes is GitHub's limit on a PR body.
const MaxBodyRunes = 65536

// Git runs git with args in the repository and returns its stdout.
type Git func(ctx context.Context, args ...string) (string, error)

// GitCLI runs bin (git) in dir.
func GitCLI(bin, dir string, perCall time.Duration) Git {
	return func(ctx context.Context, args ...string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, perCall)
		defer cancel()
		cmd := exec.CommandContext(cctx, bin, args...)
		cmd.Dir = dir
		cmd.WaitDelay = time.Second
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			return stdout.String(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
		}
		return stdout.String(), nil
	}
}

// OpenOptions configures one pr open.
type OpenOptions struct {
	Repo     string // owner/name, already resolved and validated
	Base     string // "" = the repository's default branch on GitHub
	Title    string
	Body     string
	Draft    bool
	Retarget bool // move an existing PR for this branch to Base instead of refusing
	// ReviewMention is posted as a comment when the base is not the default
	// branch (CodeRabbit skips those). "" posts nothing.
	ReviewMention string

	// ConfirmWait bounds how long the PR is re-read until its head is the
	// pushed commit (GitHub updates an existing PR's head asynchronously).
	ConfirmWait     time.Duration
	ConfirmInterval time.Duration
	GH              GH
	Git             Git
}

// OpenResult is the machine-readable summary.
type OpenResult struct {
	Status          OpenStatus `json:"status"`
	Repo            string     `json:"repo"`
	Number          int        `json:"number,omitempty"`
	URL             string     `json:"url,omitempty"`
	Base            string     `json:"base,omitempty"`
	DefaultBranch   string     `json:"default_branch,omitempty"`
	Head            string     `json:"head,omitempty"`     // the branch
	HeadSHA         string     `json:"head_sha,omitempty"` // the commit pushed and read back
	PreviousBase    string     `json:"previous_base,omitempty"`
	ReviewRequested bool       `json:"review_requested"`
	Reasons         []string   `json:"reasons"`
	Next            string     `json:"next,omitempty"`
	CheckedAt       string     `json:"checked_at"`
	Error           string     `json:"error,omitempty"`
}

// longLivedBranches are never the head of a PR opened by pr open.
var longLivedBranches = map[string]bool{
	"main": true, "master": true, "trunk": true, "develop": true, "development": true,
	"dev": true, "staging": true, "production": true, "prod": true,
}

// existingPR is one open PR whose head is the branch, from gh pr list.
type existingPR struct {
	Number              int    `json:"number"`
	URL                 string `json:"url"`
	BaseRefName         string `json:"baseRefName"`
	HeadRefName         string `json:"headRefName"`
	IsCrossRepository   *bool  `json:"isCrossRepository"`
	HeadRepositoryOwner *struct {
		Login string `json:"login"`
	} `json:"headRepositoryOwner"`
}

// Open pushes the current branch (never forced) and opens a PR for it
// against the base, or returns the open PR that already exists for it.
// Every precondition is checked before anything is pushed, and a refusal
// lists every failed one.
func Open(ctx context.Context, o OpenOptions) OpenResult {
	res := OpenResult{Repo: o.Repo, Reasons: []string{}}
	finish := func(s OpenStatus, next, msg string) OpenResult {
		res.Status, res.Next, res.Error = s, next, msg
		res.CheckedAt = time.Now().UTC().Format(time.RFC3339)
		return res
	}
	fail := func(err error) OpenResult {
		if ctx.Err() != nil {
			return finish(OpenInterrupted, "nothing was pushed; run it again", "interrupted")
		}
		return finish(OpenError, "fix the error, then run it again", err.Error())
	}

	// 1. Local state: the commit, the branch, a clean tree. HEAD is read
	// first so "not a repository" is an error, not a detached HEAD.
	headOut, err := o.Git(ctx, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fail(err)
	}
	head := strings.ToLower(strings.TrimSpace(headOut))
	if !fullSHARe.MatchString(head) {
		return fail(fmt.Errorf("git rev-parse HEAD gave %q, not a commit sha", head))
	}
	res.HeadSHA = head
	branchOut, err := o.Git(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	branch := strings.TrimSpace(branchOut)
	if err != nil || branch == "" {
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		res.Reasons = append(res.Reasons, "HEAD is detached: check out a branch first")
		branch = ""
	} else {
		res.Head = branch
	}
	status, err := o.Git(ctx, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return fail(err)
	}

	// 2. The base, from GitHub, never hard-coded.
	def, err := defaultBranch(ctx, o)
	if err != nil {
		return fail(err)
	}
	res.DefaultBranch = def
	base := o.Base
	if base == "" {
		base = def
	}
	res.Base = base

	if strings.TrimSpace(status) != "" {
		res.Reasons = append(res.Reasons, "the working tree has uncommitted changes to tracked files (they would not be in the PR): commit or stash them")
	}
	if branch != "" {
		switch {
		case branch == base:
			res.Reasons = append(res.Reasons, fmt.Sprintf("the current branch %q is the base: create a feature branch", branch))
		case branch == def || longLivedBranches[strings.ToLower(branch)]:
			res.Reasons = append(res.Reasons, fmt.Sprintf("the current branch %q is a long-lived branch: create a feature branch", branch))
		}
		if !ValidBranch(branch) {
			res.Reasons = append(res.Reasons, fmt.Sprintf("the branch name %q is not a plain branch name", branch))
		}
	}
	if strings.TrimSpace(o.Title) == "" {
		res.Reasons = append(res.Reasons, "the title is empty")
	}
	if strings.TrimSpace(o.Body) == "" {
		res.Reasons = append(res.Reasons, "the body is empty (a failed step that writes the body file leaves it empty)")
	}
	if remote, ok := originRepo(ctx, o.Git); ok && !strings.EqualFold(remote, o.Repo) {
		res.Reasons = append(res.Reasons, fmt.Sprintf("the origin remote is %s, not %s (pull requests from forks are not supported)", remote, o.Repo))
	}

	// 3. Remote state: the base exists and is behind HEAD; the branch on
	// origin (if any) is an ancestor of HEAD, so a plain push fast-forwards.
	baseSHA, err := remoteBranchSHA(ctx, o.Git, base)
	if err != nil {
		return fail(err)
	}
	if baseSHA == "" {
		res.Reasons = append(res.Reasons, fmt.Sprintf("the base branch %q does not exist on origin", base))
	} else {
		if _, err := o.Git(ctx, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "origin",
			baseSHA); err != nil {
			return fail(err)
		}
		n, err := o.Git(ctx, "rev-list", "--count", baseSHA+"..HEAD")
		if err != nil {
			return fail(err)
		}
		if strings.TrimSpace(n) == "0" {
			res.Reasons = append(res.Reasons, fmt.Sprintf("the branch has no commits that are not in %s", base))
		}
	}
	if branch != "" {
		branchSHA, err := remoteBranchSHA(ctx, o.Git, branch)
		if err != nil {
			return fail(err)
		}
		if branchSHA != "" && branchSHA != head {
			if _, err := o.Git(ctx, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "origin",
				branchSHA); err != nil {
				return fail(err)
			}
			if _, err := o.Git(ctx, "merge-base", "--is-ancestor", branchSHA, "HEAD"); err != nil {
				if ctx.Err() != nil {
					return fail(ctx.Err())
				}
				res.Reasons = append(res.Reasons, fmt.Sprintf("origin/%s has commits this branch does not (behind or diverged): pull or rebase; pr open never force-pushes", branch))
			}
		}
	}

	// 4. An open PR for this branch already?
	var prs []existingPR
	if branch != "" {
		prs, err = openPRsForBranch(ctx, o, branch)
		if err != nil {
			return fail(err)
		}
	}
	var existing *existingPR
	switch len(prs) {
	case 0:
	case 1:
		existing = &prs[0]
		if existing.BaseRefName != base && !o.Retarget {
			res.Number, res.URL = existing.Number, existing.URL
			res.Reasons = append(res.Reasons, fmt.Sprintf("open PR #%d for this branch targets %s, not %s: pass --base %s to keep it, or --retarget to move it",
				existing.Number, existing.BaseRefName, base, existing.BaseRefName))
		}
	default:
		nums := make([]string, len(prs))
		for i, p := range prs {
			nums[i] = "#" + strconv.Itoa(p.Number)
		}
		res.Reasons = append(res.Reasons, "more than one open PR has this branch as its head: "+strings.Join(nums, ", "))
	}
	if len(res.Reasons) > 0 {
		return finish(OpenRefused, "fix the reasons listed, then run it again", "")
	}
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}

	// 5. Push. A plain push: git refuses anything but a fast-forward.
	if _, err := o.Git(ctx, "push", "--quiet", "-u", "origin", "HEAD:refs/heads/"+branch); err != nil {
		if ctx.Err() != nil {
			dctx := context.WithoutCancel(ctx)
			pushedSHA, checkErr := remoteBranchSHA(dctx, o.Git, branch)
			switch {
			case checkErr != nil:
				return finish(OpenError, fmt.Sprintf("the push outcome is unknown; inspect origin/%s, then run it again", branch),
					"push was interrupted and its outcome could not be verified: "+checkErr.Error())
			case pushedSHA == head:
				return finish(OpenError, "the branch was pushed to the intended commit; run it again to open or find the PR",
					fmt.Sprintf("push was interrupted after origin/%s reached %s", branch, head))
			}
		}
		return fail(err)
	}

	// From here the push has landed, so the rest runs detached from
	// cancellation and reports what happened.
	dctx := context.WithoutCancel(ctx)
	outcome := OpenExisting
	var sentBody *string
	if existing != nil {
		res.Number, res.URL = existing.Number, existing.URL
		if existing.BaseRefName != base {
			res.PreviousBase = existing.BaseRefName
			if _, err := o.GH(dctx, "api", "-X", "PATCH", fmt.Sprintf("repos/%s/pulls/%d", o.Repo, existing.Number), "-f", "base="+base); err != nil {
				return finish(OpenError, "the branch was pushed; fix the error, then run it again", "retarget: "+err.Error())
			}
			outcome = OpenRetargeted
		}
	} else {
		args := []string{"api", "-X", "POST", fmt.Sprintf("repos/%s/pulls", o.Repo),
			"-f", "title=" + o.Title, "-f", "head=" + branch, "-f", "base=" + base, "-f", "body=" + o.Body,
			"-F", "draft=" + strconv.FormatBool(o.Draft)}
		out, err := o.GH(dctx, args...)
		if err != nil {
			return finish(OpenError, "the branch was pushed; run it again (it returns the PR if one was created)", "create: "+err.Error())
		}
		var created struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
		}
		if err := json.Unmarshal(out, &created); err != nil || created.Number <= 0 {
			return finish(OpenUnverified, "check the PR list for this branch on GitHub", "create returned no PR number")
		}
		res.Number, res.URL = created.Number, created.HTMLURL
		outcome = OpenCreated
		sentBody = &o.Body
	}

	// 6. Read back: base, head commit and (when we sent it) the body.
	if problems, err := confirmOpened(dctx, o, res.Number, base, head, sentBody, &res); err != nil {
		return finish(OpenUnverified, "check the PR on GitHub", "read-back: "+err.Error())
	} else if len(problems) > 0 {
		return finish(OpenUnverified, "fix the PR on GitHub (gh pr edit), then run it again", strings.Join(problems, "; "))
	}

	// 7. CodeRabbit skips a PR whose base is not the default branch.
	next := fmt.Sprintf("wait for the review: agentflow pr wait %d (in the background)", res.Number)
	if base != def && o.ReviewMention != "" {
		done, err := requestReview(dctx, o, res.Number)
		if err != nil {
			return finish(OpenUnverified, fmt.Sprintf("comment %q on the PR by hand", o.ReviewMention), "review request: "+err.Error())
		}
		res.ReviewRequested = done
	}
	if o.Draft {
		next = "the PR is a draft: mark it ready for review when it is, then " + next
	}
	return finish(outcome, next, "")
}

// defaultBranch reads the repository's default branch from GitHub.
func defaultBranch(ctx context.Context, o OpenOptions) (string, error) {
	out, err := o.GH(ctx, "repo", "view", o.Repo, "--json", "defaultBranchRef")
	if err != nil {
		return "", err
	}
	var rv struct {
		DefaultBranchRef *struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
	}
	if err := json.Unmarshal(out, &rv); err != nil {
		return "", fmt.Errorf("gh repo view: %w", err)
	}
	if rv.DefaultBranchRef == nil || !ValidBranch(rv.DefaultBranchRef.Name) {
		return "", errors.New("gh repo view gave no usable default branch")
	}
	return rv.DefaultBranchRef.Name, nil
}

// remoteBranchSHA returns the commit origin's branch points at, or "" when
// origin has no such branch.
func remoteBranchSHA(ctx context.Context, git Git, branch string) (string, error) {
	out, err := git(ctx, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && ref == "refs/heads/"+branch {
			sha = strings.ToLower(sha)
			if !fullSHARe.MatchString(sha) {
				return "", fmt.Errorf("git ls-remote gave %q for %s, not a commit sha", sha, branch)
			}
			return sha, nil
		}
	}
	return "", nil
}

var scpLikeRe = regexp.MustCompile(`^(?:[^@/]+@)?[^:/]+:/?(.+)$`)

// originRepo returns OWNER/NAME from origin's URL when it looks like a
// hosted remote. A local or unparseable URL returns ok=false; GitHub then
// refuses the create if the branch is not in the repository, and the
// read-back catches a same-named branch at another commit.
func originRepo(ctx context.Context, git Git) (string, bool) {
	out, err := git(ctx, "remote", "get-url", "origin")
	if err != nil {
		return "", false
	}
	raw := strings.TrimSpace(out)
	var path string
	if i := strings.Index(raw, "://"); i >= 0 {
		if strings.HasPrefix(strings.ToLower(raw), "file://") {
			return "", false
		}
		_, rest, ok := strings.Cut(raw[i+3:], "/")
		if !ok {
			return "", false
		}
		path = rest
	} else if m := scpLikeRe.FindStringSubmatch(raw); m != nil && !filepath.IsAbs(raw) {
		path = m[1]
	} else {
		return "", false
	}
	parts := strings.Split(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

// openPRsForBranch lists the open PRs in the repository whose head is this
// branch of this repository (a fork's same-named branch is not ours).
func openPRsForBranch(ctx context.Context, o OpenOptions, branch string) ([]existingPR, error) {
	out, err := o.GH(ctx, "pr", "list", "-R", o.Repo, "--head", branch, "--state", "open", "--limit", "100",
		"--json", "number,url,baseRefName,headRefName,isCrossRepository,headRepositoryOwner")
	if err != nil {
		return nil, err
	}
	var all []existingPR
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	owner, _, _ := strings.Cut(o.Repo, "/")
	var mine []existingPR
	for _, p := range all {
		if p.HeadRefName != branch {
			continue
		}
		if p.IsCrossRepository == nil {
			return nil, fmt.Errorf("gh pr list: PR #%d has no isCrossRepository", p.Number)
		}
		if *p.IsCrossRepository {
			continue
		}
		if p.HeadRepositoryOwner != nil && p.HeadRepositoryOwner.Login != "" && !strings.EqualFold(p.HeadRepositoryOwner.Login, owner) {
			continue
		}
		mine = append(mine, p)
	}
	return mine, nil
}

// normalizeBody is what GitHub keeps of a body: line endings normalized,
// surrounding whitespace not significant.
func normalizeBody(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
}

// confirmOpened re-reads the PR until its head is the pushed commit (up to
// ConfirmWait), then checks the base and, when one was sent, the body. It
// returns the mismatches it found; err is a failed or unparseable read.
func confirmOpened(ctx context.Context, o OpenOptions, number int, base, head string, body *string, res *OpenResult) ([]string, error) {
	deadline := time.Now().Add(o.ConfirmWait)
	for {
		out, err := o.GH(ctx, "pr", "view", strconv.Itoa(number), "-R", o.Repo, "--json", "number,url,state,baseRefName,headRefOid,body")
		if err != nil {
			return nil, err
		}
		var pv struct {
			Number      int     `json:"number"`
			URL         string  `json:"url"`
			State       string  `json:"state"`
			BaseRefName string  `json:"baseRefName"`
			HeadRefOid  string  `json:"headRefOid"`
			Body        *string `json:"body"`
		}
		if err := json.Unmarshal(out, &pv); err != nil {
			return nil, fmt.Errorf("gh pr view: %w", err)
		}
		if pv.URL != "" {
			res.URL = pv.URL
		}
		got := strings.ToLower(pv.HeadRefOid)
		if got == head || !time.Now().Before(deadline) {
			var problems []string
			if pv.Number != number {
				problems = append(problems, fmt.Sprintf("read back PR #%d, not #%d", pv.Number, number))
			}
			if pv.State != "OPEN" {
				problems = append(problems, fmt.Sprintf("the PR is %s, not open", strings.ToLower(pv.State)))
			}
			if pv.BaseRefName != base {
				problems = append(problems, fmt.Sprintf("the PR base is %q, not %q", pv.BaseRefName, base))
			}
			if got != head {
				problems = append(problems, fmt.Sprintf("the PR head is %s, not the pushed %s", got, head))
			}
			if body != nil {
				switch {
				case pv.Body == nil:
					problems = append(problems, "the PR read back with no body field")
				case normalizeBody(*pv.Body) != normalizeBody(*body):
					problems = append(problems, fmt.Sprintf("the PR body has %d characters, not the %d sent",
						len([]rune(normalizeBody(*pv.Body))), len([]rune(normalizeBody(*body)))))
				}
			}
			return problems, nil
		}
		time.Sleep(min(o.ConfirmInterval, time.Until(deadline)))
	}
}

// requestReview posts ReviewMention unless the PR already has a comment
// that is exactly it, so a rerun never posts it twice. It reports whether
// the PR now carries the request.
func requestReview(ctx context.Context, o OpenOptions, number int) (bool, error) {
	out, err := o.GH(ctx, "api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", o.Repo, number))
	if err != nil {
		return false, err
	}
	var pages [][]struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(out, &pages); err != nil {
		return false, fmt.Errorf("gh api issue comments: %w", err)
	}
	want := strings.TrimSpace(o.ReviewMention)
	for _, page := range pages {
		for _, c := range page {
			if strings.TrimSpace(c.Body) == want {
				return true, nil
			}
		}
	}
	if _, err := o.GH(ctx, "api", "-X", "POST", fmt.Sprintf("repos/%s/issues/%d/comments", o.Repo, number), "-f", "body="+o.ReviewMention); err != nil {
		return false, err
	}
	return true, nil
}
