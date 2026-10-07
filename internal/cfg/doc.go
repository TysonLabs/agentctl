// Package cfg is agentcfg's core: it edits services.toml and writes tokens
// to the Keychain. agentctl and agentflow must never import it (see
// internal/flow/boundary_test.go): agentctl stays read-only, and agentflow
// never touches tokens.
//
// Every edit rewrites the whole file from its decoded tree, so tables and
// keys agentcfg does not know (agentflow's [name.announce], say) come through
// unchanged. Comments do not: the file is machine-managed once agentcfg
// writes it.
package cfg

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// header is the first line of every file agentcfg writes.
const header = "# agentctl service registry. Managed by agentcfg; comments are not kept.\n"

// Doc is one read of services.toml: its bytes, their version, and the
// decoded tree that edits change.
type Doc struct {
	Path    string
	Version string // sha256 prefix of the bytes; "none" when the file is missing
	Tree    map[string]any

	hadComments bool
	onSaved     []func() error
}

// OnSaved registers a step that runs after the file is written, still under
// the lock (deleting a Keychain item the file no longer references).
func (d *Doc) OnSaved(f func() error) { d.onSaved = append(d.onSaved, f) }

// VersionOf returns the version string of file content.
func VersionOf(data []byte) string {
	if data == nil {
		return "none"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

func readDoc(path string) (*Doc, []byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Doc{Path: path, Version: VersionOf(nil), Tree: map[string]any{}}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %v", path, err)
	}
	tree := map[string]any{}
	if _, err := toml.Decode(string(data), &tree); err != nil {
		// Never echo the decoder's message: it can quote a token.
		return nil, nil, fmt.Errorf("%s is not valid TOML (content redacted); fix it by hand first", path)
	}
	return &Doc{Path: path, Version: VersionOf(data), Tree: tree, hadComments: hasComments(data)}, data, nil
}

// hasComments reports a full-line comment other than agentcfg's header.
func hasComments(data []byte) bool {
	for line := range strings.SplitSeq(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") && t+"\n" != header {
			return true
		}
	}
	return false
}

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// renderTree writes tree as TOML in a stable order: per service, [name.meta],
// then the env tables (those with a base_url), then any other table. It
// proves the result decodes back to the same tree, or returns an error and
// nothing is written.
func renderTree(tree map[string]any) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(header)
	for _, name := range sortedKeys(tree) {
		svc, ok := tree[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("top-level key %q is not a [%s.<env>] table; fix the file by hand", name, name)
		}
		if !bareKey.MatchString(name) {
			return nil, fmt.Errorf("service name %q needs quoting; agentcfg only writes plain names", name)
		}
		for _, sub := range tableOrder(svc) {
			tbl, ok := svc[sub].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s.%s is not a table; fix the file by hand", name, sub)
			}
			if err := renderTable(&b, []string{name, sub}, tbl); err != nil {
				return nil, err
			}
		}
	}
	out := b.Bytes()
	back := map[string]any{}
	if _, err := toml.Decode(string(out), &back); err != nil {
		return nil, fmt.Errorf("internal error: rendered registry does not parse")
	}
	if !reflect.DeepEqual(back, tree) {
		return nil, fmt.Errorf("internal error: rendered registry does not round-trip; nothing written")
	}
	return out, nil
}

func renderTable(b *bytes.Buffer, path []string, tbl map[string]any) error {
	for _, seg := range path {
		if !bareKey.MatchString(seg) {
			return fmt.Errorf("table name %q needs quoting; agentcfg only writes plain names", strings.Join(path, "."))
		}
	}
	b.WriteString("\n[" + strings.Join(path, ".") + "]\n")
	var subs []string
	for _, k := range keyOrder(tbl) {
		v := tbl[k]
		switch v.(type) {
		case map[string]any:
			subs = append(subs, k)
			continue
		case []map[string]any:
			return fmt.Errorf("[%s] holds an array of tables (%s); agentcfg cannot rewrite that shape", strings.Join(path, "."), k)
		}
		line, err := encodeKV(k, v)
		if err != nil {
			return fmt.Errorf("[%s] %s: %v", strings.Join(path, "."), k, err)
		}
		b.WriteString(line)
	}
	for _, k := range subs {
		if err := renderTable(b, append(append([]string{}, path...), k), tbl[k].(map[string]any)); err != nil {
			return err
		}
	}
	return nil
}

// encodeKV renders one `key = value` line with the TOML encoder, which owns
// quoting and escaping.
func encodeKV(k string, v any) (string, error) {
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(map[string]any{k: v}); err != nil {
		return "", err
	}
	s := b.String()
	if strings.HasPrefix(strings.TrimSpace(s), "[") || strings.Count(s, "\n") != 1 {
		return "", fmt.Errorf("value shape not supported")
	}
	return s, nil
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// tableOrder: meta, env tables, then the rest; each group sorted.
func tableOrder(svc map[string]any) []string {
	var meta, envs, rest []string
	for _, k := range sortedKeys(svc) {
		tbl, _ := svc[k].(map[string]any)
		switch {
		case k == "meta":
			meta = append(meta, k)
		case tbl != nil && tbl["base_url"] != nil:
			envs = append(envs, k)
		default:
			rest = append(rest, k)
		}
	}
	return append(append(meta, envs...), rest...)
}

// keyOrder puts base_url, token and token_ref first, the rest sorted.
func keyOrder(tbl map[string]any) []string {
	var out []string
	for _, k := range []string{"base_url", "token", "token_ref"} {
		if _, ok := tbl[k]; ok {
			out = append(out, k)
		}
	}
	for _, k := range sortedKeys(tbl) {
		if k != "base_url" && k != "token" && k != "token_ref" {
			out = append(out, k)
		}
	}
	return out
}
