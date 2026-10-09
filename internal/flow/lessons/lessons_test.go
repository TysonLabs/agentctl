package lessons

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const fxConcurrency = `# Concurrency

Lessons about concurrency.

### Hold the lock across the check and the write
#cr/concurrency · alpha · 2026-06-01 ^l1

- **What went wrong:** the check ran outside the lock.
- **Avoid by:** take the lock before the check.
- **Used:** 3

### Old and never used
#cr/concurrency #cr/database · beta · 2026-01-10 ^l2

- **Avoid by:** old habit.
- **Used:** 0

### Old but seen again recently
#cr/concurrency · beta · 2026-01-10 ^l3

- **Avoid by:** revived habit.
- **Also seen:** 2026-06-20, beta (#9): it happened again.
- **Used:** 0

### Misleads more than it helps
#cr/concurrency · alpha · 2026-06-02 ^l4

- **Avoid by:** a bad rule.
- **Misled:** 2
- **Used:** 1

### Linked from principles, old, unused
#cr/concurrency · gamma · 2026-01-01 ^l5

- **Avoid by:** linked habit.
- **Used:** 0
`

const fxMedia = `# Media

### Same score, repo match wins
#cr/media · alpha · 2026-05-01 ^l6

- **Avoid by:** media habit A.
- **Used:** 1

### Same score, newer but other repo
#cr/media · beta · 2026-06-30 ^l7

- **Avoid by:** media habit B.
- **Used:** 1
`

const fxInbox = `# Inbox

Next free id: l10

## 2026-06-30, alpha (#1)

### Fresh inbox lesson
#cr/concurrency · alpha · 2026-06-30 ^l8

- **Avoid by:** fresh habit.
- **Used:** 0
- **Also seen:** 2026-07-01, alpha (#2): again.
`

const fxArchive = `# Archived Lessons

### Archived lesson
#cr/concurrency · alpha · 2026-01-01 ^l9

- **Avoid by:** archived habit.
- **Used:** 9
- **Retired:** 2026-05-01 from Concurrency (never used)
`

const fxFP = "# Reviewer False Positives\n\nFormat:\n\n```\n### <mistake>\n#fp/<topic> · <repos> · <date> · <reviewer> ^fp<id>\n- **Seen:** <n>\n```\n\n" +
	`### Reports an impossible race
#fp/concurrency · alpha · 2026-06-01 · codex ^fp1
- **Claim:** a race.
- **Why it's wrong:** the order makes it impossible.
- **Tell:** write the step order.
- **Seen:** 2

### Cites a stale rule
#fp/process · beta · 2026-06-01 · coderabbit ^fp2
- **Why it's wrong:** stale context.
- **Tell:** check the head.
- **Seen:** 5

### Media only
#fp/media · beta · 2026-06-01 · codex ^fp3
- **Why it's wrong:** never emitted.
- **Tell:** find the producer.
- **Seen:** 9
`

const fxPrinciples = "# Principles\n\n- [[Code Review Lessons/Concurrency#^l5|Linked]]\n"

// vault writes the fixture and returns the lessons dir and principles path.
func vault(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "Lessons")
	files := map[string]string{
		"Concurrency.md": fxConcurrency, "Media.md": fxMedia, InboxFile: fxInbox,
		ArchiveFile: fxArchive, FPFile: fxFP,
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(root, PrinciplesFile)
	if err := os.WriteFile(p, []byte(fxPrinciples), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, p
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		out[e.Name()] = read(t, filepath.Join(dir, e.Name()))
	}
	return out
}

func TestSplitBlocksRoundTrip(t *testing.T) {
	for _, in := range []string{"", "no headings\n", "### first\nbody", "head\n\n### a\nx\n\n### b\ny", fxConcurrency, fxFP, "a\n####not\n### b\n"} {
		h, blocks := splitBlocks(in)
		if got := h + strings.Join(blocks, ""); got != in {
			t.Errorf("round trip changed input %q -> %q", in, got)
		}
		for _, b := range blocks {
			if !strings.HasPrefix(b, "### ") {
				t.Errorf("block %q does not start with ###", b)
			}
		}
	}
}

