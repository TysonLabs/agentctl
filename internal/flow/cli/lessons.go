package cli

import (
	"bytes"
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
agentflow lessons add --repo R --ref REF --topics T1,T2 --title T --what W --why Y --avoid A
                      [--principle P] [--date YYYY-MM-DD] [--new-topic] [--if-no-duplicate]
                      [--dup-threshold 0.35] [--dry-run]
agentflow lessons seen ID --note TEXT --repo R --ref REF [--date YYYY-MM-DD] [--no-bump] [--revive]
agentflow lessons search TERMS... [--topics T1,T2] [--include-archive] [--limit 10]
agentflow lessons triage [--dup-threshold 0.35] [--apply PLAN.json]
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
add      writes a new lesson to the Inbox: takes the id from its "Next free
         id: lN" line and raises it, and puts the lesson at the top of the
         section "## <date>, <repo> (<ref>)" (created newest first). Topics
         must already be in use (--new-topic allows a new one); --principle
         must be a "## <name>" heading of the principles file. Prints the id
         and candidate duplicates (score >= --dup-threshold, Archive
         included). Check them: if one records the same habit, use seen
         instead. --if-no-duplicate writes nothing and exits 3 when there is
         a candidate; --dry-run writes nothing and prints the block.
seen     adds "- **Also seen:** <date>, <repo> (<ref>): <note>" to a lesson
         in any file and raises its Used by one (--no-bump: not). An
         archived lesson needs --revive: it moves back to the topic file its
         Retired line names, without that line.
search   ranks lessons by word overlap with TERMS (title counts most, then
         Avoid by, then What went wrong and Also seen; rare words count
         more). JSON on stdout. Archive.md only with --include-archive.
triage   lists every Inbox lesson with candidate duplicates and a suggested
         topic file (JSON). --apply PLAN.json applies a JSON list of steps,
         all or nothing:
           {"id":"l12","action":"move","file":"Concurrency.md"}
           {"id":"l13","action":"merge","target":"l4","note":"TEXT"}
         move appends the lesson to the topic file; merge adds an Also seen
         line to the target (note defaults to the lesson's title, ref and
         date to its section and tag line; "ref"/"date" override), adds 1 +
         the lesson's Used to the target's Used, and drops the lesson.
         Sections left empty are removed; the Inbox header stays.
retire   lists topic-file lessons with Used 0 and no activity for --days, or
         with Misled > Used; --apply moves them to Archive.md. Lessons the
         principles file links (#^lN) are kept.
stats    counts lessons, archived lessons, retirement candidates and false
         positives (JSON).

Every command that writes locks the folder, checks everything first, and
then writes each changed file atomically; bytes outside the changed lines
stay as they were.

  --lessons-dir DIR   the lessons folder (default: $AGENTFLOW_LESSONS_DIR)
  --principles FILE   default: "Code Review Principles.md" next to the folder

Exit codes: 0 ok · 1 usage/precondition · 2 an id was not found (nothing
written) · 3 add --if-no-duplicate found a candidate duplicate (nothing written)
`

// lessonsExitCodes: 0 ok, 1 usage or precondition, 2 id not found (bump,
// seen, triage --apply), 3 add --if-no-duplicate found a candidate duplicate.
var lessonsExitCodes = struct{ ok, usage, notFound, duplicate int }{0, 1, 2, 3}

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
		top, fpTop, days, limit                  int
		apply, dryRun, newTopic, ifNoDup         bool
		noBump, revive, inclArchive              bool
		used, misled, fpSeen                     idList
		ref, title, what, why, avoid, principle  string
		date, note, planFile                     string
		dupThreshold                             float64
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
	case "add":
		fs.StringVar(&repo, "repo", "", "")
		fs.StringVar(&ref, "ref", "", "")
		fs.StringVar(&topics, "topics", "", "")
		fs.StringVar(&title, "title", "", "")
		fs.StringVar(&what, "what", "", "")
		fs.StringVar(&why, "why", "", "")
		fs.StringVar(&avoid, "avoid", "", "")
		fs.StringVar(&principle, "principle", "", "")
		fs.StringVar(&principles, "principles", "", "")
		fs.StringVar(&date, "date", "", "")
		fs.BoolVar(&newTopic, "new-topic", false, "")
		fs.BoolVar(&ifNoDup, "if-no-duplicate", false, "")
		fs.Float64Var(&dupThreshold, "dup-threshold", lessons.DefaultDupThreshold, "")
		fs.BoolVar(&dryRun, "dry-run", false, "")
	case "seen":
		fs.StringVar(&note, "note", "", "")
		fs.StringVar(&repo, "repo", "", "")
		fs.StringVar(&ref, "ref", "", "")
		fs.StringVar(&date, "date", "", "")
		fs.BoolVar(&noBump, "no-bump", false, "")
		fs.BoolVar(&revive, "revive", false, "")
	case "search":
		fs.StringVar(&topics, "topics", "", "")
		fs.BoolVar(&inclArchive, "include-archive", false, "")
		fs.IntVar(&limit, "limit", 10, "")
	case "triage":
		fs.Float64Var(&dupThreshold, "dup-threshold", lessons.DefaultDupThreshold, "")
		fs.StringVar(&planFile, "apply", "", "")
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
	// seen and search take positional arguments, which may come before,
	// between or after the flags.
	var pos []string
	for rest := args[1:]; ; {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprint(stdout, lessonsUsage)
				return lessonsExitCodes.ok
			}
			return fail("%v (see: agentflow lessons --help)", err)
		}
		if fs.NArg() == 0 {
			break
		}
		if sub != "seen" && sub != "search" {
			return fail("unexpected argument %q", fs.Arg(0))
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
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
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	if (sub == "retire" || sub == "stats") && days <= 0 {
		return fail("--days must be positive")
	}
	writeJSON := func(v any) int {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false) // file names such as "Tooling & CI.md" stay readable
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
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
	case "add":
		req := lessons.AddRequest{Repo: repo, Ref: ref, Title: title, What: what, Why: why, Avoid: avoid,
			Principle: principle, Date: date, Topics: splitTopics(topics), NewTopic: newTopic,
			DryRun: dryRun, IfNoDuplicate: ifNoDup, DupThreshold: dupThreshold}
		if dupThreshold <= 0 || dupThreshold > 1 {
			return fail("--dup-threshold must be in (0, 1]")
		}
		res, err := lessons.Add(dir, principles, req)
		if errors.Is(err, lessons.ErrDuplicate) {
			writeJSON(res)
			fmt.Fprintf(stderr, "agentflow lessons add: %v\n", err)
			return lessonsExitCodes.duplicate
		}
		if err != nil {
			return fail("%v", err)
		}
		return writeJSON(res)
	case "seen":
		if len(pos) != 1 {
			return fail("want exactly one lesson id, got %d", len(pos))
		}
		res, err := lessons.Seen(dir, lessons.SeenRequest{ID: pos[0], Note: note, Repo: repo, Ref: ref, Date: date, NoBump: noBump, Revive: revive})
		var nf *lessons.NotFoundError
		if errors.As(err, &nf) {
			fmt.Fprintf(stderr, "agentflow lessons seen: %v\n", err)
			return lessonsExitCodes.notFound
		}
		if err != nil {
			return fail("%v", err)
		}
		return writeJSON(res)
	case "search":
		if len(pos) == 0 {
			return fail("give at least one search term")
		}
		if limit < 0 {
			return fail("--limit must not be negative")
		}
		v, err := lessons.Load(dir)
		if err != nil {
			return fail("%v", err)
		}
		q := strings.Join(pos, " ")
		res := v.Search(q, lessons.SearchOptions{Topics: splitTopics(topics), IncludeArchive: inclArchive, Limit: limit})
		if res == nil {
			res = []lessons.Match{}
		}
		return writeJSON(map[string]any{"query": q, "results": res})
	case "triage":
		if dupThreshold <= 0 || dupThreshold > 1 {
			return fail("--dup-threshold must be in (0, 1]")
		}
		if planFile == "" {
			rep, err := lessons.Triage(dir, dupThreshold)
			if err != nil {
				return fail("%v", err)
			}
			return writeJSON(rep)
		}
		raw, err := os.ReadFile(planFile)
		if err != nil {
			return fail("%v", err)
		}
		var plan []lessons.PlanStep
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&plan); err != nil {
			return fail("plan %s: %v", planFile, err)
		}
		if dec.More() {
			return fail("plan %s: trailing data after the JSON list", planFile)
		}
		res, err := lessons.TriageApply(dir, plan)
		var nf *lessons.NotFoundError
		if errors.As(err, &nf) {
			fmt.Fprintf(stderr, "agentflow lessons triage: %v\n", err)
			return lessonsExitCodes.notFound
		}
		if err != nil {
			return fail("%v", err)
		}
		return writeJSON(res)
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
