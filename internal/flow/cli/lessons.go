package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/lessons"
)

const lessonsUsage = `agentflow lessons brief --topics T1,T2 [--repo NAME] [--top 8] [--fp-top 8]
agentflow lessons bump [--used IDS] [--misled IDS] [--fp-seen IDS]
agentflow lessons retire [--days 90] [--today YYYY-MM-DD] [--principles FILE] [--apply]
agentflow lessons stats [--days 90] [--principles FILE]

Maintains a folder of code-review lessons kept as Markdown (one file per
topic, plus Inbox.md, Archive.md and "Reviewer False Positives.md"; the
format is described in the lessons package doc).

brief    prints the "learned checks" section for a review brief: the top
         lessons for the topics (ranked by Used − 2×Misled, then repo, then
         recency) and the most-seen reviewer false positives. Markdown on
         stdout. codex and claude add it themselves with --lessons.
bump     raises Used / Misled on lessons and Seen on false positives, all or
         nothing. IDS is comma-separated; the flag may repeat, and an id
         listed twice is raised twice.
retire   lists topic-file lessons with Used 0 and no activity for --days, or
         with Misled > Used; --apply moves them to Archive.md. Lessons the
         principles file links (#^lN) are kept.
stats    counts lessons, archived lessons, retirement candidates and false
         positives (JSON).

  --lessons-dir DIR   the lessons folder (default: $AGENTFLOW_LESSONS_DIR)
  --principles FILE   default: "Code Review Principles.md" next to the folder

Exit codes: 0 ok · 1 usage/precondition · 2 an id was not found (bump; nothing written)
`

// lessonsExitCodes: 0 ok, 1 usage or precondition, 2 bump id not found.
var lessonsExitCodes = struct{ ok, usage, notFound int }{0, 1, 2}

// lessonsDir resolves the lessons folder: flag > AGENTFLOW_LESSONS_DIR. There
// is no default path: a guessed folder would silently brief from nothing.
func lessonsDir(flagVal string) (string, error) {
	d := flagVal
	if d == "" {
		d = os.Getenv("AGENTFLOW_LESSONS_DIR")
	}
	if d == "" {
		return "", errors.New("no lessons folder: pass --lessons-dir or set AGENTFLOW_LESSONS_DIR")
	}
	if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("lessons folder %s is not a directory", d)
	}
	return d, nil
}

func defaultPrinciples(dir string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(dir)), lessons.PrinciplesFile)
}

// idList is a repeatable, comma-separated id flag.
type idList []string

func (p *idList) String() string { return strings.Join(*p, ",") }
func (p *idList) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			*p = append(*p, s)
		}
	}
	return nil
}

func (p idList) increments() lessons.Increments {
	if len(p) == 0 {
		return nil
	}
	m := lessons.Increments{}
	for _, id := range p {
		m[id]++
	}
	return m
}

