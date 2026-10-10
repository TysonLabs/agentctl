package pr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// openFixture is a bare origin with main and development, a seed clone that
// plays other pushers, and the repo under test on a feature branch with one
// commit. git is real (so a push really fast-forwards or is refused); gh is
// the openFake below.
type openFixture struct {
	t                  *testing.T
	origin, seed, repo string
	gitCalls           [][]string
	mu                 sync.Mutex
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@e", "-c", "user.name=t", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, body, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", name)
	runGit(t, dir, "commit", "-qm", msg)
}

func newOpenFixture(t *testing.T) *openFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &openFixture{t: t, origin: filepath.Join(root, "origin.git"), seed: filepath.Join(root, "seed"), repo: filepath.Join(root, "repo")}
	runGit(t, root, "init", "-q", "--bare", "-b", "main", f.origin)
	runGit(t, root, "clone", "-q", f.origin, f.seed)
	commitFile(t, f.seed, "a.txt", "a\n", "base")
	runGit(t, f.seed, "push", "-q", "origin", "main")
	runGit(t, f.seed, "push", "-q", "origin", "main:development")
	runGit(t, root, "clone", "-q", f.origin, f.repo)
	runGit(t, f.repo, "checkout", "-qb", "feat/x")
	commitFile(t, f.repo, "b.txt", "b\n", "feature")
	return f
}

func (f *openFixture) git() Git {
	inner := GitCLI("git", f.repo, 30*time.Second)
	return func(ctx context.Context, args ...string) (string, error) {
		f.mu.Lock()
		f.gitCalls = append(f.gitCalls, args)
		f.mu.Unlock()
		return inner(ctx, args...)
	}
}

func (f *openFixture) head() string { return runGit(f.t, f.repo, "rev-parse", "HEAD") }

// originSHA is what origin's branch points at, or "".
func (f *openFixture) originSHA(branch string) string {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = f.origin
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func (f *openFixture) pushes() [][]string {
	var p [][]string
	for _, c := range f.gitCalls {
		if c[0] == "push" {
			p = append(p, c)
		}
	}
	return p
}

// openFake plays GitHub for pr open. It reads the pushed head from the
// fixture's origin, so the read-back sees what was really pushed.
type openFake struct {
	f            *openFixture
	mu           sync.Mutex
	def          string // default branch; default main
	existing     []map[string]any
	created      map[string]any // the PR as created: number, base, body, title, draft
	bodyOverride *string        // read-back body instead of the one sent
	comments     []string
	createErr    error
	calls        [][]string
}

func (g *openFake) gh(_ context.Context, args ...string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, args)
	field := func(key string) string {
		for i, a := range args {
			if (a == "-f" || a == "-F") && i+1 < len(args) && strings.HasPrefix(args[i+1], key+"=") {
				return strings.TrimPrefix(args[i+1], key+"=")
			}
		}
		return ""
	}
	switch {
	case args[0] == "repo" && args[1] == "view":
		def := g.def
		if def == "" {
			def = "main"
		}
		return json.Marshal(map[string]any{"defaultBranchRef": map[string]any{"name": def}})
	case args[0] == "pr" && args[1] == "list":
		if g.existing == nil {
			return []byte("[]"), nil
		}
		return json.Marshal(g.existing)
	case args[0] == "api" && args[3] == "repos/o/r/pulls":
		if g.createErr != nil {
			return nil, g.createErr
		}
		g.created = map[string]any{"number": 7, "base": field("base"), "body": field("body"), "title": field("title"), "head": field("head")}
		return json.Marshal(map[string]any{"number": 7, "html_url": "https://github.com/o/r/pull/7"})
	case args[0] == "api" && args[1] == "-X" && args[2] == "PATCH":
		n, _ := strconv.Atoi(args[3][strings.LastIndex(args[3], "/")+1:])
		for _, p := range g.existing {
			if p["number"] == n {
				p["baseRefName"] = field("base")
			}
		}
		return []byte(`{}`), nil
	case args[0] == "pr" && args[1] == "view":
		n, _ := strconv.Atoi(args[2])
		doc := map[string]any{"number": n, "url": "https://github.com/o/r/pull/" + args[2], "state": "OPEN",
			"headRefOid": strings.ToUpper(g.f.originSHA("feat/x")), "body": "existing body"}
		if g.created != nil && n == 7 {
			doc["baseRefName"], doc["body"] = g.created["base"], g.created["body"]
		}
		for _, p := range g.existing {
			if p["number"] == n {
				doc["baseRefName"] = p["baseRefName"]
			}
		}
		if g.bodyOverride != nil {
			doc["body"] = *g.bodyOverride
		}
		return json.Marshal(doc)
	case args[0] == "api" && args[1] == "--paginate":
		page := []map[string]any{}
		for _, c := range g.comments {
			page = append(page, map[string]any{"body": c})
		}
		return json.Marshal([][]map[string]any{page})
	case args[0] == "api" && args[1] == "-X" && args[2] == "POST" && strings.HasSuffix(args[3], "/comments"):
		g.comments = append(g.comments, field("body"))
		return []byte(`{}`), nil
	}
	return nil, errors.New("openFake: unexpected gh call: " + strings.Join(args, " "))
}

