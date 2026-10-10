// Package lessons reads and maintains a folder of code-review lessons kept as
// Markdown (an Obsidian vault folder, for example) and renders the "learned
// checks" section that agentflow appends to a review brief.
//
// The folder holds one file per topic plus three special files: Inbox.md (new,
// untriaged lessons), Archive.md (retired lessons) and
// "Reviewer False Positives.md". A lesson is a block that starts with a
// "### <title>" line, followed by a tag line such as
//
//	#cr/concurrency #cr/database · some-repo · 2026-10-09 · #principle/x ^l937
//
// and bullets: "- **Avoid by:** …", optional "- **Also seen:** …" lines, an
// optional "- **Misled:** N" and "- **Used:** N". A false-positive entry uses
// "#fp/<topic>" tags, a "^fp<N>" id and "Claim", "Why it's wrong", "Tell" and
// "Seen" bullets. Everything else in a file is kept byte for byte.
//
// Inbox.md holds the id counter, a "Next free id: l<N>" line, and groups its
// lessons under dated sections, newest first:
//
//	## 2026-10-09, some-repo (#123)
//
// A lesson ends at the next "### ", "## " or "# " heading outside a fenced
// code block; a "## " section heading that follows a lesson is not part of it.
package lessons

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Special file names inside the lessons folder.
const (
	InboxFile   = "Inbox.md"
	ArchiveFile = "Archive.md"
	FPFile      = "Reviewer False Positives.md"
)

// PrinciplesFile is the default principles file, next to the lessons folder.
// Lessons it links (#^lN) are never retired.
const PrinciplesFile = "Code Review Principles.md"

// archiveHeader starts a new Archive.md.
const archiveHeader = "# Archived Lessons\n\n" +
	"Lessons retired by `agentflow lessons retire`: never used and no activity for the retirement window, " +
	"or misled more often than they helped. Each keeps its `^l` id. To revive one, move it back to its " +
	"topic file and delete its **Retired:** line.\n"

var (
	reLessonID = regexp.MustCompile(`\^(l\d+)[ \t]*$`)
	reFPID     = regexp.MustCompile(`\^(fp\d+)[ \t]*$`)
	reCRTag    = regexp.MustCompile(`#cr/([a-z0-9-]+)`)
	reFPTag    = regexp.MustCompile(`#fp/([a-z0-9-]+)`)
	reDate     = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	reAlso     = regexp.MustCompile(`(?m)^- \*\*Also seen:\*\* .*?(\d{4}-\d{2}-\d{2})`)
	reLinkedID = regexp.MustCompile(`#\^(l\d+)`)
)

func fieldRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^- \*\*` + regexp.QuoteMeta(name) + `:\*\* (\d+)[ \t]*$`)
}

func bulletRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^- \*\*` + regexp.QuoteMeta(name) + `:\*\* (.*)$`)
}

var (
	reUsed   = fieldRe("Used")
	reMisled = fieldRe("Misled")
	reSeen   = fieldRe("Seen")
	reAvoid  = bulletRe("Avoid by")
	reWhat   = bulletRe("What went wrong")
	reAlsoTx = bulletRe("Also seen")
	reWhy    = bulletRe("Why it's wrong")
	reTell   = bulletRe("Tell")
)

// Lesson is one parsed lesson block.
type Lesson struct {
	File   string   `json:"file"`
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Topics []string `json:"topics"`
	Repo   string   `json:"repo"`
	Date   string   `json:"date"`
	Last   string   `json:"last_activity"`
	Used   int      `json:"used"`
	Misled int      `json:"misled"`
	Avoid  string   `json:"-"`
	What   string   `json:"-"`
	Also   []string `json:"-"`
}

// FalsePositive is one parsed reviewer false-positive entry.
type FalsePositive struct {
	ID     string
	Title  string
	Topics []string
	Repo   string
	Seen   int
	Why    string
	Tell   string
}

