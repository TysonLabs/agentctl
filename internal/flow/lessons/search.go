package lessons

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// stopwords are dropped from queries and documents. The list is short on
// purpose: it removes glue words, never domain words.
var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be been before but by can could did do does
		doesn don each for from had has have how if in into is it its itself just may might more most
		must never no not now of on once one only or other our out over same should so some such than
		that the their them then there these they this those through to too under until up use used
		very was we were what when where which while who why will with without would yet you your
		after again all also any because between both during first here me my new own via went wrong`) {
		stopwords[w] = true
	}
}

// stem strips a few English suffixes so "locks", "locked" and "locking" meet.
// It is deliberately crude and deterministic.
func stem(w string) string {
	for _, suf := range []string{"ing", "ed", "es", "s"} {
		if len(w) > len(suf)+3 && strings.HasSuffix(w, suf) {
			return w[:len(w)-len(suf)]
		}
	}
	return w
}

// tokens lowercases s, splits it on anything that is not a letter or digit,
// drops stopwords and words shorter than three runes, and stems the rest.
func tokens(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len([]rune(f)) < 3 || stopwords[f] {
			continue
		}
		out = append(out, stem(f))
	}
	return out
}

func tokenSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, t := range tokens(s) {
		m[t] = true
	}
	return m
}

// Field weights: a query word in the title counts most, then in Avoid by, then
// in What went wrong or an Also seen line.
const (
	weightTitle = 3.0
	weightAvoid = 2.0
	weightBody  = 1.0
)

// Match is one search result. Score is in [0, 1]: 1 means every query word
// appears in the title.
type Match struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	File   string   `json:"file"`
	Score  float64  `json:"score"`
	Topics []string `json:"topics"`
}

// SearchOptions narrows a search.
type SearchOptions struct {
	Topics         []string // only lessons sharing one of these topics (none: all)
	IncludeArchive bool     // also search Archive.md
	Exclude        string   // leave out this id (a lesson's own duplicate check)
	Limit          int      // at most this many results (0: no limit)
	MinScore       float64  // leave out results below this score
}

// Search ranks lessons by weighted word overlap with query. Each query word
// scores its inverse document frequency (rare words count more) times the
// weight of the best field it appears in; the sum is divided by what a
// title match of every query word would score. Ties rank by id order.
func (v *Vault) Search(query string, opt SearchOptions) []Match {
	q := tokenSet(query)
	if len(q) == 0 {
		return nil
	}
	type doc struct {
		l                  Lesson
		title, avoid, body map[string]bool
	}
	var docs []doc
	df := map[string]int{}
	for _, l := range v.Lessons {
		d := doc{l, tokenSet(l.Title), tokenSet(l.Avoid), tokenSet(l.What + " " + strings.Join(l.Also, " "))}
		seen := map[string]bool{}
		for _, set := range []map[string]bool{d.title, d.avoid, d.body} {
			for t := range set {
				if !seen[t] {
					seen[t] = true
					df[t]++
				}
			}
		}
		docs = append(docs, d)
	}
	n := float64(len(docs))
	idf := func(t string) float64 { return math.Log(1 + n/float64(1+df[t])) }
	var max float64
	for t := range q {
		max += weightTitle * idf(t)
	}
	want := map[string]bool{}
	for _, t := range opt.Topics {
		if t = strings.TrimSpace(t); t != "" {
			want[t] = true
		}
	}
	var out []Match
	for _, d := range docs {
		l := d.l
		if l.ID == opt.Exclude || (l.File == ArchiveFile && !opt.IncludeArchive) {
			continue
		}
		if len(want) > 0 && !intersects(l.Topics, want) {
			continue
		}
		var s float64
		for t := range q {
			switch {
			case d.title[t]:
				s += weightTitle * idf(t)
			case d.avoid[t]:
				s += weightAvoid * idf(t)
			case d.body[t]:
				s += weightBody * idf(t)
			}
		}
		if s == 0 || max == 0 {
			continue
		}
		score := math.Round(s/max*1000) / 1000
		if score < opt.MinScore {
			continue
		}
		out = append(out, Match{ID: l.ID, Title: l.Title, File: l.File, Score: score, Topics: l.Topics})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return idLess(out[i].ID, out[j].ID)
	})
	if opt.Limit > 0 && len(out) > opt.Limit {
		out = out[:opt.Limit]
	}
	return out
}

// idLess orders "l9" before "l10".
func idLess(a, b string) bool {
	na, nb := idNum(a), idNum(b)
	if na != nb {
		return na < nb
	}
	return a < b
}
