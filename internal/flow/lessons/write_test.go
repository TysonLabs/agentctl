package lessons

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func baseAdd() AddRequest {
	return AddRequest{
		Repo: "alpha", Ref: "#3", Title: "Write the counter under the folder lock",
		What: "two writers took the same id.", Why: "the read was outside the lock.",
		Avoid: "read and raise the counter while holding the lock.", Date: "2026-07-02",
		Topics: []string{"concurrency"},
	}
}

const addedBlock = "### Write the counter under the folder lock\n" +
	"#cr/concurrency · alpha · 2026-07-02 ^l10\n\n" +
	"- **What went wrong:** two writers took the same id.\n" +
	"- **Why:** the read was outside the lock.\n" +
	"- **Avoid by:** read and raise the counter while holding the lock.\n" +
	"- **Used:** 0\n"

// unchangedExcept fails if any file other than the named ones changed.
func unchangedExcept(t *testing.T, before, after map[string]string, changed ...string) {
	t.Helper()
	skip := map[string]bool{}
	for _, c := range changed {
		skip[c] = true
	}
	for f, b := range before {
		if !skip[f] && after[f] != b {
			t.Errorf("%s changed:\n%s", f, after[f])
		}
	}
	if len(after) != len(before) {
		t.Errorf("file set changed: %d -> %d files", len(before), len(after))
	}
}

func TestAddNewSectionNewestFirst(t *testing.T) {
	dir, principles := vault(t)
	before := snapshot(t, dir)
	res, err := Add(dir, principles, baseAdd())
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "l10" || res.NextFreeID != "l11" || !res.Written || !res.CreatedSection || res.CounterWas != "" {
		t.Errorf("result %+v", res)
	}
	want := strings.Replace(fxInbox, "Next free id: l10", "Next free id: l11", 1)
	want = strings.Replace(want, "## 2026-06-30", "## 2026-07-02, alpha (#3)\n\n"+addedBlock+"\n## 2026-06-30", 1)
	after := snapshot(t, dir)
	if after[InboxFile] != want {
		t.Errorf("inbox:\n%s\nwant:\n%s", after[InboxFile], want)
	}
	unchangedExcept(t, before, after, InboxFile)
	v, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got *Lesson
	for i := range v.Lessons {
		if v.Lessons[i].ID == "l10" {
			got = &v.Lessons[i]
		}
	}
	if got == nil || got.File != InboxFile || got.Repo != "alpha" || got.Date != "2026-07-02" || got.Used != 0 {
		t.Errorf("parsed back: %+v", got)
	}
}

