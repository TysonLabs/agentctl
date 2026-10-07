// Package flow holds agentflow's commands. This test keeps the three binaries
// apart: agentctl must stay a read-only HTTP client, so none of its packages
// may import agentflow or agentcfg code (agentcfg writes the registry and the
// Keychain); agentflow must not reach into agentctl's HTTP client or
// registry (and with them, its tokens), nor into agentcfg. agentflow reads
// the Keychain only under its own service (TestAgentflowReadsOnlyItsKeychainService).
package flow

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/TysonLabs/agentctl"

func TestImportBoundary(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, violation := range boundaryViolations(t, root, ".", []string{"internal/flow", "internal/cfg", "cmd"}) {
		t.Errorf("agentctl %s", violation)
	}
	flowBanned := []string{"internal/client", "internal/registry", "internal/cli", "internal/cfg"}
	entries := []string{"cmd/agentflow"}
	err := filepath.WalkDir(filepath.Join(root, "internal", "flow"), func(path string, de os.DirEntry, err error) error {
		if err != nil || !de.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries = append(entries, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reported := map[string]bool{}
	for _, entry := range entries {
		for _, violation := range boundaryViolations(t, root, entry, flowBanned) {
			if !reported[violation] {
				t.Errorf("agentflow %s", violation)
				reported[violation] = true
			}
		}
	}
}

// boundaryViolations follows the packages actually built from entry. A
// hand-maintained list of directories can miss a new intermediate package
// that imports a forbidden capability.
func boundaryViolations(t *testing.T, root, entry string, banned []string) []string {
	t.Helper()
	seen := map[string]bool{}
	var violations []string
	var walk func(string)
	walk = func(pkg string) {
		if seen[pkg] {
			return
		}
		seen[pkg] = true
		dir := root
		if pkg != "." {
			dir = filepath.Join(root, filepath.FromSlash(pkg))
		}
		for _, imp := range imports(t, dir) {
			for _, forbidden := range banned {
				prefix := module + "/" + forbidden
				if imp == prefix || strings.HasPrefix(imp, prefix+"/") {
					violations = append(violations, fmt.Sprintf("package %s imports %s", pkg, imp))
				}
			}
			switch {
			case imp == module:
				walk(".")
			case strings.HasPrefix(imp, module+"/"):
				walk(strings.TrimPrefix(imp, module+"/"))
			}
		}
	}
	walk(entry)
	return violations
}

func TestImportBoundaryFollowsTransitiveImports(t *testing.T) {
	root := t.TempDir()
	writeGoFile(t, filepath.Join(root, "main.go"), "package main\nimport _ \""+module+"/internal/bridge\"\n")
	writeGoFile(t, filepath.Join(root, "internal", "bridge", "bridge.go"), "package bridge\nimport _ \""+module+"/internal/cfg\"\n")
	writeGoFile(t, filepath.Join(root, "internal", "cfg", "cfg.go"), "package cfg\n")
	got := boundaryViolations(t, root, ".", []string{"internal/cfg"})
	if len(got) != 1 || !strings.Contains(got[0], "internal/bridge imports "+module+"/internal/cfg") {
		t.Fatalf("violations = %v, want transitive cfg import", got)
	}
}

func writeGoFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// imports lists the import paths of the non-test Go files directly in dir.
func imports(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range f.Imports {
			p, _ := strconv.Unquote(s.Path.Value)
			out = append(out, p)
		}
	}
	return out
}

// agentflow may read the Keychain only for its own credentials: it must
// never name agentctl's token service or call the token reader. (It imports
// internal/keychain for GetFrom(keychain.AgentflowService, ...).)
func TestAgentflowReadsOnlyItsKeychainService(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(filepath.Join(root, "internal", "flow"), func(path string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, banned := range []string{"keychain.Get(", "keychain.Service"} {
			if strings.Contains(string(b), banned) {
				t.Errorf("%s uses %s: agentflow must read only keychain.AgentflowService", path, banned)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