func TestLoad(t *testing.T) {
	dir, _ := vault(t)
	v, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Lessons) != 9 {
		t.Fatalf("lessons = %d, want 9", len(v.Lessons))
	}
	if len(v.FalsePositives) != 3 {
		t.Errorf("false positives = %d, want 3 (the format example has no id)", len(v.FalsePositives))
	}
	by := map[string]Lesson{}
	for _, l := range v.Lessons {
		by[l.ID] = l
	}
	if l := by["l3"]; l.Last != "2026-06-20" || l.Date != "2026-01-10" {
		t.Errorf("l3 dates = %q/%q, want Also seen to extend last activity", l.Date, l.Last)
	}
	if l := by["l4"]; l.Misled != 2 || l.Used != 1 || l.Repo != "alpha" {
		t.Errorf("l4 = %+v", l)
	}
	if l := by["l2"]; strings.Join(l.Topics, ",") != "concurrency,database" {
		t.Errorf("l2 topics = %v", l.Topics)
	}
	if by["l8"].File != InboxFile || by["l9"].File != ArchiveFile {
		t.Errorf("files: l8=%s l9=%s", by["l8"].File, by["l9"].File)
	}
	if got := strings.Join(v.Topics(), ","); got != "concurrency,database,media" {
		t.Errorf("topics = %s (archived-only topics must not count)", got)
	}
}

func TestLoadFailsClosed(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing folder must be an error")
	}
	empty := t.TempDir()
	os.WriteFile(filepath.Join(empty, "Notes.md"), []byte("# nothing here\n"), 0o644)
	if _, err := Load(empty); err == nil || !strings.Contains(err.Error(), "no lessons") {
		t.Errorf("folder without lessons: err = %v", err)
	}
}

