package lessons

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file holds the commands that write lessons: Add (a new Inbox lesson),
// Seen (an Also seen line on an existing lesson) and TriageApply (moving or
// merging Inbox lessons). Each one takes the folder lock, validates
// everything, and only then writes, each file atomically. A file it changes
// keeps every byte outside the span it owns: the inserted block or line, the
// removed block, the counter digits.

var (
	reNextID    = regexp.MustCompile(`(?m)^Next free id: l(\d+)[ \t]*\r?$`)
	reTopicName = regexp.MustCompile(`^[a-z0-9-]+$`)
	rePrinciple = regexp.MustCompile(`(?m)^## ([a-z0-9-]+)[ \t]*\r?$`)
	reSection   = regexp.MustCompile(`^## (\d{4}-\d{2}-\d{2}), (.*?) \((.*)\)[ \t]*\r?$`)
	reSecDate   = regexp.MustCompile(`^## (\d{4}-\d{2}-\d{2})\b`)
	reRetired   = regexp.MustCompile(`(?m)^- \*\*Retired:\*\* \S+ from (.+?) \(.*\n?`)
	reAlsoLine  = regexp.MustCompile(`(?m)^- \*\*Also seen:\*\* .*$`)
)

// DefaultDupThreshold is the search score from which Add and Triage report a
// lesson as a candidate duplicate.
const DefaultDupThreshold = 0.35

// ErrDuplicate is returned by Add with IfNoDuplicate when a candidate
// duplicate was found; nothing was written.
var ErrDuplicate = errors.New("candidate duplicate found (nothing written)")

// ArchivedError reports that Seen found the id in Archive.md without Revive.
type ArchivedError struct{ ID string }

func (e *ArchivedError) Error() string {
	return e.ID + " is archived (nothing written); pass --revive to move it back to its topic file"
}