// Vault is the parsed lessons folder.
type Vault struct {
	Dir            string
	Lessons        []Lesson
	FalsePositives []FalsePositive
}

// splitBlocks splits a Markdown file into the text before the first "### "
// line and the blocks that each start with one. Concatenating the header and
// the blocks gives back the input exactly.
func splitBlocks(text string) (string, []string) {
	var starts []int
	if strings.HasPrefix(text, "### ") {
		starts = append(starts, 0)
	}
	for i := 0; ; {
		j := strings.Index(text[i:], "\n### ")
		if j < 0 {
			break
		}
		starts = append(starts, i+j+1)
		i += j + 1
	}
	if len(starts) == 0 {
		return text, nil
	}
	blocks := make([]string, len(starts))
	for k, s := range starts {
		end := len(text)
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		blocks[k] = text[s:end]
	}
	return text[:starts[0]], blocks
}

// tagLine returns a block's second line (the tag line), or "".
func tagLine(block string) string {
	lines := strings.SplitN(block, "\n", 3)
	if len(lines) < 2 {
		return ""
	}
	return lines[1]
}

func titleOf(block string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.SplitN(block, "\n", 2)[0], "### "))
}

func metaRepo(tag string) string {
	parts := strings.Split(tag, "·")
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func intField(re *regexp.Regexp, block string) int {
	m := re.FindStringSubmatch(block)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func allGroups(re *regexp.Regexp, s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

func parseLesson(file, block string) Lesson {
	tag := tagLine(block)
	l := Lesson{
		File:   file,
		ID:     firstGroup(reLessonID, tag),
		Title:  titleOf(block),
		Topics: allGroups(reCRTag, tag),
		Repo:   metaRepo(tag),
		Used:   intField(reUsed, block),
		Misled: intField(reMisled, block),
		Avoid:  firstGroup(reAvoid, block),
		What:   firstGroup(reWhat, block),
	}
	for _, m := range reAlsoTx.FindAllStringSubmatch(block, -1) {
		l.Also = append(l.Also, strings.TrimSpace(m[1]))
	}
	l.Date = reDate.FindString(tag)
	l.Last = l.Date
	if l.Date != "" {
		for _, m := range reAlso.FindAllStringSubmatch(block, -1) {
			if m[1] > l.Last {
				l.Last = m[1]
			}
		}
	}
	return l
}

func parseFP(block string) FalsePositive {
	tag := tagLine(block)
	return FalsePositive{
		ID:     firstGroup(reFPID, tag),
		Title:  titleOf(block),
		Topics: allGroups(reFPTag, tag),
		Repo:   metaRepo(tag),
		Seen:   intField(reSeen, block),
		Why:    firstGroup(reWhy, block),
		Tell:   firstGroup(reTell, block),
	}
}

// mdFiles lists the folder's .md files, sorted. A missing or unreadable folder
// is an error, never an empty vault.
func mdFiles(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("lessons folder: %w", err)
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Load parses every lesson (Archive.md included, marked by File) and every
// false-positive entry with an id. A folder with no lessons is an error: it
// almost always means the wrong folder.
func Load(dir string) (*Vault, error) {
	files, err := mdFiles(dir)
	if err != nil {
		return nil, err
	}
	v := &Vault{Dir: dir}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		_, blocks := splitBlocks(string(b))
		for _, blk := range blocks {
			if f == FPFile {
				if fp := parseFP(blk); fp.ID != "" { // the header's format example has no id
					v.FalsePositives = append(v.FalsePositives, fp)
				}
				continue
			}
			if l := parseLesson(f, blk); l.ID != "" {
				v.Lessons = append(v.Lessons, l)
			}
		}
	}
	if len(v.Lessons) == 0 {
		return nil, fmt.Errorf("no lessons (### blocks with a ^l<N> id) in %s", dir)
	}
	return v, nil
}

// Topics returns the sorted topic tags used by live (not archived) lessons.
func (v *Vault) Topics() []string {
	set := map[string]bool{}
	for _, l := range v.Lessons {
		if l.File == ArchiveFile {
			continue
		}
		for _, t := range l.Topics {
			set[t] = true
		}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// UnknownTopicError names topics no live lesson is tagged with.
type UnknownTopicError struct{ Unknown, Known []string }

func (e *UnknownTopicError) Error() string {
	return fmt.Sprintf("no lessons are tagged with topic(s) %s; known topics: %s",
		strings.Join(e.Unknown, ", "), strings.Join(e.Known, ", "))
}

func intersects(a []string, set map[string]bool) bool {
	for _, x := range a {
		if set[x] {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

// BriefResult is the rendered section and what it includes.
type BriefResult struct {
	Markdown       string
	Lessons        []string
	FalsePositives []string
}

// Brief renders the learned-checks section for a diff touching topics. Live
// lessons (topic files and the Inbox, not the Archive) that share a topic are
// ranked by Used − 2×Misled (negative scores are left out), then by a match on
// repo, then by most recent activity. False positives share a topic, carry no
// topic, or are tagged "process"; they rank by Seen, then by repo.
func (v *Vault) Brief(topics []string, repo string, top, fpTop int) (BriefResult, error) {
	want := map[string]bool{}
	known := map[string]bool{}
	for _, t := range v.Topics() {
		known[t] = true
	}
	var unknown []string
	for _, t := range topics {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !known[t] {
			unknown = append(unknown, t)
		}
		want[t] = true
	}
	if len(unknown) > 0 {
		return BriefResult{}, &UnknownTopicError{Unknown: unknown, Known: v.Topics()}
	}
	if len(want) == 0 {
		return BriefResult{}, errors.New("no topics given")
	}
	repo = strings.ToLower(strings.TrimSpace(repo))
	repoMatch := func(r string) bool { return repo != "" && strings.Contains(strings.ToLower(r), repo) }

	type ranked struct {
		l     Lesson
		score int
	}
	var picked []ranked
	for _, l := range v.Lessons {
		if l.File == ArchiveFile || l.Avoid == "" || !intersects(l.Topics, want) {
			continue
		}
		if s := l.Used - 2*l.Misled; s >= 0 {
			picked = append(picked, ranked{l, s})
		}
	}
	sort.SliceStable(picked, func(i, j int) bool {
		a, b := picked[i], picked[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if ra, rb := repoMatch(a.l.Repo), repoMatch(b.l.Repo); ra != rb {
			return ra
		}
		if a.l.Last != b.l.Last {
			return a.l.Last > b.l.Last
		}
		return a.l.ID < b.l.ID
	})
	if len(picked) > top {
		picked = picked[:top]
	}

	var fps []FalsePositive
	for _, f := range v.FalsePositives {
		if len(f.Topics) == 0 || intersects(f.Topics, want) || intersects(f.Topics, map[string]bool{"process": true}) {
			fps = append(fps, f)
		}
	}
	sort.SliceStable(fps, func(i, j int) bool {
		if fps[i].Seen != fps[j].Seen {
			return fps[i].Seen > fps[j].Seen
		}
		if ri, rj := repoMatch(fps[i].Repo), repoMatch(fps[j].Repo); ri != rj {
			return ri
		}
		return fps[i].ID < fps[j].ID
	})
	if len(fps) > fpTop {
		fps = fps[:fpTop]
	}

	var res BriefResult
	var b strings.Builder
	b.WriteString("## Learned checks (second pass)\n\n")
	b.WriteString("Do your open review first. Then check the diff against each lesson below. " +
		"These are defects we have shipped or nearly shipped before. " +
		"Report a finding from this pass with the tag `[learned <id>]`, for example `[learned l346]`. " +
		"Skip a lesson that does not apply; do not report \"not applicable\".\n\n")
	for _, p := range picked {
		fmt.Fprintf(&b, "- **%s**: %s. Check: %s\n", p.l.ID, p.l.Title, clip(p.l.Avoid, 400))
		res.Lessons = append(res.Lessons, p.l.ID)
	}
	if len(fps) > 0 {
		b.WriteString("\n## Known false positives\n\n")
		b.WriteString("Reviewers have flagged these before, and they were wrong. Do not report one of these " +
			"unless you can show the concrete failing scenario in THIS diff.\n\n")
		for _, f := range fps {
			fmt.Fprintf(&b, "- **%s**: %s. Why it is usually wrong: %s Tell: %s\n", f.ID, f.Title, clip(f.Why, 300), clip(f.Tell, 200))
			res.FalsePositives = append(res.FalsePositives, f.ID)
		}
	}
	res.Markdown = b.String()
	return res, nil
}

// lock serializes writers to one lessons folder across processes. The lock
// file lives in the temp dir so a synced folder (iCloud) never sees it.
func lock(dir string) (func(), error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(abs))
	p := filepath.Join(os.TempDir(), "agentflow-lessons-"+hex.EncodeToString(sum[:8])+".lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// writeAtomic replaces path with data via a temp file and rename, keeping the
// file's mode, so a reader (or a sync client) never sees a half-written file.
func writeAtomic(path string, data string) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agentflow-lessons-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// Change is one counter change made by Bump.
type Change struct {
	ID    string `json:"id"`
	File  string `json:"file"`
	Field string `json:"field"`
	From  int    `json:"from"`
	To    int    `json:"to"`
}

// NotFoundError lists ids Bump could not find; nothing was written.
type NotFoundError struct{ IDs []string }

func (e *NotFoundError) Error() string {
	return "not found (nothing written): " + strings.Join(e.IDs, ", ")
}

// Increments maps an id to how much to add. Listing an id twice adds twice.
type Increments map[string]int

// BumpRequest names the counters to raise.
type BumpRequest struct {
	Used, Misled, Seen Increments
}

func (r BumpRequest) empty() bool { return len(r.Used)+len(r.Misled)+len(r.Seen) == 0 }

// bumpBlock raises field in block by n, inserting the field just before the
// Used line when it is absent (Misled is optional).
func bumpBlock(block, field string, n int) (string, int, int, error) {
	re := fieldRe(field)
	if loc := re.FindStringSubmatchIndex(block); loc != nil {
		from, _ := strconv.Atoi(block[loc[2]:loc[3]])
		return block[:loc[0]] + fmt.Sprintf("- **%s:** %d", field, from+n) + block[loc[1]:], from, from + n, nil
	}
	if field == "Misled" {
		if loc := reUsed.FindStringIndex(block); loc != nil {
			return block[:loc[0]] + fmt.Sprintf("- **Misled:** %d\n", n) + block[loc[0]:], 0, n, nil
		}
	}
	return "", 0, 0, fmt.Errorf("%s has no %q line", titleOf(block), field)
}

// Bump raises Used, Misled (lessons, any file but the false-positive file) and
// Seen (false-positive entries) all together: if any id is missing or appears
// twice, nothing is written.
func Bump(dir string, req BumpRequest) ([]Change, error) {
	if req.empty() {
		return nil, errors.New("nothing to bump")
	}
	unlock, err := lock(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	files, err := mdFiles(dir)
	if err != nil {
		return nil, err
	}
	type job struct {
		field string
		inc   Increments
		re    *regexp.Regexp
		fp    bool
	}
	jobs := []job{{"Used", req.Used, reLessonID, false}, {"Misled", req.Misled, reLessonID, false}, {"Seen", req.Seen, reFPID, true}}
	found := map[string]int{} // field+id -> occurrences
	contents := map[string]string{}
	var changes []Change
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		header, blocks := splitBlocks(string(raw))
		changed := false
		for i := range blocks {
			for _, j := range jobs {
				if len(j.inc) == 0 || j.fp != (f == FPFile) {
					continue
				}
				id := firstGroup(j.re, tagLine(blocks[i]))
				n, ok := j.inc[id]
				if id == "" || !ok {
					continue
				}
				found[j.field+" "+id]++
				nb, from, to, err := bumpBlock(blocks[i], j.field, n)
				if err != nil {
					return nil, fmt.Errorf("%s (%s): %w", id, f, err)
				}
				blocks[i] = nb
				changed = true
				changes = append(changes, Change{ID: id, File: f, Field: j.field, From: from, To: to})
			}
		}
		if changed {
			contents[f] = header + strings.Join(blocks, "")
		}
	}
	var missing, dup []string
	for _, j := range jobs {
		for id := range j.inc {
			switch found[j.field+" "+id] {
			case 0:
				missing = append(missing, id)
			case 1:
			default:
				dup = append(dup, id)
			}
		}
	}
	sort.Strings(missing)
	sort.Strings(dup)
	if len(dup) > 0 {
		return nil, fmt.Errorf("id(s) appear in more than one block (nothing written): %s", strings.Join(dup, ", "))
	}
	if len(missing) > 0 {
		return nil, &NotFoundError{IDs: missing}
	}
	for f, c := range contents {
		if err := writeAtomic(filepath.Join(dir, f), c); err != nil {
			return nil, err
		}
	}
	return changes, nil
}

// Candidate is a lesson that meets a retirement rule.
type Candidate struct {
	Lesson
	Reason string `json:"reason"`
	Linked bool   `json:"linked_from_principles"`
}

// linkedIDs reads the lesson ids the principles file links. A missing
// principles file means no links; any other read error is returned, since
// retiring a linked lesson would break the link.
func linkedIDs(principles string) (map[string]bool, error) {
	b, err := os.ReadFile(principles)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, m := range reLinkedID.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = true
	}
	return out, nil
}

// isTopicFile reports whether retirement applies to file: not the Inbox (not
// yet triaged), the Archive or the false-positive file.
func isTopicFile(f string) bool {
	return f != InboxFile && f != ArchiveFile && f != FPFile
}

// RetireCandidates lists topic-file lessons with Misled > Used, or with Used 0
// and no activity (date or Also seen) within days of today. Lessons with no
// date are never retired by age.
func RetireCandidates(dir, principles string, days int, today time.Time) ([]Candidate, error) {
	v, err := Load(dir)
	if err != nil {
		return nil, err
	}
	linked, err := linkedIDs(principles)
	if err != nil {
		return nil, err
	}
	cutoff := today.AddDate(0, 0, -days).Format("2006-01-02")
	var out []Candidate
	for _, l := range v.Lessons {
		if !isTopicFile(l.File) {
			continue
		}
		var reason string
		switch {
		case l.Misled > l.Used:
			reason = fmt.Sprintf("misled %d > used %d", l.Misled, l.Used)
		case l.Used == 0 && l.Last != "" && l.Last <= cutoff:
			reason = "never used, last activity " + l.Last
		default:
			continue
		}
		out = append(out, Candidate{Lesson: l, Reason: reason, Linked: linked[l.ID]})
	}
	return out, nil
}

// Retire moves every unlinked candidate to Archive.md, adding a Retired line.
// The archive is written before the topic files, so an interrupted run can
// leave a lesson in both places but never in neither.
func Retire(dir, principles string, days int, today time.Time) ([]Candidate, error) {
	unlock, err := lock(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	cands, err := RetireCandidates(dir, principles, days, today)
	if err != nil {
		return nil, err
	}
	move := map[string]Candidate{}
	for _, c := range cands {
		if !c.Linked {
			move[c.ID] = c
		}
	}
	if len(move) == 0 {
		return nil, nil
	}
	files, err := mdFiles(dir)
	if err != nil {
		return nil, err
	}
	var moved []string
	var retired []Candidate
	rewrites := map[string]string{}
	for _, f := range files {
		if !isTopicFile(f) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		header, blocks := splitBlocks(string(raw))
		var keep []string
		for _, blk := range blocks {
			id := firstGroup(reLessonID, tagLine(blk))
			c, ok := move[id]
			if !ok || id == "" {
				keep = append(keep, blk)
				continue
			}
			retiredLine := fmt.Sprintf("- **Retired:** %s from %s (%s)\n",
				today.Format("2006-01-02"), strings.TrimSuffix(f, ".md"), c.Reason)
			moved = append(moved, insertBeforeTrailingNewlines(blk, retiredLine))
			retired = append(retired, c)
		}
		if len(keep) == len(blocks) {
			continue
		}
		rewrites[f] = header + strings.Join(keep, "")
	}
	ap := filepath.Join(dir, ArchiveFile)
	old, err := os.ReadFile(ap)
	if errors.Is(err, os.ErrNotExist) {
		old, err = []byte(archiveHeader), nil
	}
	if err != nil {
		return nil, err
	}
	archive := string(old)
	for _, blk := range moved {
		archive = appendMarkdownBlock(archive, blk)
	}
	if err := writeAtomic(ap, archive); err != nil {
		return nil, err
	}
	for f, c := range rewrites {
		if err := writeAtomic(filepath.Join(dir, f), c); err != nil {
			return nil, err
		}
	}
	return retired, nil
}

// insertBeforeTrailingNewlines adds line to a block without deleting or
// rewriting any byte already in the block. Keeping the original trailing
// newlines after the inserted line also preserves the separator before the
// next heading when the block came from the middle of a topic file.
func insertBeforeTrailingNewlines(block, line string) string {
	i := len(block)
	for i > 0 && block[i-1] == '\n' {
		i--
	}
	if i < len(block) {
		return block[:i+1] + line + block[i+1:]
	}
	return block + "\n" + line
}

// appendMarkdownBlock appends a block after at least one blank line. It only
// adds separator bytes; in particular, it never trims or normalizes the
// existing archive or the moved block.
func appendMarkdownBlock(text, block string) string {
	newlines := 0
	for i := len(text); i > 0 && text[i-1] == '\n'; i-- {
		newlines++
	}
	switch newlines {
	case 0:
		text += "\n\n"
	case 1:
		text += "\n"
	}
	return text + block
}

// Stats summarizes the folder.
type Stats struct {
	Lessons          int `json:"lessons"`
	Inbox            int `json:"inbox"`
	Archived         int `json:"archived"`
	UsedZero         int `json:"used_zero"`
	MisledAny        int `json:"misled_nonzero"`
	RetireCandidates int `json:"retire_candidates"`
	RetireLinked     int `json:"retire_kept_linked"`
	FalsePositives   int `json:"false_positives"`
}

// Summarize counts live lessons (topic files and Inbox), archived lessons and
// retirement candidates for the given window.
func Summarize(dir, principles string, days int, today time.Time) (Stats, error) {
	v, err := Load(dir)
	if err != nil {
		return Stats{}, err
	}
	var s Stats
	for _, l := range v.Lessons {
		switch l.File {
		case ArchiveFile:
			s.Archived++
			continue
		case InboxFile:
			s.Inbox++
		}
		s.Lessons++
		if l.Used == 0 {
			s.UsedZero++
		}
		if l.Misled > 0 {
			s.MisledAny++
		}
	}
	cands, err := RetireCandidates(dir, principles, days, today)
	if err != nil {
		return Stats{}, err
	}
	for _, c := range cands {
		if c.Linked {
			s.RetireLinked++
		} else {
			s.RetireCandidates++
		}
	}
	s.FalsePositives = len(v.FalsePositives)
	return s, nil
}