func TestBriefRanking(t *testing.T) {
	dir, _ := vault(t)
	v, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := v.Brief([]string{"concurrency", "media"}, "alpha", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	// l1 (score 3) first; l6 before l7 (same score, repo match beats recency);
	// l8 (inbox) included; l4 (1-4<0) and l9 (archived) left out.
	got := strings.Join(res.Lessons, ",")
	want := "l1,l6,l7,l8,l3,l2,l5"
	if got != want {
		t.Errorf("order = %s, want %s", got, want)
	}
	if strings.Join(res.FalsePositives, ",") != "fp3,fp2,fp1" {
		t.Errorf("fps = %v", res.FalsePositives)
	}
	for _, s := range []string{"## Learned checks", "[learned <id>]", "- **l1**: Hold the lock across the check and the write. Check: take the lock before the check.", "## Known false positives", "Tell: write the step order."} {
		if !strings.Contains(res.Markdown, s) {
			t.Errorf("markdown missing %q:\n%s", s, res.Markdown)
		}
	}
	res, _ = v.Brief([]string{"database"}, "", 10, 10)
	if strings.Join(res.Lessons, ",") != "l2" || strings.Join(res.FalsePositives, ",") != "fp2" {
		t.Errorf("database: lessons %v fps %v (process FPs always apply)", res.Lessons, res.FalsePositives)
	}
	res, _ = v.Brief([]string{"concurrency"}, "", 1, 0)
	if len(res.Lessons) != 1 || len(res.FalsePositives) != 0 || strings.Contains(res.Markdown, "Known false positives") {
		t.Errorf("caps not applied: %+v", res)
	}
}

func TestBriefUnknownTopic(t *testing.T) {
	dir, _ := vault(t)
	v, _ := Load(dir)
	_, err := v.Brief([]string{"concurrency", "nope"}, "", 8, 8)
	var ut *UnknownTopicError
	if !errors.As(err, &ut) || strings.Join(ut.Unknown, ",") != "nope" || !strings.Contains(err.Error(), "media") {
		t.Errorf("err = %v", err)
	}
	if _, err := v.Brief([]string{" ", ""}, "", 8, 8); err == nil {
		t.Error("no topics must be an error")
	}
}

func TestClipIsRuneSafe(t *testing.T) {
	if got := clip("ééééé", 3); got != "éé…" {
		t.Errorf("clip = %q", got)
	}
	if got := clip("short", 10); got != "short" {
		t.Errorf("clip = %q", got)
	}
}

func TestBump(t *testing.T) {
	dir, _ := vault(t)
	before := snapshot(t, dir)
	changes, err := Bump(dir, BumpRequest{
		Used:   Increments{"l1": 2, "l8": 1},
		Misled: Increments{"l6": 1, "l4": 1},
		Seen:   Increments{"fp1": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 5 {
		t.Errorf("changes = %+v", changes)
	}
	conc := read(t, filepath.Join(dir, "Concurrency.md"))
	if !strings.Contains(conc, "take the lock before the check.\n- **Used:** 5\n") {
		t.Errorf("l1 Used not 5:\n%s", conc)
	}
	if !strings.Contains(conc, "- **Misled:** 3\n- **Used:** 1\n") {
		t.Errorf("l4 Misled not 3:\n%s", conc)
	}
	if got := read(t, filepath.Join(dir, "Media.md")); !strings.Contains(got, "media habit A.\n- **Misled:** 1\n- **Used:** 1\n") {
		t.Errorf("Misled not inserted before Used:\n%s", got)
	}
	if got := read(t, filepath.Join(dir, InboxFile)); !strings.Contains(got, "- **Used:** 1\n- **Also seen:**") {
		t.Errorf("inbox lesson with Also seen after Used:\n%s", got)
	}
	if got := read(t, filepath.Join(dir, FPFile)); !strings.Contains(got, "- **Seen:** 3\n") {
		t.Errorf("fp1 Seen not 3:\n%s", got)
	}
	// Only the counter lines changed: undo them and every file is byte-identical.
	norm := regexp.MustCompile(`(?m)^- \*\*(Used|Misled|Seen):\*\* \d+\n`)
	after := snapshot(t, dir)
	for f, b := range before {
		if norm.ReplaceAllString(b, "") != norm.ReplaceAllString(after[f], "") {
			t.Errorf("%s changed outside counter lines", f)
		}
	}
	if _, ok := after[".agentflow-lessons"]; len(after) != len(before) || ok {
		t.Errorf("stray files: %v", after)
	}
}

func TestBumpAllOrNothing(t *testing.T) {
	dir, _ := vault(t)
	before := snapshot(t, dir)
	_, err := Bump(dir, BumpRequest{Used: Increments{"l1": 1, "l404": 1}, Seen: Increments{"fp1": 1}})
	var nf *NotFoundError
	if !errors.As(err, &nf) || strings.Join(nf.IDs, ",") != "l404" {
		t.Fatalf("err = %v", err)
	}
	// A lesson id is not looked up in the false-positive file, nor fp ids elsewhere.
	if _, err := Bump(dir, BumpRequest{Seen: Increments{"l1": 1}}); !errors.As(err, &nf) {
		t.Errorf("lesson id as fp: err = %v", err)
	}
	dup := filepath.Join(dir, "Dup.md")
	os.WriteFile(dup, []byte("### Copy\n#cr/media · x · 2026-01-01 ^l1\n- **Used:** 0\n"), 0o644)
	if _, err := Bump(dir, BumpRequest{Used: Increments{"l1": 1}}); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Errorf("duplicate id: err = %v", err)
	}
	os.Remove(dup)
	if _, err := Bump(dir, BumpRequest{}); err == nil {
		t.Error("empty request must be an error")
	}
	for f, b := range snapshot(t, dir) {
		if before[f] != b {
			t.Errorf("%s was written by a failed bump", f)
		}
	}
}

func TestBumpConcurrent(t *testing.T) {
	dir, _ := vault(t)
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Bump(dir, BumpRequest{Used: Increments{"l1": 1}})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := read(t, filepath.Join(dir, "Concurrency.md")); !strings.Contains(got, "- **Used:** 23\n") {
		t.Errorf("lost updates: want Used 23\n%s", got)
	}
}

func TestRetire(t *testing.T) {
	dir, principles := vault(t)
	today := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	cands, err := RetireCandidates(dir, principles, 90, today)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]Candidate{}
	for _, c := range cands {
		reasons[c.ID] = c
	}
	// l2 old+unused; l4 misled>used; l5 old+unused but linked; l3 revived by
	// Also seen; l8 is in the inbox; l9 is archived already.
	if len(reasons) != 3 || !reasons["l5"].Linked || reasons["l2"].Linked || !strings.Contains(reasons["l4"].Reason, "misled") {
		t.Fatalf("candidates = %+v", cands)
	}
	before := snapshot(t, dir)
	moved, err := Retire(dir, principles, 90, today)
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 2 {
		t.Fatalf("moved = %+v", moved)
	}
	conc := read(t, filepath.Join(dir, "Concurrency.md"))
	for _, gone := range []string{"^l2", "^l4"} {
		if strings.Contains(conc, gone) {
			t.Errorf("%s still in Concurrency.md", gone)
		}
	}
	arch := read(t, filepath.Join(dir, ArchiveFile))
	if !strings.HasPrefix(arch, "# Archived Lessons\n\n### Archived lesson") || !strings.Contains(arch, "- **Retired:** 2026-07-01 from Concurrency (misled 2 > used 1)\n") {
		t.Errorf("archive:\n%s", arch)
	}
	// No lesson is lost or glued onto another line, and the topic file still ends cleanly.
	count := func(m map[string]string) int {
		n := 0
		for _, c := range m {
			n += strings.Count(c, "\n### ") + map[bool]int{true: 1}[strings.HasPrefix(c, "### ")]
		}
		return n
	}
	after := snapshot(t, dir)
	if count(before) != count(after) {
		t.Errorf("headings %d -> %d", count(before), count(after))
	}
	for f, c := range after {
		if regexp.MustCompile(`\S### `).MatchString(c) {
			t.Errorf("%s has a glued heading", f)
		}
		if !strings.HasSuffix(c, "\n") || strings.HasSuffix(c, "\n\n\n") {
			t.Errorf("%s ends badly: %q", f, c[len(c)-10:])
		}
	}
	if !strings.Contains(conc, "- **Used:** 0\n\n### Linked from principles") {
		t.Errorf("blank line between kept lessons lost:\n%s", conc)
	}
	// A second run finds nothing more and leaves files alone.
	if moved, err := Retire(dir, principles, 90, today); err != nil || len(moved) != 0 {
		t.Errorf("second run: %v %v", moved, err)
	}
}

func TestRetirePreservesBytesOutsideMovedBlock(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Lessons")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	topic := "# Exact header\t\n\n" +
		"### Retire me\n#cr/concurrency · alpha · 2026-01-01 ^l10\n\n- **Avoid by:** old.\n- **Used:** 0\n\n\n" +
		"### Keep me\n#cr/concurrency · alpha · 2026-06-01 ^l11\n\n- **Avoid by:** current.\n- **Used:** 2\n\n\n"
	archiveBefore := "# Existing archive\n\n\n"
	if err := os.WriteFile(filepath.Join(dir, "Concurrency.md"), []byte(topic), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ArchiveFile), []byte(archiveBefore), 0o600); err != nil {
		t.Fatal(err)
	}
	header, blocks := splitBlocks(topic)
	if len(blocks) != 2 {
		t.Fatalf("fixture split into %d blocks", len(blocks))
	}
	today := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if _, err := Retire(dir, filepath.Join(root, PrinciplesFile), 90, today); err != nil {
		t.Fatal(err)
	}
	if got, want := read(t, filepath.Join(dir, "Concurrency.md")), header+blocks[1]; got != want {
		t.Errorf("topic bytes outside the moved block changed\ngot:  %q\nwant: %q", got, want)
	}
	archiveAfter := read(t, filepath.Join(dir, ArchiveFile))
	if !strings.HasPrefix(archiveAfter, archiveBefore) {
		t.Errorf("existing archive bytes changed\nbefore: %q\nafter:  %q", archiveBefore, archiveAfter)
	}
	retiredLine := "- **Retired:** 2026-07-01 from Concurrency (never used, last activity 2026-01-01)\n"
	withoutRetiredLine := strings.Replace(archiveAfter, retiredLine, "", 1)
	if !strings.Contains(withoutRetiredLine, blocks[0]) {
		t.Errorf("moved block bytes changed\nblock:   %q\narchive: %q", blocks[0], archiveAfter)
	}
	if fi, err := os.Stat(filepath.Join(dir, "Concurrency.md")); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o640 {
		t.Errorf("topic mode = %v; want 0640", fi.Mode().Perm())
	}
	if fi, err := os.Stat(filepath.Join(dir, ArchiveFile)); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("archive mode = %v; want 0600", fi.Mode().Perm())
	}
}