// idNum returns the number in "l12" or "fp3", or -1.
func idNum(id string) int {
	n, err := strconv.Atoi(strings.TrimLeft(id, "lfp"))
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// lines calls fn for each line of text with its start offset and its text
// without the newline, and whether it is outside a fenced code block. A
// fence line itself counts as inside.
func lines(text string, fn func(start int, line string, outside bool) bool) {
	fenced := false
	for start := 0; start < len(text); {
		end := strings.IndexByte(text[start:], '\n')
		next := len(text)
		if end >= 0 {
			end += start
			next = end + 1
		} else {
			end = len(text)
		}
		line := text[start:end]
		isFence := strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
		if isFence {
			fenced = !fenced
		}
		if !fn(start, line, !fenced && !isFence) {
			return
		}
		start = next
	}
}

// isSectionHeading reports a "# " or "## " heading line: one that ends the
// lesson before it.
func isSectionHeading(line string) bool {
	return strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ")
}

// splitTail splits a block from splitBlocks into the lesson itself and a tail
// that starts at the first "# " or "## " heading after the title (in the Inbox,
// the heading of the next dated section). own + tail == block.
func splitTail(block string) (own, tail string) {
	cut := len(block)
	first := true
	lines(block, func(start int, line string, outside bool) bool {
		if first {
			first = false
			return true
		}
		if outside && isSectionHeading(line) {
			cut = start
			return false
		}
		return true
	})
	return block[:cut], block[cut:]
}

// trimNewlines returns block ending in exactly one newline.
func trimNewlines(block string) string {
	return strings.TrimRight(block, "\n") + "\n"
}

// fileSet is every .md file of the folder, split into header and blocks, so a
// command can change some blocks and write back only the files it changed.
type fileSet struct {
	dir    string
	names  []string
	header map[string]string
	blocks map[string][]string
	dirty  map[string]bool
}

type loc struct {
	file string
	idx  int
}

func loadFiles(dir string) (*fileSet, error) {
	names, err := mdFiles(dir)
	if err != nil {
		return nil, err
	}
	fs := &fileSet{dir: dir, names: names, header: map[string]string{}, blocks: map[string][]string{}, dirty: map[string]bool{}}
	for _, f := range names {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		fs.header[f], fs.blocks[f] = splitBlocks(string(b))
	}
	return fs, nil
}

func (fs *fileSet) has(f string) bool {
	_, ok := fs.blocks[f]
	return ok
}

func (fs *fileSet) text(f string) string {
	return fs.header[f] + strings.Join(fs.blocks[f], "")
}

// find returns every block whose tag line carries the lesson id (the
// false-positive file holds ^fp ids only and is skipped).
func (fs *fileSet) find(id string) []loc {
	var out []loc
	for _, f := range fs.names {
		if f == FPFile {
			continue
		}
		for i, b := range fs.blocks[f] {
			if firstGroup(reLessonID, tagLine(b)) == id {
				out = append(out, loc{f, i})
			}
		}
	}
	return out
}

// maxID returns the highest lesson id number in the folder, or 0.
func (fs *fileSet) maxID() int {
	max := 0
	for _, f := range fs.names {
		if f == FPFile {
			continue
		}
		for _, b := range fs.blocks[f] {
			if n := idNum(firstGroup(reLessonID, tagLine(b))); n > max {
				max = n
			}
		}
	}
	return max
}

// write writes the changed files, the ones in last after all others, so an
// interrupted run can leave a lesson in two files but never in none.
func (fs *fileSet) write(last string) error {
	var order []string
	for f := range fs.dirty {
		if f != last {
			order = append(order, f)
		}
	}
	sort.Strings(order)
	if fs.dirty[last] {
		order = append(order, last)
	}
	for _, f := range order {
		if err := writeAtomic(filepath.Join(fs.dir, f), fs.text(f)); err != nil {
			return err
		}
	}
	return nil
}

func (fs *fileSet) findOne(id string) (loc, error) {
	locs := fs.find(id)
	switch len(locs) {
	case 0:
		return loc{}, &NotFoundError{IDs: []string{id}}
	case 1:
		return locs[0], nil
	}
	var files []string
	for _, l := range locs {
		files = append(files, l.file)
	}
	return loc{}, fmt.Errorf("%s appears in more than one block (%s); fix the duplicate first (nothing written)", id, strings.Join(files, ", "))
}

// checkLine rejects an empty or multi-line field.
func checkLine(name, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if strings.ContainsAny(v, "\r\n") {
		return fmt.Errorf("%s must be a single line", name)
	}
	return nil
}

func checkDate(d string) error {
	if _, err := time.Parse("2006-01-02", d); err != nil {
		return fmt.Errorf("date %q must be YYYY-MM-DD", d)
	}
	return nil
}

// insertAlsoSeen adds line (ending in a newline) after the lesson's last Also
// seen line, or else just before its Misled or Used line.
func insertAlsoSeen(own, line string) (string, error) {
	if locs := reAlsoLine.FindAllStringIndex(own, -1); len(locs) > 0 {
		end := locs[len(locs)-1][1]
		if end == len(own) {
			return own + "\n" + line, nil
		}
		return own[:end+1] + line + own[end+1:], nil
	}
	for _, re := range []*regexp.Regexp{reMisled, reUsed} {
		if l := re.FindStringIndex(own); l != nil {
			return own[:l[0]] + line + own[l[0]:], nil
		}
	}
	return "", fmt.Errorf("%s has no Used line", titleOf(own))
}

func alsoSeenLine(date, repo, ref, note string) string {
	if ref == "" {
		return fmt.Sprintf("- **Also seen:** %s, %s: %s\n", date, repo, note)
	}
	return fmt.Sprintf("- **Also seen:** %s, %s (%s): %s\n", date, repo, ref, note)
}

// spliceBlock inserts chunk (ending in a newline) at pos, adding only the
// newlines needed for a blank line before it and, when text follows, after it.
func spliceBlock(text string, pos int, chunk string) string {
	before, after := text[:pos], text[pos:]
	if before != "" {
		if !strings.HasSuffix(before, "\n") {
			chunk = "\n\n" + chunk
		} else if !strings.HasSuffix(before, "\n\n") {
			chunk = "\n" + chunk
		}
	}
	if after != "" {
		chunk += "\n"
	}
	return before + chunk + after
}

// AddRequest is a new lesson for the Inbox.
type AddRequest struct {
	Repo, Ref, Title, What, Why, Avoid, Principle, Date string
	Topics                                              []string
	NewTopic, DryRun, IfNoDuplicate                     bool
	DupThreshold                                        float64
}

// AddResult says what Add wrote, or would write with DryRun.
type AddResult struct {
	ID             string  `json:"id"`
	File           string  `json:"file"`
	Section        string  `json:"section"`
	CreatedSection bool    `json:"created_section"`
	NextFreeID     string  `json:"next_free_id"`
	CounterWas     string  `json:"counter_was,omitempty"`
	Written        bool    `json:"written"`
	Duplicates     []Match `json:"duplicates"`
	Block          string  `json:"block,omitempty"`
}

func (r *AddRequest) validate() error {
	for _, f := range []struct{ name, v string }{
		{"--repo", r.Repo}, {"--ref", r.Ref}, {"--title", r.Title}, {"--what", r.What},
		{"--why", r.Why}, {"--avoid", r.Avoid}, {"--date", r.Date},
	} {
		if err := checkLine(f.name, f.v); err != nil {
			return err
		}
	}
	if strings.ContainsAny(r.Repo, "·,()") {
		return errors.New("--repo must not contain '·', ',' or parentheses")
	}
	if err := checkDate(r.Date); err != nil {
		return err
	}
	if len(r.Topics) == 0 {
		return errors.New("--topics is required")
	}
	seen := map[string]bool{}
	for _, t := range r.Topics {
		if !reTopicName.MatchString(t) {
			return fmt.Errorf("topic %q must be lowercase letters, digits and dashes", t)
		}
		if seen[t] {
			return fmt.Errorf("topic %q listed twice", t)
		}
		seen[t] = true
	}
	if r.Principle != "" && !reTopicName.MatchString(r.Principle) {
		return fmt.Errorf("principle %q must be lowercase letters, digits and dashes", r.Principle)
	}
	return nil
}

func (r *AddRequest) render(id string) string {
	var tags []string
	for _, t := range r.Topics {
		tags = append(tags, "#cr/"+t)
	}
	meta := strings.Join(tags, " ") + " · " + strings.TrimSpace(r.Repo) + " · " + r.Date
	if r.Principle != "" {
		meta += " · #principle/" + r.Principle
	}
	return fmt.Sprintf("### %s\n%s ^%s\n\n- **What went wrong:** %s\n- **Why:** %s\n- **Avoid by:** %s\n- **Used:** 0\n",
		strings.TrimSpace(r.Title), meta, id, strings.TrimSpace(r.What), strings.TrimSpace(r.Why), strings.TrimSpace(r.Avoid))
}

// principleNames reads the "## <name>" headings of the principles file.
func principleNames(path string) (map[string]bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("principles file: %w", err)
	}
	out := map[string]bool{}
	for _, m := range rePrinciple.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = true
	}
	return out, nil
}