func TestAddExistingSectionGoesOnTop(t *testing.T) {
	dir, principles := vault(t)
	req := baseAdd()
	req.Date, req.Ref = "2026-06-30", "#1"
	res, err := Add(dir, principles, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.CreatedSection {
		t.Error("section exists; must not create one")
	}
	block := strings.Replace(addedBlock, "2026-07-02", "2026-06-30", 1)
	want := strings.Replace(fxInbox, "Next free id: l10", "Next free id: l11", 1)
	want = strings.Replace(want, "### Fresh inbox lesson", block+"\n### Fresh inbox lesson", 1)
	if got := read(t, filepath.Join(dir, InboxFile)); got != want {
		t.Errorf("inbox:\n%s\nwant:\n%s", got, want)
	}
}

func TestAddOldestSectionAppends(t *testing.T) {
	dir, principles := vault(t)
	req := baseAdd()
	req.Date = "2026-01-01"
	req.Principle = "every-path"
	if _, err := Add(dir, principles, req); err == nil || !strings.Contains(err.Error(), "every-path") {
		t.Fatalf("unknown principle: %v", err)
	}
	if err := os.WriteFile(principles, []byte(fxPrinciples+"\n## every-path\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(dir, principles, req); err != nil {
		t.Fatal(err)
	}
	block := strings.Replace(addedBlock, "2026-07-02 ^l10", "2026-01-01 · #principle/every-path ^l10", 1)
	want := strings.Replace(fxInbox, "Next free id: l10", "Next free id: l11", 1) + "\n## 2026-01-01, alpha (#3)\n\n" + block
	if got := read(t, filepath.Join(dir, InboxFile)); got != want {
		t.Errorf("inbox:\n%q\nwant:\n%q", got, want)
	}
}

// Odd bytes around the insertion point (no final newline, trailing spaces,
// CRLF on the counter line) survive.
func TestAddPreservesOddBytes(t *testing.T) {
	dir, principles := vault(t)
	odd := "# Inbox  \n\nNext free id: l10\r\n\n## 2026-06-30, alpha (#1)\n### Fresh inbox lesson\n#cr/concurrency · alpha · 2026-06-30 ^l8\n- **Avoid by:** x \n- **Used:** 0"
	if err := os.WriteFile(filepath.Join(dir, InboxFile), []byte(odd), 0o644); err != nil {
		t.Fatal(err)
	}
	req := baseAdd()
	req.Date, req.Ref = "2026-06-30", "#1"
	if _, err := Add(dir, principles, req); err != nil {
		t.Fatal(err)
	}
	block := strings.Replace(addedBlock, "2026-07-02", "2026-06-30", 1)
	want := strings.Replace(odd, "l10\r\n", "l11\r\n", 1)
	want = strings.Replace(want, "(#1)\n### Fresh", "(#1)\n\n"+block+"\n### Fresh", 1)
	if got := read(t, filepath.Join(dir, InboxFile)); got != want {
		t.Errorf("inbox:\n%q\nwant:\n%q", got, want)
	}
}

func TestAddRejectsBadInputAndWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		edit func(*AddRequest)
		want string
	}{
		{"empty title", func(r *AddRequest) { r.Title = " " }, "--title is required"},
		{"multi-line what", func(r *AddRequest) { r.What = "a\nb" }, "single line"},
		{"multi-line ref", func(r *AddRequest) { r.Ref = "#1\r" }, "single line"},
		{"no topics", func(r *AddRequest) { r.Topics = nil }, "--topics"},
		{"bad topic", func(r *AddRequest) { r.Topics = []string{"Con currency"} }, "lowercase"},
		{"twice topic", func(r *AddRequest) { r.Topics = []string{"concurrency", "concurrency"} }, "twice"},
		{"unknown topic", func(r *AddRequest) { r.Topics = []string{"concurrency", "nope"} }, "--new-topic"},
		{"unknown principle", func(r *AddRequest) { r.Principle = "nope" }, "not a"},
		{"bad date", func(r *AddRequest) { r.Date = "2026-13-01" }, "YYYY-MM-DD"},
		{"repo separator", func(r *AddRequest) { r.Repo = "a · b" }, "--repo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, principles := vault(t)
			before := snapshot(t, dir)
			req := baseAdd()
			c.edit(&req)
			if _, err := Add(dir, principles, req); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
			unchangedExcept(t, before, snapshot(t, dir))
		})
	}
}

func TestAddCounterProblems(t *testing.T) {
	for name, inbox := range map[string]string{
		"no counter":  strings.Replace(fxInbox, "Next free id: l10\n", "", 1),
		"two counter": strings.Replace(fxInbox, "Next free id: l10\n", "Next free id: l10\nNext free id: l12\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			dir, principles := vault(t)
			if err := os.WriteFile(filepath.Join(dir, InboxFile), []byte(inbox), 0o644); err != nil {
				t.Fatal(err)
			}
			before := snapshot(t, dir)
			if _, err := Add(dir, principles, baseAdd()); err == nil || !strings.Contains(err.Error(), "exactly one") {
				t.Fatalf("err %v", err)
			}
			unchangedExcept(t, before, snapshot(t, dir))
		})
	}
	t.Run("no inbox", func(t *testing.T) {
		dir, principles := vault(t)
		os.Remove(filepath.Join(dir, InboxFile))
		if _, err := Add(dir, principles, baseAdd()); err == nil || !strings.Contains(err.Error(), InboxFile) {
			t.Fatalf("err %v", err)
		}
	})
}

