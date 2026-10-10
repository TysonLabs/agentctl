package cli

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/TysonLabs/agentctl/internal/flow/agent"
	"github.com/TysonLabs/agentctl/internal/flow/coderabbit"
	"github.com/TysonLabs/agentctl/internal/flow/lessons"
	"github.com/TysonLabs/agentctl/internal/flow/pr"
	"github.com/TysonLabs/agentctl/internal/flow/render"
	"github.com/TysonLabs/agentctl/internal/flow/ship"
	"github.com/TysonLabs/agentctl/internal/flow/worktree"
)

// outputFormat is the value of --format. JSON is the default and the stable
// API; text is a short summary for people and agents reading a terminal.
// The format never changes the exit code, and commands that save their JSON
// (result.json under --out) save it in both formats.
type outputFormat string

const (
	formatJSON outputFormat = "json"
	formatText outputFormat = "text"
)

func (f *outputFormat) String() string { return string(*f) }

func (f *outputFormat) Set(v string) error {
	switch outputFormat(v) {
	case formatJSON, formatText:
		*f = outputFormat(v)
		return nil
	}
	return fmt.Errorf("must be json or text")
}

// formatFlag registers --format on fs.
func formatFlag(fs *flag.FlagSet) *outputFormat {
	f := formatJSON
	fs.Var(&f, "format", "")
	return &f
}

// emit writes jsonOut unchanged, or the text rendering for --format text.
func emit(w io.Writer, f outputFormat, jsonOut []byte, text func() string) {
	if f == formatText {
		_, _ = io.WriteString(w, text())
		return
	}
	_, _ = w.Write(jsonOut)
}

// Per-command text renderers. Each takes the result and the exit code the
// command returns, and prints the header, the key fields, then the lists.

func agentText(name string, res agent.Result, code int, outDir string) string {
	t := render.New(name, string(res.Status), code)
	t.Field("final", res.Final)
	if res.Exit != nil {
		t.Field(name+"_exit", *res.Exit)
	} else {
		t.Field(name+"_exit", "none (killed by agentflow)")
	}
	t.Field("mode", res.Mode).Field("sandbox", res.Sandbox).Field("dir", res.Dir)
	t.Field("duration_s", res.DurationS).Field("thread_id", res.ThreadID)
	if res.Usage != nil {
		t.Field("tokens", fmt.Sprintf("input %d (cached %d), output %d", res.Usage.InputTokens, res.Usage.CachedInputTokens, res.Usage.OutputTokens))
	}
	t.Field("cost_usd", res.CostUSD).Field("error", res.Error)
	t.Field("stderr", res.Stderr).Field("json", resultPath(outDir))
	return t.String()
}

func coderabbitText(res coderabbit.Result, code int) string {
	t := render.New("coderabbit", string(res.Status), code)
	t.Field("base", res.Base).Field("base_commit", res.BaseCommit).Field("review_type", res.ReviewType)
	t.Field("findings", findingsSummary(res))
	t.Field("reviewed_files", len(res.ReviewedFiles)).Field("duration_s", res.DurationS)
	t.Field("error", res.Error).Field("stderr", res.Stderr).Field("json", resultPath(res.Out))
	for _, f := range res.Findings {
		sev := f.Severity
		if sev == "" {
			sev = "unknown"
		}
		t.Item("[%s] %s", sev, f.File)
		body, _ := render.Cap(render.Block(f.Text), pr.MaxBodyRunes)
		t.Indent("    ", body)
	}
	return t.String()
}

// findingsSummary is "3 (major 1, minor 2)": the count, then the severity
// counts in name order so the line is stable.
func findingsSummary(res coderabbit.Result) string {
	s := strconv.Itoa(len(res.Findings))
	if len(res.Severities) == 0 {
		return s
	}
	keys := make([]string, 0, len(res.Severities))
	for k := range res.Severities {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, res.Severities[k])
	}
	return s + " (" + strings.Join(parts, ", ") + ")"
}

func verifyText(res ship.Result, code int) string {
	t := render.New("ship verify", string(res.Status), code)
	t.Field("service", res.Service).Field("expected", res.Expected).Field("running", res.Running)
	t.Field("match", res.Match).Field("started_at", res.StartedAt)
	t.Field("attempts", res.Attempts).Field("duration_s", res.DurationS).Field("checked_at", res.CheckedAt)
	t.Field("error", res.Error)
	return t.String()
}

func announceText(res ship.AnnounceResult, code int) string {
	t := render.New("ship announce", string(res.Status), code)
	t.Field("service", res.Service).Field("sha", res.SHA).Field("channel", res.Channel)
	t.Field("posted_at", res.PostedAt).Field("error", res.Error)
	if len(res.Payload) > 0 {
		t.Field("payload", "(dry run; the Slack payload follows)")
		t.Indent("    ", string(res.Payload))
	}
	return t.String()
}

func waitText(res pr.Result, code int) string {
	t := render.New("pr wait", string(res.Status), code)
	t.Field("repo", res.Repo).Field("pr", res.PR).Field("head", res.Head).Field("reviewed", res.Reviewed)
	t.Field("open_threads", len(res.OpenThreads)).Field("threads_complete", res.ThreadsComplete)
	t.Field("next", res.Next).Field("error", res.Error)
	t.Field("attempts", res.Attempts).Field("duration_s", res.DurationS).Field("checked_at", res.CheckedAt)
	for _, th := range res.OpenThreads {
		loc := th.Path
		if th.Line > 0 {
			loc += ":" + strconv.Itoa(th.Line)
		}
		t.Item("%s", strings.TrimSpace(th.ID+" "+loc+" "+th.Excerpt))
		if th.Replies != nil {
			t.Field("    replies", *th.Replies)
		}
		t.Field("    url", th.URL)
		if th.Body != nil {
			t.Indent("    ", *th.Body)
		}
	}
	return t.String()
}