// Add allocates the next lesson id from the Inbox's "Next free id: lN" line,
// raises that line, and inserts the lesson at the top of the Inbox section
// "## <date>, <repo> (<ref>)", creating the section newest first when it is
// absent. If the counter is not above every id in the folder (someone added a
// lesson by hand), the id after the highest one is used instead. Candidate
// duplicates (search score ≥ DupThreshold, Archive included) are reported;
// with IfNoDuplicate any candidate stops the write with ErrDuplicate.
func Add(dir, principles string, req AddRequest) (AddResult, error) {
	if err := req.validate(); err != nil {
		return AddResult{}, err
	}
	unlock, err := lock(dir)
	if err != nil {
		return AddResult{}, err
	}
	defer unlock()
	v, err := Load(dir)
	if err != nil {
		return AddResult{}, err
	}
	if !req.NewTopic {
		known := map[string]bool{}
		for _, t := range v.Topics() {
			known[t] = true
		}
		var unknown []string
		for _, t := range req.Topics {
			if !known[t] {
				unknown = append(unknown, t)
			}
		}
		if len(unknown) > 0 {
			return AddResult{}, fmt.Errorf("%w (pass --new-topic to add one)", &UnknownTopicError{Unknown: unknown, Known: v.Topics()})
		}
	}
	if req.Principle != "" {
		names, err := principleNames(principles)
		if err != nil {
			return AddResult{}, err
		}
		if !names[req.Principle] {
			return AddResult{}, fmt.Errorf("principle %q is not a \"## <name>\" heading in %s", req.Principle, principles)
		}
	}
	fs, err := loadFiles(dir)
	if err != nil {
		return AddResult{}, err
	}
	if !fs.has(InboxFile) {
		return AddResult{}, fmt.Errorf("%s not found in %s", InboxFile, dir)
	}
	inbox := fs.text(InboxFile)
	m := reNextID.FindAllStringSubmatchIndex(inbox, -1)
	if len(m) != 1 {
		return AddResult{}, fmt.Errorf("%s must have exactly one \"Next free id: l<N>\" line, found %d", InboxFile, len(m))
	}
	next, err := strconv.Atoi(inbox[m[0][2]:m[0][3]])
	if err != nil {
		return AddResult{}, err
	}
	res := AddResult{File: InboxFile, Duplicates: []Match{}}
	if max := fs.maxID(); next <= max {
		res.CounterWas = fmt.Sprintf("l%d", next)
		next = max + 1
	}
	res.ID = fmt.Sprintf("l%d", next)
	res.NextFreeID = fmt.Sprintf("l%d", next+1)
	block := req.render(res.ID)

	threshold := req.DupThreshold
	if threshold <= 0 {
		threshold = DefaultDupThreshold
	}
	if d := v.Search(req.Title+" "+req.Avoid, SearchOptions{IncludeArchive: true, Limit: 5, MinScore: threshold}); d != nil {
		res.Duplicates = d
	}
	if req.IfNoDuplicate && len(res.Duplicates) > 0 {
		return res, ErrDuplicate
	}

	inbox = inbox[:m[0][2]] + strconv.Itoa(next+1) + inbox[m[0][3]:]
	res.Section = fmt.Sprintf("## %s, %s (%s)", req.Date, strings.TrimSpace(req.Repo), strings.TrimSpace(req.Ref))
	inbox, res.CreatedSection = insertInbox(inbox, res.Section, req.Date, block)
	if req.DryRun {
		res.Block = block
		return res, nil
	}
	if err := writeAtomic(filepath.Join(dir, InboxFile), inbox); err != nil {
		return AddResult{}, err
	}
	res.Written = true
	return res, nil
}

