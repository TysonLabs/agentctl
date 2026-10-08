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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/pr"
)

var prMergeExitCodes = map[pr.MergeStatus]int{
	pr.MergeMerged:      0,
	pr.MergeGHError:     1,
	pr.MergeRefused:     2,
	pr.MergeSyncFailed:  3,
	pr.MergeUnconfirmed: 4,
	pr.MergeInterrupted: 130,
}

var fullSHARe = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func runPRMerge(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow pr merge: "+format+"\n", a...)
		return 1
	}
	fs := flag.NewFlagSet("agentflow pr merge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := pr.MergeOptions{
		MergeableWait: 30 * time.Second, MergeableInterval: 3 * time.Second,
		ConfirmWait: 20 * time.Second, ConfirmInterval: 2 * time.Second,
	}
	fs.StringVar(&o.Repo, "repo", "", "")
	fs.StringVar(&o.Method, "method", "merge", "")
	fs.StringVar(&o.Head, "head", "", "")
	fs.BoolVar(&o.Admin, "admin", false, "")
	fs.StringVar(&o.SyncBranch, "sync-branch", "", "")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, prUsage)
			return 0
		}
		return fail("%v (see: agentflow pr --help)", err)
	}
	if len(pos) != 1 {
		return fail("want exactly one PR number, got %d", len(pos))
	}
	n, err := strconv.Atoi(strings.TrimPrefix(pos[0], "#"))
	if err != nil || n <= 0 {
		return fail("PR number %q is not a positive integer", pos[0])
	}
	o.PR = n
	switch o.Method {
	case "merge", "squash", "rebase":
	default:
		return fail("--method %q is not merge, squash or rebase", o.Method)
	}
	if o.Head != "" && !fullSHARe.MatchString(o.Head) {
		return fail("--head %q is not a full 40-character commit sha", o.Head)
	}
	if o.SyncBranch != "" && (!pr.BranchRe.MatchString(o.SyncBranch) || strings.Contains(o.SyncBranch, "..")) {
		return fail("--sync-branch %q is not a plain branch name", o.SyncBranch)
	}
	bin := os.Getenv("AGENTFLOW_GH")
	if bin == "" {
		if bin, err = exec.LookPath("gh"); err != nil {
			return fail("gh not found: put it on PATH or set AGENTFLOW_GH")
		}
	}
	o.GH = pr.GHCLI(bin, 60*time.Second)
	if o.Repo == "" {
		// Never assume a repo: merging the same number in the wrong repo is
		// not undoable.
		out, err := o.GH(ctx, "repo", "view", "--json", "nameWithOwner")
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(stderr, "agentflow pr merge: interrupted")
				return prMergeExitCodes[pr.MergeInterrupted]
			}
			return fail("no --repo and the current directory has no GitHub repo: %v", err)
		}
		var rv struct {
			NameWithOwner string `json:"nameWithOwner"`
		}
		if json.Unmarshal(out, &rv) != nil || rv.NameWithOwner == "" {
			return fail("no --repo and gh repo view gave no name")
		}
		o.Repo = rv.NameWithOwner
	}
	owner, name, _ := strings.Cut(o.Repo, "/")
	if !repoRe.MatchString(o.Repo) || owner == "." || owner == ".." || name == "." || name == ".." {
		return fail("--repo %q is not OWNER/NAME", o.Repo)
	}

	res := pr.Merge(ctx, o)
	out, _ := json.MarshalIndent(res, "", "  ")
	_, _ = stdout.Write(append(out, '\n'))
	code, ok := prMergeExitCodes[res.Status]
	if !ok {
		fmt.Fprintf(stderr, "agentflow pr merge: unknown result status %q\n", res.Status)
		return 1
	}
	if code != 0 {
		msg := res.Next
		switch {
		case res.Error != "":
			msg = res.Error
		case len(res.Reasons) > 0:
			msg = strings.Join(res.Reasons, "; ")
		case res.Sync != nil && res.Sync.Error != "":
			msg = res.Sync.Error
		}
		fmt.Fprintf(stderr, "agentflow pr merge: %s: %s\n", res.Status, msg)
	}
	return code
}
