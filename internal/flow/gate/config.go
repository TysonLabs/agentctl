// Package gate runs a project's gate: the ordered steps in its [name.gate]
// table in services.toml. It records a receipt for every step in the
// repository's git common dir, keyed by the exact content of the work tree,
// so a rerun on the same tree skips the steps that already passed, and
// `agentflow gate --check` can prove a tree was gated.
package gate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/TysonLabs/agentctl/internal/gatespec"
)

// project is one top-level name in services.toml, as far as the gate needs it.
type project struct {
	name    string
	repo    string // [name.meta] repo, as written
	gate    any    // decoded [name.gate], nil when absent
	hasGate bool
}

// loadProjects reads only the [name.meta] and [name.gate] tables. Env tables
// (and their tokens) and [name.announce] stay undecoded.
func loadProjects(path string) (map[string]project, error) {
	var raw map[string]map[string]toml.Primitive
	md, err := toml.DecodeFile(path, &raw)
	if err != nil {
		var pe toml.ParseError
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("config file not found: %s — add a gate: agentcfg gate <name> --add NAME --run CMD", path)
		case errors.As(err, &pe):
			// Never echo the lexeme: it may be a token.
			return nil, fmt.Errorf("parsing %s: TOML syntax error at line %d (file content redacted)", path, pe.Position.Line)
		default:
			return nil, fmt.Errorf("parsing %s: TOML decode error (file content redacted)", path)
		}
	}
	out := map[string]project{}
	for name, tables := range raw {
		p := project{name: name}
		if prim, ok := tables["meta"]; ok {
			var meta map[string]any
			if err := md.PrimitiveDecode(prim, &meta); err == nil {
				p.repo, _ = meta["repo"].(string)
			}
		}
		if prim, ok := tables["gate"]; ok {
			var g any
			if err := md.PrimitiveDecode(prim, &g); err != nil {
				return nil, fmt.Errorf("[%s.gate] in %s: %v", name, path, err)
			}
			p.gate, p.hasGate = g, true
		}
		out[name] = p
	}
	return out, nil
}

// target is a resolved gate run: which project, its validated steps, and
// the checkout to run them in.
type target struct {
	project string
	spec    gatespec.Spec
	root    string // work tree root (a linked worktree's own root)
	common  string // git common dir, shared by every worktree of the repo
}

// resolve finds the project and the checkout. dirGiven reports whether dir
// came from --dir (otherwise it is the current directory).
func resolve(ctx context.Context, configPath, projectName, dir string, dirGiven bool) (*target, error) {
	projects, err := loadProjects(configPath)
	if err != nil {
		return nil, err
	}
	var p project
	if projectName != "" {
		var ok bool
		if p, ok = projects[projectName]; !ok {
			return nil, fmt.Errorf("no project %q in %s — add its gate: agentcfg gate %s --add NAME --run CMD", projectName, configPath, projectName)
		}
		// Outside any checkout, a named project runs in its own repo.
		if !dirGiven && p.repo != "" {
			if _, _, err := gitDirs(ctx, dir); err != nil {
				dir = expandHome(p.repo)
			}
		}
	}
	root, common, err := gitDirs(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("%s is not inside a git work tree (use --dir): %v", dir, err)
	}
	if projectName == "" {
		var withGate, without []string
		for _, name := range sortedNames(projects) {
			q := projects[name]
			if q.repo == "" || !repoMatches(ctx, q.repo, root, common) {
				continue
			}
			if q.hasGate {
				withGate = append(withGate, name)
			} else {
				without = append(without, name)
			}
		}
		switch {
		case len(withGate) > 1:
			return nil, fmt.Errorf("projects %s all use the repo at %s; pick one with --project", strings.Join(withGate, ", "), root)
		case len(withGate) == 1:
			p = projects[withGate[0]]
		case len(without) > 0:
			return nil, fmt.Errorf("project %s has no [%s.gate] table — add steps: agentcfg gate %s --add NAME --run CMD", without[0], without[0], without[0])
		default:
			return nil, fmt.Errorf("no project in %s has a meta.repo for the checkout at %s — set it: agentcfg meta <name> repo=%s, then agentcfg gate <name> --add NAME --run CMD (or pass --project)", configPath, root, root)
		}
	} else if p.repo != "" && !repoMatches(ctx, p.repo, root, common) {
		return nil, fmt.Errorf("%s is not a checkout of %s's repo (%s); run in that repo or pass --dir", root, p.name, p.repo)
	}
	if !p.hasGate {
		return nil, fmt.Errorf("project %s has no [%s.gate] table — add steps: agentcfg gate %s --add NAME --run CMD", p.name, p.name, p.name)
	}
	spec, err := gatespec.FromTable(p.name, p.gate)
	if err != nil {
		return nil, fmt.Errorf("%v in %s — fix it: agentcfg gate %s", err, configPath, p.name)
	}
	if len(spec.Steps) == 0 {
		return nil, fmt.Errorf("[%s.gate] has no steps — add one: agentcfg gate %s --add NAME --run CMD", p.name, p.name)
	}
	return &target{project: p.name, spec: spec, root: root, common: common}, nil
}

func sortedNames(m map[string]project) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// expandHome expands a leading "~" or "~/".
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// canonical resolves p to an absolute path with no symlinks.
func canonical(p string) (string, error) {
	abs, err := filepath.Abs(expandHome(p))
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// repoMatches reports whether repo (a meta.repo value) is the checkout at
// root or another worktree of the same repository (same git common dir).
func repoMatches(ctx context.Context, repo, root, common string) bool {
	r, err := canonical(repo)
	if err != nil {
		return false
	}
	if r == root {
		return true
	}
	_, c, err := gitDirs(ctx, r)
	return err == nil && c == common
}