// section is one "## " heading of a file and the text up to the next "# " or
// "## " heading.
type section struct {
	start, end int
	heading    string
	empty      bool
}

func sections(text string) []section {
	var out []section
	closeLast := func(at int) {
		if n := len(out); n > 0 && out[n-1].end < 0 {
			out[n-1].end = at
			body := text[out[n-1].start:at]
			if i := strings.IndexByte(body, '\n'); i >= 0 {
				out[n-1].empty = strings.TrimSpace(body[i+1:]) == ""
			} else {
				out[n-1].empty = true
			}
		}
	}
	lines(text, func(start int, line string, outside bool) bool {
		if outside && isSectionHeading(line) {
			closeLast(start)
			if strings.HasPrefix(line, "## ") {
				out = append(out, section{start: start, end: -1, heading: strings.TrimRight(line, " \t\r")})
			}
		}
		return true
	})
	closeLast(len(text))
	return out
}

// insertInbox inserts block at the top of the section with heading, or in a
// new section placed before the first dated section that is not newer.
func insertInbox(text, heading, date, block string) (string, bool) {
	secs := sections(text)
	for _, s := range secs {
		if s.heading != heading {
			continue
		}
		pos := s.end
		first := true
		lines(text[s.start:s.end], func(start int, line string, _ bool) bool {
			if first {
				first = false
				return true
			}
			if strings.TrimSpace(line) != "" {
				pos = s.start + start
				return false
			}
			return true
		})
		return spliceBlock(text, pos, block), false
	}
	chunk := heading + "\n\n" + block
	for _, s := range secs {
		if m := reSecDate.FindStringSubmatch(s.heading); m != nil && m[1] <= date {
			return spliceBlock(text, s.start, chunk), true
		}
	}
	return spliceBlock(text, len(text), chunk), true
}