func threadText(res pr.ThreadResult, code int) string {
	t := render.New("pr thread", string(res.Status), code)
	t.Field("thread", res.Thread).Field("repo", res.Repo)
	if res.PR > 0 {
		t.Field("pr", res.PR)
	}
	loc := res.Path
	if loc != "" && res.Line > 0 {
		loc += ":" + strconv.Itoa(res.Line)
	}
	t.Field("path", loc)
	if res.Status == pr.ThreadOK {
		t.Field("resolved", res.Resolved).Field("outdated", res.Outdated)
		t.Field("comments", len(res.Comments)).Field("comments_complete", res.Complete)
	}
	t.Field("error", res.Error)
	for _, c := range res.Comments {
		t.Item("%s %s", c.Author, c.CreatedAt)
		t.Field("    url", c.URL)
		t.Indent("    ", c.Body)
	}
	return t.String()
}

func replyText(res pr.ReplyResult, code int) string {
	t := render.New("pr reply", string(res.Status), code)
	t.Field("thread", res.Thread).Field("repo", res.Repo)
	if res.PR > 0 {
		t.Field("pr", res.PR)
	}
	t.Field("replied", res.Replied).Field("resolved", res.Resolved).Field("reply_url", res.ReplyURL)
	t.Field("body", res.Body).Field("next", res.Next).Field("error", res.Error)
	return t.String()
}

func mergeText(res pr.MergeResult, code int) string {
	t := render.New("pr merge", string(res.Status), code)
	t.Field("repo", res.Repo).Field("pr", res.PR).Field("head", res.Head).Field("base", res.Base)
	t.Field("method", res.Method).Field("merge_sha", res.MergeSHA)
	if res.AutoMerge {
		t.Field("auto_merge", true)
	}
	if s := res.Sync; s != nil {
		line := s.Branch + " " + s.Status
		if s.SHA != "" {
			line += " " + s.SHA
		}
		if s.Error != "" {
			line += ": " + s.Error
		}
		t.Field("sync", line)
	}
	t.Field("next", res.Next).Field("error", res.Error)
	for _, r := range res.Reasons {
		t.Item("refused: %s", r)
	}
	return t.String()
}

// worktreeEntry is the part of a worktree done/sweep entry the text shows.
type worktreeEntry struct {
	worktree.Check
	Removed *worktree.Removed
	Error   string
}

func worktreeEntryState(e worktreeEntry) string {
	switch {
	case e.Removed != nil:
		return "removed"
	case e.Error != "":
		return "error"
	case e.OK:
		return "removable"
	}
	return "refused"
}

func worktreeEntryText(t *render.Text, e worktreeEntry) {
	t.Item("[%s] %s", worktreeEntryState(e), e.Path)
	t.Field("    branch", e.Branch).Field("    merged_via", e.MergedVia).Field("    keep_branch", e.KeepBranch)
	if e.Removed != nil {
		t.Field("    freed_bytes", e.Removed.FreedBytes)
		t.Field("    branch_deleted", e.Removed.BranchDeleted).Field("    remote_deleted", e.Removed.RemoteDeleted)
		t.Field("    note", e.Removed.Note)
	}
	t.Field("    error", e.Error)
	for _, r := range e.Refusals {
		t.Field("    refused", r)
	}
}

func worktreeDoneText(e worktreeEntry, into string, dryRun bool, code int) string {
	status := worktreeEntryState(e)
	if dryRun && e.OK {
		status = "removable (dry run)"
	}
	t := render.New("worktree done", status, code)
	t.Field("into", into)
	worktreeEntryText(t, e)
	return t.String()
}

func worktreeSweepText(entries []worktreeEntry, into string, removable, removed, pruned int, freed int64, sweepErr string, applied bool, code int) string {
	status := "listed"
	if applied {
		status = "swept"
	}
	if code != 0 {
		status = "error"
	}
	t := render.New("worktree sweep", status, code)
	t.Field("into", into).Field("removable", removable).Field("removed", removed)
	t.Field("pruned_stale_records", pruned).Field("freed_bytes", freed).Field("error", sweepErr)
	for _, e := range entries {
		worktreeEntryText(t, e)
	}
	return t.String()
}

func bumpText(changes []lessons.Change) string {
	t := render.New("lessons bump", "ok", lessonsExitCodes.ok)
	t.Field("changes", len(changes))
	for _, c := range changes {
		t.Item("%s %s %d -> %d (%s)", c.ID, c.Field, c.From, c.To, c.File)
	}
	return t.String()
}

func retireText(cands []lessons.Candidate, applied bool) string {
	t := render.New("lessons retire", "ok", lessonsExitCodes.ok)
	t.Field("applied", applied).Field("candidates", len(cands))
	for _, c := range cands {
		linked := ""
		if c.Linked {
			linked = " [kept: linked from principles]"
		}
		t.Item("%s %s: %s (%s)%s", c.ID, c.Title, c.Reason, c.File, linked)
	}
	return t.String()
}

func statsText(s lessons.Stats) string {
	t := render.New("lessons stats", "ok", lessonsExitCodes.ok)
	t.Field("lessons", s.Lessons).Field("inbox", s.Inbox).Field("archived", s.Archived)
	t.Field("used_zero", s.UsedZero).Field("misled_nonzero", s.MisledAny)
	t.Field("retire_candidates", s.RetireCandidates).Field("retire_kept_linked", s.RetireLinked)
	t.Field("false_positives", s.FalsePositives)
	return t.String()
}

func resultPath(outDir string) string {
	if outDir == "" {
		return ""
	}
	return filepath.Join(outDir, "result.json")
}
