package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TysonLabs/agentctl/internal/flow/pr"
)

func TestPROpenUsageErrors(t *testing.T) {
	t.Setenv("AGENTFLOW_GH", "/nonexistent/gh") // never run: each case fails first
	dir := t.TempDir()
	body := filepath.Join(dir, "body.md")
	if err := os.WriteFile(body, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(bad, []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.md")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), pr.MaxBodyRunes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"pr", "open", "--body", "b"}, "--title is required"},
		{[]string{"pr", "open", "--title", "t"}, "a body is required"},
		{[]string{"pr", "open", "--title", "t", "--body", "b", "--body-file", body}, "mutually exclusive"},
		{[]string{"pr", "open", "--title", "t", "--body-file", filepath.Join(dir, "missing")}, "--body-file"},
		{[]string{"pr", "open", "--title", "t", "--body-file", bad}, "not UTF-8"},
		{[]string{"pr", "open", "--title", "t", "--body-file", big}, "GitHub allows"},
		{[]string{"pr", "open", "--title", "a\nb", "--body", "b"}, "one line"},
		{[]string{"pr", "open", "--title", "t", "--body", "b", "--base", "-x"}, "plain branch name"},
		{[]string{"pr", "open", "--title", "t", "--body", "b", "--base", "a..b"}, "plain branch name"},
		{[]string{"pr", "open", "--title", "t", "--body", "b", "--no-review-request", "--reviewer-mention", "x"}, "mutually exclusive"},
		{[]string{"pr", "open", "--title", "t", "--body", "b", "--reviewer-mention", " "}, "is empty"},
		{[]string{"pr", "open", "--title", "t", "--body", "b", "--dir", filepath.Join(dir, "nope")}, "not a directory"},
		{[]string{"pr", "open", "--title", "t", "--body", "b", "--repo", "nope"}, "not OWNER/NAME"},
		{[]string{"pr", "open", "--title", "t", "--body", "b", "--repo", "o/.."}, "not OWNER/NAME"},
		{[]string{"pr", "open", "--title", "t", "--body", "b", "extra"}, "unexpected argument"},
		{[]string{"pr", "open", "--force"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if code := Run(c.args, &out, &errb); code != 1 || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%v: exit %d, stderr %q; want 1 and %q", c.args, code, errb.String(), c.want)
		}
	}
}

func TestPROpenHelp(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"pr", "open", "--help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "agentflow pr open --title") {
		t.Errorf("exit %d, stdout %q", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"pr", "--help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "agentflow pr open") {
		t.Errorf("pr --help does not list pr open: %q", out.String())
	}
}

func TestPROpenExitCodesCoverEveryStatus(t *testing.T) {
	all := []pr.OpenStatus{pr.OpenCreated, pr.OpenExisting, pr.OpenRetargeted, pr.OpenRefused,
		pr.OpenError, pr.OpenUnverified, pr.OpenInterrupted}
	for _, s := range all {
		if _, ok := prOpenExitCodes[s]; !ok {
			t.Errorf("no exit code for %s", s)
		}
	}
	if len(prOpenExitCodes) != len(all) {
		t.Errorf("prOpenExitCodes has %d entries, want %d", len(prOpenExitCodes), len(all))
	}
	for s, want := range map[pr.OpenStatus]int{pr.OpenRefused: 2, pr.OpenError: 3, pr.OpenUnverified: 4} {
		if prOpenExitCodes[s] != want {
			t.Errorf("%s exits %d, want %d", s, prOpenExitCodes[s], want)
		}
	}
}