// SeenRequest adds an Also seen line to a lesson.
type SeenRequest struct {
	ID, Note, Repo, Ref, Date string
	NoBump, Revive            bool
}

// SeenResult says what Seen changed.
type SeenResult struct {
	ID      string  `json:"id"`
	File    string  `json:"file"`
	Line    string  `json:"line"`
	Used    *Change `json:"used,omitempty"`
	Revived string  `json:"revived_from,omitempty"`
}

// Seen adds "- **Also seen:** <date>, <repo> (<ref>): <note>" to a lesson in
// any file and raises its Used by one (unless NoBump), in one write. A lesson
// in Archive.md needs Revive: it then moves back to the end of the topic file
// its Retired line names, without that line. The topic file is written before
// the archive.
func Seen(dir string, req SeenRequest) (SeenResult, error) {
	for _, f := range []struct{ name, v string }{{"id", req.ID}, {"--note", req.Note}, {"--repo", req.Repo}, {"--ref", req.Ref}, {"--date", req.Date}} {
		if err := checkLine(f.name, f.v); err != nil {
			return SeenResult{}, err
		}
	}
	if err := checkDate(req.Date); err != nil {
		return SeenResult{}, err
	}
	unlock, err := lock(dir)
	if err != nil {
		return SeenResult{}, err
	}
	defer unlock()
	fs, err := loadFiles(dir)
	if err != nil {
		return SeenResult{}, err
	}
	at, err := fs.findOne(req.ID)
	if err != nil {
		return SeenResult{}, err
	}
	if at.file == ArchiveFile && !req.Revive {
		return SeenResult{}, &ArchivedError{ID: req.ID}
	}
	own, tail := splitTail(fs.blocks[at.file][at.idx])
	line := alsoSeenLine(req.Date, strings.TrimSpace(req.Repo), strings.TrimSpace(req.Ref), strings.TrimSpace(req.Note))
	if own, err = insertAlsoSeen(own, line); err != nil {
		return SeenResult{}, err
	}
	res := SeenResult{ID: req.ID, File: at.file, Line: strings.TrimSuffix(line, "\n")}
	if !req.NoBump {
		nb, from, to, err := bumpBlock(own, "Used", 1)
		if err != nil {
			return SeenResult{}, err
		}
		own = nb
		res.Used = &Change{ID: req.ID, File: at.file, Field: "Used", From: from, To: to}
	}
	if at.file == ArchiveFile {
		m := reRetired.FindStringSubmatchIndex(own)
		if m == nil {
			return SeenResult{}, fmt.Errorf("%s has no \"Retired: <date> from <topic> (…)\" line naming its topic file", req.ID)
		}
		dest := own[m[2]:m[3]] + ".md"
		if filepath.Base(dest) != dest || !isTopicFile(dest) || !fs.has(dest) {
			return SeenResult{}, fmt.Errorf("%s: its Retired line names %q, which is not a topic file in the folder (nothing written)", req.ID, dest)
		}
		own = own[:m[0]] + own[m[1]:]
		fs.blocks[at.file][at.idx] = tail
		fs.dirty[at.file] = true
		fs.header[dest], fs.blocks[dest] = appendMarkdownBlock(fs.text(dest), trimNewlines(own)), nil
		fs.dirty[dest] = true
		res.Revived, res.File = ArchiveFile, dest
		if res.Used != nil {
			res.Used.File = dest
		}
		return res, fs.write(ArchiveFile)
	}
	fs.blocks[at.file][at.idx] = own + tail
	fs.dirty[at.file] = true
	return res, fs.write("")
}