// A counter at or below an id already in the folder (a hand-added lesson)
// must not hand out that id again.
func TestAddCounterBehindExistingIDs(t *testing.T) {
	dir, principles := vault(t)
	p := filepath.Join(dir, InboxFile)
	if err := os.WriteFile(p, []byte(strings.Replace(fxInbox, "l10", "l9", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Add(dir, principles, baseAdd())
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != "l10" || res.CounterWas != "l9" || !strings.Contains(read(t, p), "Next free id: l11\n") {
		t.Errorf("result %+v\n%s", res, read(t, p))
	}
}

func TestAddNewTopic(t *testing.T) {
	dir, principles := vault(t)
	req := baseAdd()
	req.Topics = []string{"brand-new"}
	req.NewTopic = true
	if _, err := Add(dir, principles, req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(t, filepath.Join(dir, InboxFile)), "#cr/brand-new · alpha") {
		t.Error("new topic not written")
	}
}

func TestAddDuplicatesDryRunAndRefusal(t *testing.T) {
	dir, principles := vault(t)
	before := snapshot(t, dir)
	req := baseAdd()
	req.Title = "Hold the lock across the check and the write, again"
	req.Avoid = "take the lock before the check."
	req.DryRun = true
	res, err := Add(dir, principles, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Written || len(res.Duplicates) == 0 || res.Duplicates[0].ID != "l1" || !strings.HasPrefix(res.Block, "### Hold the lock") {
		t.Errorf("dry run %+v", res)
	}
	unchangedExcept(t, before, snapshot(t, dir))
	req.DryRun, req.IfNoDuplicate = false, true
	if res, err = Add(dir, principles, req); !errors.Is(err, ErrDuplicate) || res.Written || len(res.Duplicates) == 0 {
		t.Errorf("if-no-duplicate: %v %+v", err, res)
	}
	unchangedExcept(t, before, snapshot(t, dir))
	// A lesson that matches nothing goes through with IfNoDuplicate.
	other := baseAdd()
	other.Title, other.Avoid, other.IfNoDuplicate = "Quartz widgets frobnicate", "zebra", true
	if res, err = Add(dir, principles, other); err != nil || !res.Written || len(res.Duplicates) != 0 {
		t.Errorf("no duplicate: %v %+v", err, res)
	}
}

// Concurrent adds never hand out the same id: each takes the folder lock
// around reading and raising the counter.
func TestAddConcurrent(t *testing.T) {
	dir, principles := vault(t)
	const n = 20
	var wg sync.WaitGroup
	ids := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := baseAdd()
			req.Title = fmt.Sprintf("Concurrent lesson %d", i)
			res, err := Add(dir, principles, req)
			errs <- err
			ids <- res.ID
		}(i)
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Errorf("id %s handed out twice", id)
		}
		seen[id] = true
	}
	inbox := read(t, filepath.Join(dir, InboxFile))
	if !strings.Contains(inbox, fmt.Sprintf("Next free id: l%d\n", 10+n)) {
		t.Errorf("counter not at l%d:\n%s", 10+n, inbox)
	}
	v, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, l := range v.Lessons {
		count[l.ID]++
	}
	for id, c := range count {
		if c != 1 {
			t.Errorf("%s appears %d times", id, c)
		}
	}
	if len(count) != 9+n {
		t.Errorf("want %d lessons, got %d", 9+n, len(count))
	}
}

func TestSeenTopicFileBeforeUsed(t *testing.T) {
	dir, _ := vault(t)
	before := snapshot(t, dir)
	res, err := Seen(dir, SeenRequest{ID: "l1", Note: "again.", Repo: "beta", Ref: "#7", Date: "2026-07-03"})
	if err != nil {
		t.Fatal(err)
	}
	if res.File != "Concurrency.md" || res.Used == nil || res.Used.From != 3 || res.Used.To != 4 {
		t.Errorf("result %+v", res)
	}
	want := strings.Replace(fxConcurrency, "- **Avoid by:** take the lock before the check.\n- **Used:** 3\n",
		"- **Avoid by:** take the lock before the check.\n- **Also seen:** 2026-07-03, beta (#7): again.\n- **Used:** 4\n", 1)
	after := snapshot(t, dir)
	if after["Concurrency.md"] != want {
		t.Errorf("got:\n%s\nwant:\n%s", after["Concurrency.md"], want)
	}
	unchangedExcept(t, before, after, "Concurrency.md")
}

func TestSeenAfterLastAlsoSeenAndNoBump(t *testing.T) {
	dir, _ := vault(t)
	if _, err := Seen(dir, SeenRequest{ID: "l8", Note: "third.", Repo: "alpha", Ref: "#5", Date: "2026-07-04", NoBump: true}); err != nil {
		t.Fatal(err)
	}
	want := fxInbox + "- **Also seen:** 2026-07-04, alpha (#5): third.\n"
	if got := read(t, filepath.Join(dir, InboxFile)); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if _, err := Seen(dir, SeenRequest{ID: "l3", Note: "n", Repo: "r", Ref: "x", Date: "2026-07-05"}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dir, "Concurrency.md")); !strings.Contains(got,
		"(#9): it happened again.\n- **Also seen:** 2026-07-05, r (x): n\n- **Used:** 1\n") {
		t.Errorf("l3:\n%s", got)
	}
}