func splitTopics(s string) []string {
	var out []string
	for _, t := range strings.Split(s, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func runLessons(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, lessonsUsage)
		return lessonsExitCodes.usage
	}
	sub := args[0]
	if sub == "help" || sub == "--help" || sub == "-h" {
		fmt.Fprint(stdout, lessonsUsage)
		return lessonsExitCodes.ok
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow lessons "+sub+": "+format+"\n", a...)
		return lessonsExitCodes.usage
	}
	fs := flag.NewFlagSet("agentflow lessons "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		dirFlag, topics, repo, principles, today string
		top, fpTop, days                         int
		apply                                    bool
		used, misled, fpSeen                     idList
	)
	fs.StringVar(&dirFlag, "lessons-dir", "", "")
	switch sub {
	case "brief":
		fs.StringVar(&topics, "topics", "", "")
		fs.StringVar(&repo, "repo", "", "")
		fs.IntVar(&top, "top", 8, "")
		fs.IntVar(&fpTop, "fp-top", 8, "")
	case "bump":
		fs.Var(&used, "used", "")
		fs.Var(&misled, "misled", "")
		fs.Var(&fpSeen, "fp-seen", "")
	case "retire", "stats":
		fs.IntVar(&days, "days", 90, "")
		fs.StringVar(&principles, "principles", "", "")
		if sub == "retire" {
			fs.StringVar(&today, "today", "", "")
			fs.BoolVar(&apply, "apply", false, "")
		}
	default:
		fmt.Fprintf(stderr, "agentflow lessons: unknown subcommand %q\n\n%s", sub, lessonsUsage)
		return lessonsExitCodes.usage
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, lessonsUsage)
			return lessonsExitCodes.ok
		}
		return fail("%v (see: agentflow lessons --help)", err)
	}
	if fs.NArg() > 0 {
		return fail("unexpected argument %q", fs.Arg(0))
	}
	dir, err := lessonsDir(dirFlag)
	if err != nil {
		return fail("%v", err)
	}
	if principles == "" {
		principles = defaultPrinciples(dir)
	}
	day := time.Now()
	if today != "" {
		if day, err = time.Parse("2006-01-02", today); err != nil {
			return fail("--today must be YYYY-MM-DD")
		}
	}
	if (sub == "retire" || sub == "stats") && days <= 0 {
		return fail("--days must be positive")
	}
	writeJSON := func(v any) int {
		out, _ := json.MarshalIndent(v, "", "  ")
		_, _ = stdout.Write(append(out, '\n'))
		return lessonsExitCodes.ok
	}

	switch sub {
	case "brief":
		if topics == "" {
			return fail("--topics is required")
		}
		if top < 0 || fpTop < 0 {
			return fail("--top and --fp-top must not be negative")
		}
		v, err := lessons.Load(dir)
		if err != nil {
			return fail("%v", err)
		}
		res, err := v.Brief(splitTopics(topics), repo, top, fpTop)
		if err != nil {
			return fail("%v", err)
		}
		fmt.Fprint(stdout, res.Markdown)
		return lessonsExitCodes.ok
	case "bump":
		req := lessons.BumpRequest{Used: used.increments(), Misled: misled.increments(), Seen: fpSeen.increments()}
		changes, err := lessons.Bump(dir, req)
		var nf *lessons.NotFoundError
		if errors.As(err, &nf) {
			fmt.Fprintf(stderr, "agentflow lessons bump: %v\n", err)
			return lessonsExitCodes.notFound
		}
		if err != nil {
			return fail("%v", err)
		}
		return writeJSON(map[string]any{"changes": changes})
	case "retire":
		var cands []lessons.Candidate
		if apply {
			cands, err = lessons.Retire(dir, principles, days, day)
		} else {
			cands, err = lessons.RetireCandidates(dir, principles, days, day)
		}
		if err != nil {
			return fail("%v", err)
		}
		if cands == nil {
			cands = []lessons.Candidate{}
		}
		return writeJSON(map[string]any{"applied": apply, "candidates": cands})
	default: // stats
		s, err := lessons.Summarize(dir, principles, days, day)
		if err != nil {
			return fail("%v", err)
		}
		return writeJSON(s)
	}
}

// lessonsSection renders the brief section for runAgent's --lessons. The repo
// defaults to the name of dir's origin remote; with no remote it is empty and
// ranking ignores repo.
func lessonsSection(dir, dirFlag, topics, repo string) (string, error) {
	d, err := lessonsDir(dirFlag)
	if err != nil {
		return "", err
	}
	if repo == "" {
		repo = originRepoName(dir)
	}
	v, err := lessons.Load(d)
	if err != nil {
		return "", err
	}
	res, err := v.Brief(splitTopics(topics), repo, 8, 8)
	if err != nil {
		return "", err
	}
	return res.Markdown, nil
}

// originRepoName returns the last path element of dir's origin URL without
// ".git" (".../org/name.git" and "git@host:org/name" both give "name"), or "".
func originRepoName(dir string) string {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	u := strings.TrimSuffix(strings.TrimSpace(string(out)), "/")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	return strings.TrimSuffix(u, ".git")
}
