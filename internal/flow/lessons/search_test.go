package lessons

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestTokens(t *testing.T) {
	got := tokens("The LOCKS were locked, and locking: a re-check of `ids`!")
	want := []string{"lock", "lock", "lock", "check", "ids"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokens %v, want %v", got, want)
	}
}

func ids(ms []Match) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func TestSearchRanking(t *testing.T) {
	dir, _ := vault(t)
	v, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	// "lock" is in l1's title; "check" in l1's title too. Nothing else has them.
	if got := v.Search("lock check", SearchOptions{}); len(got) != 1 || got[0].ID != "l1" || got[0].Score != 1 {
		t.Errorf("title match: %+v", got)
	}
	// A title match outranks an Avoid-by match: "habit" is in every Avoid by,
	// "revived" only in l3's; "seen" is in l3's title.
	got := v.Search("seen habit", SearchOptions{})
	if len(got) == 0 || got[0].ID != "l3" {
		t.Errorf("weighting: %v", ids(got))
	}
	// Archive is left out unless asked; Exclude, Topics and Limit apply.
	if got := v.Search("archived", SearchOptions{}); len(got) != 0 {
		t.Errorf("archive leaked: %v", ids(got))
	}
	if got := v.Search("archived", SearchOptions{IncludeArchive: true}); len(got) != 1 || got[0].ID != "l9" {
		t.Errorf("include archive: %v", ids(got))
	}
	if got := v.Search("lock check", SearchOptions{Exclude: "l1"}); len(got) != 0 {
		t.Errorf("exclude: %v", ids(got))
	}
	if got := v.Search("habit", SearchOptions{Topics: []string{"media"}}); !reflect.DeepEqual(ids(got), []string{"l6", "l7"}) {
		t.Errorf("topics + id tie order: %v", ids(got))
	}
	if got := v.Search("habit", SearchOptions{Limit: 2}); len(got) != 2 {
		t.Errorf("limit: %v", ids(got))
	}
	if got := v.Search("the and of", SearchOptions{}); got != nil {
		t.Errorf("stopword query: %v", ids(got))
	}
	// Also seen text is searchable.
	if got := v.Search("happened", SearchOptions{}); len(got) != 1 || got[0].ID != "l3" {
		t.Errorf("also seen: %v", ids(got))
	}
	// Deterministic: the same query gives the same order every time.
	first := v.Search("habit lock", SearchOptions{})
	for i := 0; i < 20; i++ {
		if again := v.Search("habit lock", SearchOptions{}); !reflect.DeepEqual(again, first) {
			t.Fatalf("order changed: %v vs %v", ids(again), ids(first))
		}
	}
}

func TestIDLess(t *testing.T) {
	if !idLess("l9", "l10") || idLess("l10", "l9") {
		t.Error("numeric id order")
	}
}

func TestSearchDoesNotReadPastSectionBoundary(t *testing.T) {
	dir, _ := vault(t)
	p := filepath.Join(dir, InboxFile)
	inbox := fxInbox + "\n## 2026-06-29, alpha (#0)\n\n- **Also seen:** 2026-07-09, alpha (#9): boundarytoken\n"
	if err := os.WriteFile(p, []byte(inbox), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Search("boundarytoken", SearchOptions{}); len(got) != 0 {
		t.Errorf("section text attributed to previous lesson: %v", ids(got))
	}
}