// TriageItem is one Inbox lesson with what triage needs to decide on it.
type TriageItem struct {
	ID         string   `json:"id"`
	Title      string   `json:"title"`
	Topics     []string `json:"topics"`
	Repo       string   `json:"repo"`
	Date       string   `json:"date"`
	Section    string   `json:"section"`
	Used       int      `json:"used"`
	Suggested  string   `json:"suggested_file"`
	Duplicates []Match  `json:"duplicates"`
}

// TriageReport lists the Inbox for triage.
type TriageReport struct {
	NextFreeID string       `json:"next_free_id"`
	Items      []TriageItem `json:"items"`
}

// inboxSections maps each Inbox lesson id to the "## " heading it sits under.
func inboxSections(header string, blocks []string) map[string]string {
	cur := ""
	last := func(s string) {
		for _, sec := range sections(s) {
			cur = sec.heading
		}
	}
	last(header)
	out := map[string]string{}
	for _, b := range blocks {
		own, tail := splitTail(b)
		if id := firstGroup(reLessonID, tagLine(own)); id != "" {
			out[id] = cur
		}
		last(tail)
	}
	return out
}

// suggestFile picks the topic file holding the most live lessons tagged with
// the lesson's first topic that any topic file uses (ties: file name order).
func suggestFile(v *Vault, topics []string) string {
	for _, t := range topics {
		count := map[string]int{}
		for _, l := range v.Lessons {
			if isTopicFile(l.File) && len(l.Topics) > 0 && l.Topics[0] == t {
				count[l.File]++
			}
		}
		best, n := "", 0
		for f, c := range count {
			if c > n || (c == n && f < best) {
				best, n = f, c
			}
		}
		if best != "" {
			return best
		}
	}
	return ""
}

// Triage lists every Inbox lesson with its candidate duplicates and a
// suggested topic file. It writes nothing.
func Triage(dir string, threshold float64) (TriageReport, error) {
	if threshold <= 0 {
		threshold = DefaultDupThreshold
	}
	v, err := Load(dir)
	if err != nil {
		return TriageReport{}, err
	}
	fs, err := loadFiles(dir)
	if err != nil {
		return TriageReport{}, err
	}
	if !fs.has(InboxFile) {
		return TriageReport{}, fmt.Errorf("%s not found in %s", InboxFile, dir)
	}
	rep := TriageReport{Items: []TriageItem{}}
	if m := reNextID.FindStringSubmatch(fs.text(InboxFile)); m != nil {
		rep.NextFreeID = "l" + m[1]
	}
	secs := inboxSections(fs.header[InboxFile], fs.blocks[InboxFile])
	for _, l := range v.Lessons {
		if l.File != InboxFile {
			continue
		}
		it := TriageItem{ID: l.ID, Title: l.Title, Topics: l.Topics, Repo: l.Repo, Date: l.Date, Section: secs[l.ID],
			Used: l.Used, Suggested: suggestFile(v, l.Topics), Duplicates: []Match{}}
		if d := v.Search(l.Title+" "+l.Avoid, SearchOptions{IncludeArchive: true, Exclude: l.ID, Limit: 5, MinScore: threshold}); d != nil {
			it.Duplicates = d
		}
		rep.Items = append(rep.Items, it)
	}
	return rep, nil
}

