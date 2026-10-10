package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/TysonLabs/agentctl/internal/flow/pr"
)

const prOpenUsage = `agentflow pr open --title T (--body-file F | --body TEXT) [flags]

Push the current branch (never forced) and open a pull request for it. If an
open PR for the branch already exists, return it instead of opening another.

  --title T                the PR title
  --body-file F            the PR body, read from file F
  --body TEXT              the PR body (use --body-file for anything long)
  --base B                 the base branch (default: the repository's default
                           branch, read from GitHub; never hard-coded)
  --draft                  open the PR as a draft
  --retarget               an open PR for this branch with another base is
                           moved to B (default: refuse)
  --dir D                  the repository checkout (default: current directory)
  --repo OWNER/NAME        GitHub repository (default: D's, from gh; never
                           guessed)
  --no-review-request      on a non-default base, do not comment the review
                           request (see below)
  --reviewer-mention TEXT  the review-request comment (default
                           "@coderabbitai review")

Every precondition is checked before anything is pushed, and a refusal lists
every failed one: HEAD is on a branch that is not the base, the default
branch or another long-lived branch; no uncommitted changes to tracked
files; the branch has commits that are not in the base; the branch on
origin, if any, is an ancestor of HEAD (a plain push fast-forwards it); the
title and body are not empty; at most one open PR has the branch as head,
and its base is the requested one (or --retarget).

CodeRabbit does not review a PR whose base is not the default branch, so on
such a base pr open comments the review request, once (a rerun finds it).

After opening, the PR is read back: it must be open, on the base, at the
pushed commit and, when pr open sent the body, carry that body. An existing
PR's title and body are never changed.

Output: a JSON result on stdout: status (created, existing, retargeted,
refused, error, unverified, interrupted), number, url, base, head (the
branch), head_sha, review_requested, reasons, next.

Exit codes: 0 created, existing or retargeted · 1 usage error · 2 refused,
nothing pushed or changed · 3 git or gh error (see "error"; the branch may be
pushed) · 4 the PR exists but its read-back or review request failed · 130
interrupted, nothing pushed.
`

var prOpenExitCodes = map[pr.OpenStatus]int{
	pr.OpenCreated:     0,
	pr.OpenExisting:    0,
	pr.OpenRetargeted:  0,
	pr.OpenRefused:     2,
	pr.OpenError:       3,
	pr.OpenUnverified:  4,
	pr.OpenInterrupted: 130,
}

