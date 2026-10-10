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
// project -> tree -> step -> run hash -> receipt. The tree key is the work
// tree's content hash, so every worktree of the repo shares it.
type receiptFile struct {
	Version  int                                                 `json:"version"`
	Projects map[string]map[string]map[string]map[string]Receipt `json:"projects"`
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
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyReceipts(), nil
	}
	if err != nil {
		return nil, err
	}
	rf := &receiptFile{}
	if err := json.Unmarshal(b, rf); err != nil || !validReceipts(rf) {
		return nil, fmt.Errorf("corrupt receipt file %s (remove it to start over)", path)
	}
	return rf, nil
}

func emptyReceipts() *receiptFile {
	return &receiptFile{Version: 1, Projects: map[string]map[string]map[string]map[string]Receipt{}}
}

func validReceipts(rf *receiptFile) bool {
	if rf.Version != 1 || rf.Projects == nil {
		return false
	}
	for project, trees := range rf.Projects {
		if project == "" || trees == nil {
			return false
		}
		for tree, steps := range trees {
			if !treeHashRe.MatchString(tree) || steps == nil {
				return false
			}
			for step, runs := range steps {
				if step == "" || runs == nil {
					return false
				}
				for hash, r := range runs {
					if len(hash) != sha256.Size*2 || hash != r.RunHash {
						return false
					}
					if _, err := hex.DecodeString(hash); err != nil || r.At.IsZero() || r.Secs < 0 {
						return false
					}
					if r.Status != StepPassed && r.Status != StepFailed && r.Status != StepTimeout {
						return false
					}
					if r.OK != (r.Status == StepPassed) {
						return false
					}
				}
			}
		}
	}
	return true
}

// lookup returns the receipt for project/tree/step.
func (rf *receiptFile) lookup(project, tree, step, hash string) (Receipt, bool) {
	r, ok := rf.Projects[project][tree][step][hash]
	return r, ok
}

func (rf *receiptFile) latest(project, tree, step string) (Receipt, bool) {
	var latest Receipt
	found := false
	for _, r := range rf.Projects[project][tree][step] {
		if !found || r.At.After(latest.At) {
			latest, found = r, true
		}
	}
	return latest, found
}

// passed reports a passing receipt for this exact run string.
func (rf *receiptFile) passed(project, tree, step, run string) bool {
	r, ok := rf.lookup(project, tree, step, runHash(run))
	return ok && r.OK
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
		rf = emptyReceipts()
	}
	trees := rf.Projects[project]
	if trees == nil {
		trees = map[string]map[string]map[string]Receipt{}
		rf.Projects[project] = trees
	}
	if trees[tree] == nil {
		trees[tree] = map[string]map[string]Receipt{}
	}
	if trees[tree][step] == nil {
		trees[tree][step] = map[string]Receipt{}
	}
	trees[tree][step][r.RunHash] = r
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
func prune(trees map[string]map[string]map[string]Receipt, keep string) {
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
		for _, runs := range steps {
			for _, r := range runs {
				if r.At.After(latest) {
					latest = r.At
				}
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
