package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Receipt records one step's outcome on one work tree.
type Receipt struct {
	RunHash string    `json:"run_hash"` // sha256 of the step's run string
	OK      bool      `json:"ok"`
	Status  string    `json:"status"`
	Exit    *int      `json:"exit"`
	Secs    float64   `json:"secs"`
	At      time.Time `json:"at"`
	Head    string    `json:"head,omitempty"` // HEAD when it ran (informational)
}

// receiptFile is <git-common-dir>/agentflow/gate/receipts.json:
// project -> tree -> step -> receipt. The key is the work tree's content
// hash, so every worktree of the repo shares it.
type receiptFile struct {
	Version  int                                      `json:"version"`
	Projects map[string]map[string]map[string]Receipt `json:"projects"`
}

// keepTrees bounds how many trees per project keep receipts.
const keepTrees = 30

func receiptsPath(common string) string {
	return filepath.Join(common, "agentflow", "gate", "receipts.json")
}

func runHash(run string) string {
	sum := sha256.Sum256([]byte(run))
	return hex.EncodeToString(sum[:])
}

// readReceipts reads the receipt file. A missing file is empty.
func readReceipts(path string) (*receiptFile, error) {
	rf := &receiptFile{Version: 1, Projects: map[string]map[string]map[string]Receipt{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return rf, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, rf); err != nil || rf.Projects == nil {
		return nil, fmt.Errorf("corrupt receipt file %s (remove it to start over)", path)
	}
	return rf, nil
}

// lookup returns the receipt for project/tree/step.
func (rf *receiptFile) lookup(project, tree, step string) (Receipt, bool) {
	r, ok := rf.Projects[project][tree][step]
	return r, ok
}

// passed reports a passing receipt for this exact run string.
func (rf *receiptFile) passed(project, tree, step, run string) bool {
	r, ok := rf.lookup(project, tree, step)
	return ok && r.OK && r.RunHash == runHash(run)
}

// recordReceipt adds one receipt under an exclusive lock (read, merge,
// atomic replace), so concurrent gates in sibling worktrees never lose each
// other's receipts. A corrupt file is replaced: it is a cache, and a lost
// receipt only means a step reruns.
func recordReceipt(path, project, tree, step string, r Receipt) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)

	rf, err := readReceipts(path)
	if err != nil {
		rf = &receiptFile{Version: 1, Projects: map[string]map[string]map[string]Receipt{}}
	}
	trees := rf.Projects[project]
	if trees == nil {
		trees = map[string]map[string]Receipt{}
		rf.Projects[project] = trees
	}
	if trees[tree] == nil {
		trees[tree] = map[string]Receipt{}
	}
	trees[tree][step] = r
	prune(trees, tree)

	b, err := json.MarshalIndent(rf, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".receipts-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// prune keeps the keepTrees most recently used trees, always keeping keep.
func prune(trees map[string]map[string]Receipt, keep string) {
	if len(trees) <= keepTrees {
		return
	}
	type aged struct {
		tree string
		at   time.Time
	}
	var all []aged
	for t, steps := range trees {
		var latest time.Time
		for _, r := range steps {
			if r.At.After(latest) {
				latest = r.At
			}
		}
		all = append(all, aged{t, latest})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	for i, a := range all {
		if i >= keepTrees && a.tree != keep {
			delete(trees, a.tree)
		}
	}
}