// PlanStep is one triage decision: "move" an Inbox lesson to the end of a
// topic File, or "merge" it into Target as an Also seen line (Note defaults to
// the lesson's title; Ref and Date default to its section and tag line).
type PlanStep struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	File   string `json:"file,omitempty"`
	Target string `json:"target,omitempty"`
	Note   string `json:"note,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Date   string `json:"date,omitempty"`
}

// TriageDone is one applied step.
type TriageDone struct {
	ID     string  `json:"id"`
	Action string  `json:"action"`
	File   string  `json:"file"`
	Line   string  `json:"line,omitempty"`
	Used   *Change `json:"used,omitempty"`
}

// TriageResult says what TriageApply changed.
type TriageResult struct {
	Applied         []TriageDone `json:"applied"`
	DroppedSections []string     `json:"dropped_sections"`
}

// TriageApply applies a plan all or nothing: every step is checked before any
// file is written. A move appends the lesson to the end of its topic file; a
// merge adds an Also seen line to the target, raises its Used by one plus the
// Inbox lesson's Used (and Misled by its Misled), and drops the Inbox lesson.
// Inbox sections the plan empties are removed; the Inbox header and its Next
// free id stay. The Inbox is written last.
func TriageApply(dir string, plan []PlanStep) (TriageResult, error) {
	if len(plan) == 0 {
		return TriageResult{}, errors.New("empty plan")
	}
	unlock, err := lock(dir)
	if err != nil {
		return TriageResult{}, err
	}
	defer unlock()
	fs, err := loadFiles(dir)
	if err != nil {
		return TriageResult{}, err
	}
	if !fs.has(InboxFile) {
		return TriageResult{}, fmt.Errorf("%s not found in %s", InboxFile, dir)
	}
	secs := inboxSections(fs.header[InboxFile], fs.blocks[InboxFile])
	sources := map[string]bool{}
	merged := map[string]bool{}
	src := map[string]int{} // id -> Inbox block index
	var missing []string
	for i, st := range plan {
		where := fmt.Sprintf("plan step %d (%s)", i+1, st.ID)
		if st.ID == "" {
			return TriageResult{}, fmt.Errorf("plan step %d: id is required", i+1)
		}
		if sources[st.ID] {
			return TriageResult{}, fmt.Errorf("%s: id listed twice", where)
		}
		sources[st.ID] = true
		locs := fs.find(st.ID)
		var inInbox []loc
		var elsewhere []string
		for _, l := range locs {
			if l.file == InboxFile {
				inInbox = append(inInbox, l)
			} else {
				elsewhere = append(elsewhere, l.file)
			}
		}
		if len(locs) == 0 {
			missing = append(missing, st.ID)
			continue
		}
		if len(inInbox) != 1 {
			return TriageResult{}, fmt.Errorf("%s: want exactly one Inbox lesson with this id, found %d", where, len(inInbox))
		}
		if len(elsewhere) > 0 {
			return TriageResult{}, fmt.Errorf("%s: the id is also used in %s; ids must be unique", where, strings.Join(elsewhere, ", "))
		}
		src[st.ID] = inInbox[0].idx
		switch st.Action {
		case "move":
			if st.Target != "" || st.Note != "" || st.Ref != "" || st.Date != "" {
				return TriageResult{}, fmt.Errorf("%s: move takes only file", where)
			}
			if st.File == "" || filepath.Base(st.File) != st.File || !strings.HasSuffix(st.File, ".md") || !isTopicFile(st.File) {
				return TriageResult{}, fmt.Errorf("%s: file %q must be a topic file name such as \"Concurrency.md\"", where, st.File)
			}
			if !fs.has(st.File) {
				return TriageResult{}, fmt.Errorf("%s: topic file %q does not exist; create it first", where, st.File)
			}
		case "merge":
			if st.File != "" {
				return TriageResult{}, fmt.Errorf("%s: merge takes target, not file", where)
			}
			merged[st.ID] = true
			for _, f := range []struct{ name, v string }{{"note", st.Note}, {"ref", st.Ref}} {
				if strings.ContainsAny(f.v, "\r\n") {
					return TriageResult{}, fmt.Errorf("%s: %s must be a single line", where, f.name)
				}
			}
			if st.Date != "" {
				if err := checkDate(st.Date); err != nil {
					return TriageResult{}, fmt.Errorf("%s: %w", where, err)
				}
			}
		default:
			return TriageResult{}, fmt.Errorf("%s: action must be \"move\" or \"merge\", not %q", where, st.Action)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return TriageResult{}, &NotFoundError{IDs: missing}
	}

	// Merges first: they edit targets in place (a target may be an Inbox lesson
	// that a later move carries along with its new line).
	var res TriageResult
	for i, st := range plan {
		if st.Action != "merge" {
			continue
		}
		where := fmt.Sprintf("plan step %d (%s)", i+1, st.ID)
		if st.Target == st.ID || st.Target == "" {
			return TriageResult{}, fmt.Errorf("%s: merge needs a target other than itself", where)
		}
		if merged[st.Target] {
			return TriageResult{}, fmt.Errorf("%s: target %s is itself merged by this plan", where, st.Target)
		}
		at, err := fs.findOne(st.Target)
		if err != nil {
			return TriageResult{}, fmt.Errorf("%s: target: %w", where, err)
		}
		if !isTopicFile(at.file) && at.file != InboxFile {
			return TriageResult{}, fmt.Errorf("%s: target %s is in %s; merge into a live lesson", where, st.Target, at.file)
		}
		srcOwn, _ := splitTail(fs.blocks[InboxFile][src[st.ID]])
		l := parseLesson(InboxFile, srcOwn)
		date, ref, note := st.Date, st.Ref, strings.TrimSpace(st.Note)
		if date == "" {
			date = l.Date
		}
		if date == "" {
			return TriageResult{}, fmt.Errorf("%s: the lesson has no date; give one in the plan", where)
		}
		if ref == "" {
			if m := reSection.FindStringSubmatch(secs[st.ID]); m != nil {
				ref = m[3]
			}
		}
		if note == "" {
			note = l.Title
		}
		repo := l.Repo
		if repo == "" {
			return TriageResult{}, fmt.Errorf("%s: the lesson has no repo in its tag line", where)
		}
		line := alsoSeenLine(date, repo, strings.TrimSpace(ref), note)
		own, tail := splitTail(fs.blocks[at.file][at.idx])
		if own, err = insertAlsoSeen(own, line); err != nil {
			return TriageResult{}, fmt.Errorf("%s: %w", where, err)
		}
		nb, from, to, err := bumpBlock(own, "Used", 1+l.Used)
		if err != nil {
			return TriageResult{}, fmt.Errorf("%s: %w", where, err)
		}
		own = nb
		if l.Misled > 0 {
			if own, _, _, err = bumpBlock(own, "Misled", l.Misled); err != nil {
				return TriageResult{}, fmt.Errorf("%s: %w", where, err)
			}
		}
		fs.blocks[at.file][at.idx] = own + tail
		fs.dirty[at.file] = true
		res.Applied = append(res.Applied, TriageDone{ID: st.ID, Action: "merge", File: at.file,
			Line: strings.TrimSuffix(line, "\n"), Used: &Change{ID: st.Target, File: at.file, Field: "Used", From: from, To: to}})
	}
	before := sections(fs.text(InboxFile))
	for _, st := range plan {
		i := src[st.ID]
		own, tail := splitTail(fs.blocks[InboxFile][i])
		if st.Action == "move" {
			fs.header[st.File] = appendMarkdownBlock(fs.text(st.File), trimNewlines(own))
			fs.blocks[st.File] = nil
			fs.dirty[st.File] = true
			res.Applied = append(res.Applied, TriageDone{ID: st.ID, Action: "move", File: st.File})
		}
		fs.blocks[InboxFile][i] = tail
	}
	fs.dirty[InboxFile] = true
	inbox := fs.text(InboxFile)
	after := sections(inbox)
	res.DroppedSections = []string{}
	if len(before) == len(after) {
		for k := len(after) - 1; k >= 0; k-- {
			if after[k].empty && !before[k].empty {
				inbox = inbox[:after[k].start] + inbox[after[k].end:]
				res.DroppedSections = append([]string{after[k].heading}, res.DroppedSections...)
			}
		}
	}
	fs.header[InboxFile], fs.blocks[InboxFile] = inbox, nil
	return res, fs.write(InboxFile)
}