func TestSeenErrorsWriteNothing(t *testing.T) {
	dir, _ := vault(t)
	before := snapshot(t, dir)
	ok := SeenRequest{ID: "l1", Note: "n", Repo: "r", Ref: "#1", Date: "2026-07-01"}
	var nf *NotFoundError
	missing := ok
	missing.ID = "l404"
	if _, err := Seen(dir, missing); !errors.As(err, &nf) {
		t.Errorf("missing: %v", err)
	}
	fp := ok
	fp.ID = "fp1"
	if _, err := Seen(dir, fp); !errors.As(err, &nf) {
		t.Errorf("false-positive id must not match a lesson: %v", err)
	}
	var ae *ArchivedError
	arch := ok
	arch.ID = "l9"
	if _, err := Seen(dir, arch); !errors.As(err, &ae) {
		t.Errorf("archived: %v", err)
	}
	for _, edit := range []func(*SeenRequest){
		func(r *SeenRequest) { r.Note = "" },
		func(r *SeenRequest) { r.Note = "a\nb" },
		func(r *SeenRequest) { r.Date = "today" },
		func(r *SeenRequest) { r.Repo = "" },
	} {
		r := ok
		edit(&r)
		if _, err := Seen(dir, r); err == nil {
			t.Errorf("want error for %+v", r)
		}
	}
	unchangedExcept(t, before, snapshot(t, dir))
}

