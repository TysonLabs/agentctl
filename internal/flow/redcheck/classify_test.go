package redcheck

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"*_test.go", "internal/x/y_test.go", true},
		{"*_test.go", "internal/x/y.go", false},
		{"**/tests/**", "tests/a.rs", true},
		{"**/tests/**", "crates/foo/tests/integration/main.rs", true},
		{"**/tests/**", "src/tests.rs", false},
		{"**/testdata/**", "pkg/testdata/in.json", true},
		{"fixtures/**", "fixtures/a/b.txt", true},
		{"fixtures/**", "src/fixtures/a.txt", false},
		{"src/x.go", "src/x.go", true},
		{"*.test.ts", "web/a.test.ts", true},
		{"test_*.py", "pkg/test_a.py", true},
	}
	for _, c := range cases {
		if got := Match(c.pat, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestNormalizePattern(t *testing.T) {
	for in, want := range map[string]string{"./fixtures/": "fixtures/**", "a/*.txt": "a/*.txt"} {
		if got, err := NormalizePattern(in); err != nil || got != want {
			t.Errorf("NormalizePattern(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "a/[", "  "} {
		if _, err := NormalizePattern(bad); err == nil {
			t.Errorf("NormalizePattern(%q): want an error", bad)
		}
	}
}

func TestJudgeSource(t *testing.T) {
	const code = "fn f() {}\n"
	const mod = "#[cfg(test)]\nmod tests {\n    #[test]\n    fn t() {}\n}\n"
	cases := []struct {
		name, path      string
		status          byte
		before, after   string
		splice          bool
		wantSplice      bool
		wantInconclusve bool
	}{
		{"go source has no markers", "a.go", 'M', "x", "y", true, false, false},
		{"rust code-only change", "a.rs", 'M', code + mod, "fn f() { 1; }\n" + mod, true, false, false},
		{"rust module changed: splice", "a.rs", 'M', code, "fn f() { 1; }\n" + mod, true, true, false},
		{"rust module changed, no splice", "a.rs", 'M', code, "fn f() { 1; }\n" + mod, false, false, true},
		{"rust new file with tests", "a.rs", 'A', "", code + mod, true, false, true},
		{"rust test attribute above module", "a.rs", 'M', code, "#[test]\nfn g() {}\n" + code + mod, true, false, true},
		{"rust moved test is not a new test", "a.rs", 'M', "#[test]\nfn g() {}\n" + code, code + "#[test]\nfn g() {}\n", true, false, false},
		{"python test in source", "m.py", 'M', "def f(): pass\n", "def f(): pass\ndef test_f(): pass\n", true, false, true},
		{"python plain change", "m.py", 'M', "def f(): pass\n", "def f(): return 1\n", true, false, false},
		{"js in-source test", "m.js", 'M', "export const f = 1\n", "export const f = 1\ntest('f', () => {})\n", true, false, true},
		{"deleted rust file", "a.rs", 'D', code + mod, "", true, false, false},
	}
	for _, c := range cases {
		v := judgeSource(c.path, c.status, c.before, c.after, c.splice)
		if (v.splice != "") != c.wantSplice || (len(v.reasons) > 0) != c.wantInconclusve {
			t.Errorf("%s: splice=%q reasons=%v", c.name, v.splice, v.reasons)
		}
	}
}