func TestRetireIncludesExactAgeBoundary(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Lessons")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lesson := "### Exactly ninety days old\n#cr/concurrency · alpha · 2026-04-02 ^l10\n\n- **Avoid by:** retire on the boundary.\n- **Used:** 0\n"
	if err := os.WriteFile(filepath.Join(dir, "Concurrency.md"), []byte(lesson), 0o644); err != nil {
		t.Fatal(err)
	}
	today := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	cands, err := RetireCandidates(dir, filepath.Join(root, PrinciplesFile), 90, today)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].ID != "l10" {
		t.Fatalf("candidates = %+v, want l10 at the 90-day boundary", cands)
	}
}

func TestRetireCreatesArchive(t *testing.T) {
	dir, principles := vault(t)
	os.Remove(filepath.Join(dir, ArchiveFile))
	if _, err := Retire(dir, principles, 90, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	arch := read(t, filepath.Join(dir, ArchiveFile))
	if !strings.HasPrefix(arch, archiveHeader+"\n### ") || strings.Count(arch, "\n### ") != 2 {
		t.Errorf("new archive:\n%s", arch)
	}
}

func TestSummarize(t *testing.T) {
	dir, principles := vault(t)
	s, err := Summarize(dir, principles, 90, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := Stats{Lessons: 8, Inbox: 1, Archived: 1, UsedZero: 4, MisledAny: 1, RetireCandidates: 2, RetireLinked: 1, FalsePositives: 3}
	if s != want {
		t.Errorf("stats = %+v, want %+v", s, want)
	}
}