func TestSeenRevive(t *testing.T) {
	dir, _ := vault(t)
	res, err := Seen(dir, SeenRequest{ID: "l9", Note: "back.", Repo: "alpha", Ref: "#8", Date: "2026-07-06", Revive: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.File != "Concurrency.md" || res.Revived != ArchiveFile || res.Used.To != 10 {
		t.Errorf("result %+v", res)
	}
	want := fxConcurrency + "\n### Archived lesson\n#cr/concurrency · alpha · 2026-01-01 ^l9\n\n- **Avoid by:** archived habit.\n" +
		"- **Also seen:** 2026-07-06, alpha (#8): back.\n- **Used:** 10\n"
	if got := read(t, filepath.Join(dir, "Concurrency.md")); got != want {
		t.Errorf("topic:\n%s\nwant:\n%s", got, want)
	}
	if got := read(t, filepath.Join(dir, ArchiveFile)); got != "# Archived Lessons\n\n" {
		t.Errorf("archive: %q", got)
	}
}

func TestSeenReviveNeedsTopicFile(t *testing.T) {
	dir, _ := vault(t)
	p := filepath.Join(dir, ArchiveFile)
	for _, arch := range []string{
		strings.Replace(fxArchive, "from Concurrency", "from Gone", 1),
		strings.Replace(fxArchive, "from Concurrency", "from ../Concurrency", 1),
		strings.Replace(fxArchive, "from Concurrency", "from Inbox", 1),
		strings.Replace(fxArchive, "- **Retired:** 2026-05-01 from Concurrency (never used)\n", "", 1),
	} {
		if err := os.WriteFile(p, []byte(arch), 0o644); err != nil {
			t.Fatal(err)
		}
		before := snapshot(t, dir)
		if _, err := Seen(dir, SeenRequest{ID: "l9", Note: "n", Repo: "r", Ref: "x", Date: "2026-07-06", Revive: true}); err == nil {
			t.Errorf("want error for archive:\n%s", arch)
		}
		unchangedExcept(t, before, snapshot(t, dir))
	}
}

// triageInbox has two sections; l11 and l12 share one.
const triageInbox = `# Inbox

Header text stays.

Next free id: l20

## 2026-07-02, alpha (#4)

### Media score habit again
#cr/media · alpha · 2026-07-02 ^l11

- **What went wrong:** w.
- **Avoid by:** media habit A again.
- **Used:** 1

### Third fresh lesson
#cr/concurrency · alpha · 2026-07-02 ^l12

- **Avoid by:** third habit.
- **Used:** 0

## 2026-06-30, alpha (#1)

### Fresh inbox lesson
#cr/concurrency · alpha · 2026-06-30 ^l8

- **Avoid by:** fresh habit.
- **Used:** 0
- **Also seen:** 2026-07-01, alpha (#2): again.
`

func triageVault(t *testing.T) string {
	t.Helper()
	dir, _ := vault(t)
	if err := os.WriteFile(filepath.Join(dir, InboxFile), []byte(triageInbox), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTriageList(t *testing.T) {
	dir := triageVault(t)
	rep, err := Triage(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.NextFreeID != "l20" || len(rep.Items) != 3 {
		t.Fatalf("report %+v", rep)
	}
	byID := map[string]TriageItem{}
	for _, it := range rep.Items {
		byID[it.ID] = it
	}
	if it := byID["l11"]; it.Suggested != "Media.md" || it.Section != "## 2026-07-02, alpha (#4)" || len(it.Duplicates) == 0 || it.Duplicates[0].ID != "l6" {
		t.Errorf("l11 %+v", it)
	}
	if it := byID["l8"]; it.Suggested != "Concurrency.md" || it.Section != "## 2026-06-30, alpha (#1)" {
		t.Errorf("l8 %+v", it)
	}
	for _, it := range rep.Items {
		for _, d := range it.Duplicates {
			if d.ID == it.ID {
				t.Errorf("%s lists itself as a duplicate", it.ID)
			}
		}
	}
}

func TestTriageApplyMoveAndMerge(t *testing.T) {
	dir := triageVault(t)
	before := snapshot(t, dir)
	res, err := TriageApply(dir, []PlanStep{
		{ID: "l8", Action: "move", File: "Concurrency.md"},
		{ID: "l11", Action: "merge", Target: "l6", Note: "same media habit."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Applied) != 2 || len(res.DroppedSections) != 1 || res.DroppedSections[0] != "## 2026-06-30, alpha (#1)" {
		t.Errorf("result %+v", res)
	}
	after := snapshot(t, dir)
	wantInbox := strings.Replace(triageInbox, triageInbox[strings.Index(triageInbox, "### Media score"):strings.Index(triageInbox, "### Third")], "", 1)
	wantInbox = wantInbox[:strings.Index(wantInbox, "## 2026-06-30")]
	if after[InboxFile] != wantInbox {
		t.Errorf("inbox:\n%q\nwant:\n%q", after[InboxFile], wantInbox)
	}
	wantConc := fxConcurrency + "\n### Fresh inbox lesson\n#cr/concurrency · alpha · 2026-06-30 ^l8\n\n- **Avoid by:** fresh habit.\n" +
		"- **Used:** 0\n- **Also seen:** 2026-07-01, alpha (#2): again.\n"
	if after["Concurrency.md"] != wantConc {
		t.Errorf("concurrency:\n%s\nwant:\n%s", after["Concurrency.md"], wantConc)
	}
	// Merge: Also seen with the Inbox section's ref and the lesson's date;
	// Used += 1 + the merged lesson's Used (1).
	wantMedia := strings.Replace(fxMedia, "- **Avoid by:** media habit A.\n- **Used:** 1\n",
		"- **Avoid by:** media habit A.\n- **Also seen:** 2026-07-02, alpha (#4): same media habit.\n- **Used:** 3\n", 1)
	if after["Media.md"] != wantMedia {
		t.Errorf("media:\n%s\nwant:\n%s", after["Media.md"], wantMedia)
	}
	unchangedExcept(t, before, after, InboxFile, "Concurrency.md", "Media.md")
	v, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, l := range v.Lessons {
		count[l.ID]++
	}
	if count["l8"] != 1 || count["l11"] != 0 || count["l12"] != 1 {
		t.Errorf("ids after triage: %v", count)
	}
}

// A merge target that is itself an Inbox lesson moved by the same plan
// arrives in its topic file with the new Also seen line.
func TestTriageApplyMergeIntoMovedInboxLesson(t *testing.T) {
	dir := triageVault(t)
	if _, err := TriageApply(dir, []PlanStep{
		{ID: "l12", Action: "merge", Target: "l8", Note: "dup", Ref: "#9", Date: "2026-07-07"},
		{ID: "l8", Action: "move", File: "Concurrency.md"},
	}); err != nil {
		t.Fatal(err)
	}
	conc := read(t, filepath.Join(dir, "Concurrency.md"))
	if !strings.HasSuffix(conc, "- **Also seen:** 2026-07-01, alpha (#2): again.\n- **Also seen:** 2026-07-07, alpha (#9): dup\n") ||
		!strings.Contains(conc, "- **Used:** 1\n- **Also seen:** 2026-07-01") {
		t.Errorf("concurrency:\n%s", conc)
	}
	inbox := read(t, filepath.Join(dir, InboxFile))
	if strings.Contains(inbox, "^l12") || strings.Contains(inbox, "^l8") || !strings.Contains(inbox, "^l11") ||
		!strings.Contains(inbox, "Next free id: l20\n") || strings.Contains(inbox, "## 2026-06-30") {
		t.Errorf("inbox:\n%s", inbox)
	}
}

func TestTriageApplyAllOrNothing(t *testing.T) {
	good := PlanStep{ID: "l8", Action: "move", File: "Concurrency.md"}
	cases := []struct {
		name string
		step PlanStep
		want string
	}{
		{"unknown id", PlanStep{ID: "l404", Action: "move", File: "Media.md"}, "l404"},
		{"not in inbox", PlanStep{ID: "l1", Action: "move", File: "Media.md"}, "exactly one Inbox lesson"},
		{"listed twice", good, "twice"},
		{"bad action", PlanStep{ID: "l11", Action: "delete"}, "action"},
		{"missing file", PlanStep{ID: "l11", Action: "move", File: "Nope.md"}, "does not exist"},
		{"path file", PlanStep{ID: "l11", Action: "move", File: "../Media.md"}, "topic file name"},
		{"archive file", PlanStep{ID: "l11", Action: "move", File: ArchiveFile}, "topic file name"},
		{"inbox file", PlanStep{ID: "l11", Action: "move", File: InboxFile}, "topic file name"},
		{"move with target", PlanStep{ID: "l11", Action: "move", File: "Media.md", Target: "l6"}, "only file"},
		{"merge self", PlanStep{ID: "l11", Action: "merge", Target: "l11"}, "other than itself"},
		{"merge archived", PlanStep{ID: "l11", Action: "merge", Target: "l9"}, "live lesson"},
		{"merge missing target", PlanStep{ID: "l11", Action: "merge", Target: "l404"}, "l404"},
		{"merge into merged", PlanStep{ID: "l11", Action: "merge", Target: "l12"}, "itself merged"},
		{"multi-line note", PlanStep{ID: "l11", Action: "merge", Target: "l6", Note: "a\nb"}, "single line"},
		{"bad date", PlanStep{ID: "l11", Action: "merge", Target: "l6", Date: "x"}, "YYYY-MM-DD"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := triageVault(t)
			before := snapshot(t, dir)
			plan := []PlanStep{good, c.step}
			if c.name == "merge into merged" {
				plan = append(plan, PlanStep{ID: "l12", Action: "merge", Target: "l1"})
			}
			if _, err := TriageApply(dir, plan); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err %v, want %q", err, c.want)
			}
			unchangedExcept(t, before, snapshot(t, dir))
		})
	}
	t.Run("empty plan", func(t *testing.T) {
		if _, err := TriageApply(triageVault(t), nil); err == nil {
			t.Error("want error")
		}
	})
}

// Moving an Inbox lesson whose id is also used in a topic file would leave
// the id twice in the vault: refused.
func TestTriageApplyRefusesDuplicateID(t *testing.T) {
	dir := triageVault(t)
	p := filepath.Join(dir, "Media.md")
	if err := os.WriteFile(p, []byte(fxMedia+"\n### Clash\n#cr/media · x · 2026-01-01 ^l12\n\n- **Used:** 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	if _, err := TriageApply(dir, []PlanStep{{ID: "l12", Action: "move", File: "Concurrency.md"}}); err == nil || !strings.Contains(err.Error(), "unique") {
		t.Fatalf("err %v", err)
	}
	unchangedExcept(t, before, snapshot(t, dir))
}

// A section that was already empty, or that keeps text other than the moved
// lesson, stays.
func TestTriageApplyKeepsSectionsItDidNotEmpty(t *testing.T) {
	dir := triageVault(t)
	inbox := strings.Replace(triageInbox, "## 2026-06-30, alpha (#1)\n\n", "## 2026-07-01, empty (#0)\n\n## 2026-06-30, alpha (#1)\n\nA note under the heading.\n\n", 1)
	if err := os.WriteFile(filepath.Join(dir, InboxFile), []byte(inbox), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := TriageApply(dir, []PlanStep{{ID: "l8", Action: "move", File: "Concurrency.md"}})
	if err != nil {
		t.Fatal(err)
	}
	want := inbox[:strings.Index(inbox, "### Fresh inbox lesson")]
	if got := read(t, filepath.Join(dir, InboxFile)); got != want || len(res.DroppedSections) != 0 {
		t.Errorf("inbox:\n%q\nwant:\n%q (dropped %v)", got, want, res.DroppedSections)
	}
}

// A heading-like line inside a fenced code block belongs to the lesson.
func TestSplitTailIgnoresFences(t *testing.T) {
	block := "### T\n#cr/x · r · 2026-01-01 ^l1\n\n```sh\n# a shell comment\n## not a heading\n```\n- **Used:** 0\n\n## 2026-01-01, r (x)\n\n"
	own, tail := splitTail(block)
	if tail != "## 2026-01-01, r (x)\n\n" || own+tail != block {
		t.Errorf("own %q tail %q", own, tail)
	}
}
