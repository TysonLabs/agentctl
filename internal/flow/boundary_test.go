// Package flow holds agentflow's commands. This test keeps the three binaries
// apart: agentctl must stay a read-only HTTP client, so none of its packages
// may import agentflow or agentcfg code (agentcfg writes the registry and the
// Keychain); agentflow must not reach into agentctl's HTTP client, registry
// or Keychain reader (and with them, its tokens), nor into agentcfg.
package flow

import (
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
	agentctlDirs := []string{".", "internal/cli", "internal/client", "internal/keychain", "internal/registry", "internal/render"}
	for _, d := range agentctlDirs {
		for _, imp := range imports(t, filepath.Join(root, d)) {
			for _, banned := range []string{"/internal/flow", "/internal/cfg", "/cmd/"} {
				if strings.HasPrefix(imp, module+banned) {
					t.Errorf("agentctl package %s imports %s", d, imp)
				}
			}
		}
	}
	err := filepath.WalkDir(filepath.Join(root, "internal", "flow"), func(path string, de os.DirEntry, err error) error {
		if err != nil || !de.IsDir() {
			return err
		}
		for _, imp := range imports(t, path) {
			for _, banned := range []string{"/internal/client", "/internal/registry", "/internal/cli", "/internal/keychain"} {
				if imp == module+banned {
					t.Errorf("agentflow package %s imports agentctl's %s", path, imp)
				}
			}
			if strings.HasPrefix(imp, module+"/internal/cfg") {
				t.Errorf("agentflow package %s imports agentcfg's %s", path, imp)
			}
		}
		return nil
	})
	if err != nil {
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