func (g *openFake) count(prefix ...string) int {
	n := 0
	for _, c := range g.calls {
		if len(c) >= len(prefix) && strings.Join(c[:len(prefix)], "\x00") == strings.Join(prefix, "\x00") {
			n++
		}
	}
	return n
}

func (f *openFixture) opts(g *openFake) OpenOptions {
	return OpenOptions{Repo: "o/r", Title: "Add b", Body: "What changed.\n\nHow verified.\n",
		ReviewMention: DefaultReviewMention, ConfirmWait: 200 * time.Millisecond, ConfirmInterval: 10 * time.Millisecond,
		GH: g.gh, Git: f.git()}
}

func existingPR7(base string) []map[string]any {
	return []map[string]any{{"number": 7, "url": "https://github.com/o/r/pull/7", "baseRefName": base,
		"headRefName": "feat/x", "isCrossRepository": false, "headRepositoryOwner": map[string]any{"login": "o"}}}
}

func assertNoForcePush(t *testing.T, f *openFixture) {
	t.Helper()
	for _, p := range f.pushes() {
		for _, a := range p {
			if a == "-f" || strings.HasPrefix(a, "--force") || strings.HasPrefix(a, "+") || strings.Contains(a, ":+") {
				t.Errorf("forced push: %v", p)
			}
		}
	}
}

func TestOpenCreatesAgainstDerivedDefaultBranch(t *testing.T) {
	f := newOpenFixture(t)
	g := &openFake{f: f, def: "main"}
	res := Open(context.Background(), f.opts(g))
	if res.Status != OpenCreated {
		t.Fatalf("status %s, reasons %v, error %q", res.Status, res.Reasons, res.Error)
	}
	if res.Base != "main" || res.DefaultBranch != "main" || res.Number != 7 || res.Head != "feat/x" || res.HeadSHA != f.head() {
		t.Errorf("result %+v", res)
	}
	if got := f.originSHA("feat/x"); got != f.head() {
		t.Errorf("origin feat/x = %q, want the pushed head %s", got, f.head())
	}
	if g.created["base"] != "main" || g.created["head"] != "feat/x" {
		t.Errorf("created %v", g.created)
	}
	if res.ReviewRequested || g.count("api", "-X", "POST") != 1 { // the create only, no comment on the default base
		t.Errorf("review requested on the default base: %v, calls %v", res.ReviewRequested, g.calls)
	}
	if up := runGit(t, f.repo, "rev-parse", "--abbrev-ref", "feat/x@{upstream}"); up != "origin/feat/x" {
		t.Errorf("upstream %q, want origin/feat/x", up)
	}
	assertNoForcePush(t, f)
}

func TestOpenDerivesANonMainDefaultBranch(t *testing.T) {
	f := newOpenFixture(t)
	g := &openFake{f: f, def: "development"}
	res := Open(context.Background(), f.opts(g))
	if res.Status != OpenCreated || res.Base != "development" || g.created["base"] != "development" {
		t.Fatalf("status %s base %q created %v (%v %q)", res.Status, res.Base, g.created, res.Reasons, res.Error)
	}
	if res.ReviewRequested {
		t.Error("the default branch needs no review request")
	}
}