func runPROpen(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow pr open: "+format+"\n", a...)
		return 1
	}
	fs := flag.NewFlagSet("agentflow pr open", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := pr.OpenOptions{ConfirmWait: 15 * time.Second, ConfirmInterval: 2 * time.Second}
	var bodyFile, body, dir, mention string
	var noReview bool
	fs.StringVar(&o.Title, "title", "", "")
	fs.StringVar(&bodyFile, "body-file", "", "")
	fs.StringVar(&body, "body", "", "")
	fs.StringVar(&o.Base, "base", "", "")
	fs.BoolVar(&o.Draft, "draft", false, "")
	fs.BoolVar(&o.Retarget, "retarget", false, "")
	fs.StringVar(&dir, "dir", "", "")
	fs.StringVar(&o.Repo, "repo", "", "")
	fs.BoolVar(&noReview, "no-review-request", false, "")
	fs.StringVar(&mention, "reviewer-mention", pr.DefaultReviewMention, "")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, prOpenUsage)
			return 0
		}
		return fail("%v (see: agentflow pr open --help)", err)
	}
	if len(pos) != 0 {
		return fail("unexpected argument %q", pos[0])
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch {
	case set["body"] && set["body-file"]:
		return fail("--body and --body-file are mutually exclusive")
	case set["body-file"]:
		b, err := os.ReadFile(bodyFile)
		if err != nil {
			return fail("--body-file: %v", err)
		}
		if !utf8.Valid(b) {
			return fail("--body-file %s is not UTF-8 text", bodyFile)
		}
		o.Body = string(b)
	case set["body"]:
		o.Body = body
	default:
		return fail("a body is required: --body-file F or --body TEXT")
	}
	if !set["title"] {
		return fail("--title is required")
	}
	if n := utf8.RuneCountInString(o.Body); n > pr.MaxBodyRunes {
		return fail("the body has %d characters; GitHub allows %d", n, pr.MaxBodyRunes)
	}
	if strings.ContainsAny(o.Title, "\r\n") {
		return fail("--title must be one line")
	}
	if o.Base != "" && !pr.ValidBranch(o.Base) {
		return fail("--base %q is not a plain branch name", o.Base)
	}
	if o.Repo != "" && !validPRRepo(o.Repo) {
		return fail("--repo %q is not OWNER/NAME", o.Repo)
	}
	if noReview && set["reviewer-mention"] {
		return fail("--no-review-request and --reviewer-mention are mutually exclusive")
	}
	if !noReview {
		if strings.TrimSpace(mention) == "" {
			return fail("--reviewer-mention is empty (use --no-review-request to post none)")
		}
		o.ReviewMention = mention
	}
	if dir != "" {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return fail("--dir %q is not a directory", dir)
		}
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return writePROpenResult(pr.OpenResult{Status: pr.OpenError, Repo: o.Repo, Reasons: []string{},
			Next: "install git, then run it again", Error: "git not found on PATH"}, stdout, stderr)
	}
	ghBin := os.Getenv("AGENTFLOW_GH")
	if ghBin == "" {
		if ghBin, err = exec.LookPath("gh"); err != nil {
			return writePROpenResult(pr.OpenResult{Status: pr.OpenError, Repo: o.Repo, Reasons: []string{},
				Next: "install gh or set AGENTFLOW_GH, then run it again", Error: "gh not found on PATH"}, stdout, stderr)
		}
	}
	o.GH = pr.GHCLIIn(ghBin, dir, 60*time.Second)
	// A push can take a while; every other git call is local or a small fetch.
	o.Git = pr.GitCLI(gitBin, dir, 5*time.Minute)

	repo, err := resolvePRRepo(ctx, o.GH, o.Repo)
	if err != nil {
		if ctx.Err() != nil {
			return writePROpenResult(pr.OpenResult{Status: pr.OpenInterrupted, Repo: o.Repo, Reasons: []string{},
				Next: "nothing was pushed; run it again", Error: "interrupted"}, stdout, stderr)
		}
		return writePROpenResult(pr.OpenResult{Status: pr.OpenError, Repo: o.Repo, Reasons: []string{},
			Next: "fix the error, then run it again", Error: err.Error()}, stdout, stderr)
	}
	o.Repo = repo

	res := pr.Open(ctx, o)
	return writePROpenResult(res, stdout, stderr)
}

func writePROpenResult(res pr.OpenResult, stdout, stderr io.Writer) int {
	if res.Reasons == nil {
		res.Reasons = []string{}
	}
	if res.CheckedAt == "" {
		res.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	_, _ = stdout.Write(append(out, '\n'))
	code, ok := prOpenExitCodes[res.Status]
	if !ok {
		fmt.Fprintf(stderr, "agentflow pr open: unknown result status %q\n", res.Status)
		return 1
	}
	if code != 0 {
		msg := res.Error
		if len(res.Reasons) > 0 {
			msg = strings.Join(res.Reasons, "; ")
		}
		fmt.Fprintf(stderr, "agentflow pr open: %s: %s\n", res.Status, msg)
	}
	return code
}

// resolvePRRepo returns --repo, or the directory's repository from gh, and
// validates a derived repository as OWNER/NAME. It never guesses. An explicit
// repository was already validated with the other command-line arguments.
// TODO(#33): pr wait and pr merge still carry their own copies; point them here.
func resolvePRRepo(ctx context.Context, gh pr.GH, repo string) (string, error) {
	if repo == "" {
		out, err := gh(ctx, "repo", "view", "--json", "nameWithOwner")
		if err != nil {
			return "", fmt.Errorf("no --repo and the directory has no GitHub repo: %w", err)
		}
		var rv struct {
			NameWithOwner string `json:"nameWithOwner"`
		}
		if err := json.Unmarshal(out, &rv); err != nil {
			return "", fmt.Errorf("no --repo and gh repo view returned invalid JSON: %w", err)
		}
		if rv.NameWithOwner == "" {
			return "", errors.New("no --repo and gh repo view gave no name")
		}
		repo = rv.NameWithOwner
	}
	if !validPRRepo(repo) {
		return "", fmt.Errorf("gh repo view gave invalid repository %q", repo)
	}
	return repo, nil
}

func validPRRepo(repo string) bool {
	owner, name, _ := strings.Cut(repo, "/")
	return repoRe.MatchString(repo) && owner != "." && owner != ".." && name != "." && name != ".."
}