// Every string goes to gh api with -f (raw), so a numeric title stays a
// string and a body starting with @ is not read as a file; -F only for draft.
func TestOpenPassesStringsRaw(t *testing.T) {
	f := newOpenFixture(t)
	g := &openFake{f: f}
	o := f.opts(g)
	o.Title, o.Body, o.Draft = "123", "@/etc/passwd", true
	res := Open(context.Background(), o)
	if res.Status != OpenCreated {
		t.Fatalf("status %s %v %q", res.Status, res.Reasons, res.Error)
	}
	var create []string
	for _, c := range g.calls {
		if len(c) > 3 && c[3] == "repos/o/r/pulls" {
			create = c
		}
	}
	for i, a := range create {
		if a != "-F" && a != "-f" {
			continue
		}
		v := create[i+1]
		if strings.HasPrefix(v, "draft=") != (a == "-F") {
			t.Errorf("%s %s: want -f for strings, -F only for draft", a, v)
		}
	}
	if g.created["title"] != "123" || g.created["body"] != "@/etc/passwd" {
		t.Errorf("created %v", g.created)
	}
	if !strings.Contains(res.Next, "draft") {
		t.Errorf("next %q does not mention the draft", res.Next)
	}
}

func TestOpenRefusesWithEveryReasonAndPushesNothing(t *testing.T) {
	f := newOpenFixture(t)
	runGit(t, f.repo, "reset", "-q", "--hard", "origin/main") // no commits beyond the base
	if err := os.WriteFile(filepath.Join(f.repo, "a.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &openFake{f: f}
	o := f.opts(g)
	o.Title, o.Body = " ", "\n"
	res := Open(context.Background(), o)
	if res.Status != OpenRefused {
		t.Fatalf("status %s, want refused", res.Status)
	}
	all := strings.Join(res.Reasons, "\n")
	for _, want := range []string{"uncommitted changes", "no commits that are not in main", "title is empty", "body is empty"} {
		if !strings.Contains(all, want) {
			t.Errorf("reasons %q lack %q", all, want)
		}
	}
	if len(f.pushes()) != 0 || f.originSHA("feat/x") != "" {
		t.Errorf("pushed on refusal: %v", f.pushes())
	}
	if g.count("api") != 0 {
		t.Errorf("gh api called on refusal: %v", g.calls)
	}
}

func TestOpenRefusesBaseAndLongLivedBranches(t *testing.T) {
	cases := []struct {
		branch, base, want string
	}{
		{"main", "", "is the base"},
		{"development", "", "long-lived"},
		{"feat/x", "feat/x", "is the base"},
		{"Production", "", "long-lived"},
	}
	for _, c := range cases {
		f := newOpenFixture(t)
		if c.branch != "feat/x" {
			runGit(t, f.repo, "checkout", "-qB", c.branch)
		}
		g := &openFake{f: f}
		o := f.opts(g)
		o.Base = c.base
		res := Open(context.Background(), o)
		if res.Status != OpenRefused || !strings.Contains(strings.Join(res.Reasons, "\n"), c.want) {
			t.Errorf("%s on base %q: status %s reasons %v; want refused %q", c.branch, c.base, res.Status, res.Reasons, c.want)
		}
		if len(f.pushes()) != 0 {
			t.Errorf("%s: pushed on refusal", c.branch)
		}
	}
}

func TestOpenRefusesDetachedHead(t *testing.T) {
	f := newOpenFixture(t)
	runGit(t, f.repo, "checkout", "-q", "--detach")
	res := Open(context.Background(), f.opts(&openFake{f: f}))
	if res.Status != OpenRefused || !strings.Contains(strings.Join(res.Reasons, ""), "detached") {
		t.Fatalf("status %s reasons %v", res.Status, res.Reasons)
	}
}

func TestOpenRefusesMissingBase(t *testing.T) {
	f := newOpenFixture(t)
	o := f.opts(&openFake{f: f})
	o.Base = "release"
	res := Open(context.Background(), o)
	if res.Status != OpenRefused || !strings.Contains(strings.Join(res.Reasons, ""), `"release" does not exist on origin`) {
		t.Fatalf("status %s reasons %v", res.Status, res.Reasons)
	}
}

// A branch that is behind (or diverged from) origin is refused, never
// force-pushed over.
func TestOpenRefusesBranchBehindOrigin(t *testing.T) {
	f := newOpenFixture(t)
	runGit(t, f.repo, "push", "-q", "origin", "feat/x")
	runGit(t, f.seed, "fetch", "-q", "origin")
	runGit(t, f.seed, "checkout", "-qb", "feat/x", "origin/feat/x")
	commitFile(t, f.seed, "c.txt", "c\n", "someone else's commit")
	runGit(t, f.seed, "push", "-q", "origin", "feat/x")
	theirs := f.originSHA("feat/x")
	commitFile(t, f.repo, "d.txt", "d\n", "my diverging commit")

	res := Open(context.Background(), f.opts(&openFake{f: f}))
	if res.Status != OpenRefused || !strings.Contains(strings.Join(res.Reasons, ""), "behind or diverged") {
		t.Fatalf("status %s reasons %v", res.Status, res.Reasons)
	}
	if len(f.pushes()) != 0 || f.originSHA("feat/x") != theirs {
		t.Errorf("origin changed: pushes %v", f.pushes())
	}
}

// A branch already on origin and an ancestor of HEAD fast-forwards.
func TestOpenFastForwardsPushedBranch(t *testing.T) {
	f := newOpenFixture(t)
	runGit(t, f.repo, "push", "-q", "origin", "feat/x")
	commitFile(t, f.repo, "e.txt", "e\n", "more")
	g := &openFake{f: f}
	res := Open(context.Background(), f.opts(g))
	if res.Status != OpenCreated || f.originSHA("feat/x") != f.head() {
		t.Fatalf("status %s %v %q; origin %s head %s", res.Status, res.Reasons, res.Error, f.originSHA("feat/x"), f.head())
	}
	assertNoForcePush(t, f)
}

func TestOpenReturnsExistingPR(t *testing.T) {
	f := newOpenFixture(t)
	g := &openFake{f: f, existing: existingPR7("main")}
	res := Open(context.Background(), f.opts(g))
	if res.Status != OpenExisting || res.Number != 7 || res.Base != "main" || res.HeadSHA != f.head() {
		t.Fatalf("result %+v", res)
	}
	if g.created != nil {
		t.Error("created a second PR")
	}
	if f.originSHA("feat/x") != f.head() {
		t.Error("the new commits were not pushed to the existing PR's branch")
	}
}

// A fork's PR from a same-named branch is not this branch's PR.
func TestOpenIgnoresForkPRWithSameBranchName(t *testing.T) {
	f := newOpenFixture(t)
	fork := existingPR7("development")
	fork[0]["number"] = 9
	fork[0]["isCrossRepository"] = true
	fork[0]["headRepositoryOwner"] = map[string]any{"login": "someone"}
	g := &openFake{f: f, existing: fork}
	res := Open(context.Background(), f.opts(g))
	if res.Status != OpenCreated {
		t.Fatalf("status %s %v %q", res.Status, res.Reasons, res.Error)
	}
}

func TestOpenExistingPRWithOtherBaseNeedsRetarget(t *testing.T) {
	f := newOpenFixture(t)
	g := &openFake{f: f, existing: existingPR7("development")}
	res := Open(context.Background(), f.opts(g))
	if res.Status != OpenRefused || res.Number != 7 || !strings.Contains(strings.Join(res.Reasons, ""), "targets development, not main") {
		t.Fatalf("status %s reasons %v", res.Status, res.Reasons)
	}
	if len(f.pushes()) != 0 || g.count("api", "-X", "PATCH") != 0 {
		t.Error("changed something on refusal")
	}

	o := f.opts(g)
	o.Retarget = true
	res = Open(context.Background(), o)
	if res.Status != OpenRetargeted || res.Base != "main" || res.PreviousBase != "development" {
		t.Fatalf("retarget: %+v", res)
	}
	var patch []string
	for _, c := range g.calls {
		if len(c) > 2 && c[2] == "PATCH" {
			patch = c
		}
	}
	if strings.Join(patch, " ") != "api -X PATCH repos/o/r/pulls/7 -f base=main" {
		t.Errorf("patch call %v", patch)
	}
}

// On a non-default base CodeRabbit skips the PR, so the review request is
// commented, and a rerun does not comment it again.
func TestOpenRequestsReviewOnNonDefaultBaseOnce(t *testing.T) {
	f := newOpenFixture(t)
	g := &openFake{f: f}
	o := f.opts(g)
	o.Base = "development"
	res := Open(context.Background(), o)
	if res.Status != OpenCreated || !res.ReviewRequested {
		t.Fatalf("result %+v", res)
	}
	if len(g.comments) != 1 || g.comments[0] != "@coderabbitai review" {
		t.Fatalf("comments %q", g.comments)
	}
	g.existing = existingPR7("development")
	res = Open(context.Background(), o)
	if res.Status != OpenExisting || !res.ReviewRequested || len(g.comments) != 1 {
		t.Fatalf("rerun: status %s requested %v comments %q", res.Status, res.ReviewRequested, g.comments)
	}
}

func TestOpenReviewRequestCanBeDisabledOrCustom(t *testing.T) {
	f := newOpenFixture(t)
	g := &openFake{f: f}
	o := f.opts(g)
	o.Base, o.ReviewMention = "development", ""
	if res := Open(context.Background(), o); res.Status != OpenCreated || res.ReviewRequested || len(g.comments) != 0 {
		t.Fatalf("disabled: %+v comments %q", res, g.comments)
	}

	f = newOpenFixture(t)
	g = &openFake{f: f}
	o = f.opts(g)
	o.Base, o.ReviewMention = "development", "@bot please review"
	if res := Open(context.Background(), o); !res.ReviewRequested || len(g.comments) != 1 || g.comments[0] != "@bot please review" {
		t.Fatalf("custom: %+v comments %q", res, g.comments)
	}
}

// The known failure: a pipe that fails leaves the body empty on GitHub.
func TestOpenBodyReadBackMismatchIsUnverified(t *testing.T) {
	f := newOpenFixture(t)
	empty := ""
	g := &openFake{f: f, bodyOverride: &empty}
	res := Open(context.Background(), f.opts(g))
	if res.Status != OpenUnverified || res.Number != 7 || !strings.Contains(res.Error, "body has 0 characters") {
		t.Fatalf("result %+v", res)
	}
}

func TestOpenBodyReadBackToleratesLineEndings(t *testing.T) {
	f := newOpenFixture(t)
	crlf := "What changed.\r\n\r\nHow verified.\r\n"
	g := &openFake{f: f, bodyOverride: &crlf}
	if res := Open(context.Background(), f.opts(g)); res.Status != OpenCreated {
		t.Fatalf("result %+v", res)
	}
}

func TestOpenCreateErrorAfterPushIsError(t *testing.T) {
	f := newOpenFixture(t)
	g := &openFake{f: f, createErr: errors.New("gh api: HTTP 422")}
	res := Open(context.Background(), f.opts(g))
	if res.Status != OpenError || !strings.Contains(res.Error, "422") || !strings.Contains(res.Next, "pushed") {
		t.Fatalf("result %+v", res)
	}
}

func TestOpenRefusesOriginOfAnotherRepo(t *testing.T) {
	f := newOpenFixture(t)
	o := f.opts(&openFake{f: f})
	real := o.Git
	o.Git = func(ctx context.Context, args ...string) (string, error) {
		if strings.Join(args, " ") == "remote get-url origin" {
			return "git@github.com:other/r.git\n", nil
		}
		return real(ctx, args...)
	}
	res := Open(context.Background(), o)
	if res.Status != OpenRefused || !strings.Contains(strings.Join(res.Reasons, ""), "origin remote is other/r, not o/r") {
		t.Fatalf("status %s reasons %v", res.Status, res.Reasons)
	}
	if len(f.pushes()) != 0 {
		t.Error("pushed on refusal")
	}
}

func TestOriginRepoParsesHostedRemotes(t *testing.T) {
	cases := map[string]string{
		"git@github.com:Acme/svc.git":         "Acme/svc",
		"https://github.com/Acme/svc":         "Acme/svc",
		"https://x@github.com/Acme/svc.git/":  "Acme/svc",
		"ssh://git@github.com/Acme/svc.git":   "Acme/svc",
		"github-work:Acme/svc.git":            "Acme/svc",
		"/tmp/origin.git":                     "",
		"file:///tmp/Acme/svc.git":            "",
		"https://ghe.example.com/a/b/svc.git": "",
	}
	for url, want := range cases {
		git := func(context.Context, ...string) (string, error) { return url + "\n", nil }
		got, ok := originRepo(context.Background(), git)
		if got != want || ok != (want != "") {
			t.Errorf("%s: got %q %v, want %q", url, got, ok, want)
		}
	}
}
