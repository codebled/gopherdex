// Package ranking orders search results from two sources, modules hosted
// on the registry and public modules from pkg.go.dev, into one list.
//
// Each candidate gets a score from how well its name, path and summary
// match the query, plus popularity: downloads and "used by" counts for
// hosted modules, and pkg.go.dev's own order (which reflects how many
// packages import a module) for public ones. Verified builds get a small
// boost; deprecated and vulnerable modules sink.
package ranking

import (
	"math"
	"path"
	"slices"
	"strings"

	"golang.org/x/mod/module"
)

// Candidate is one search result from either source.
type Candidate struct {
	Path     string
	Version  string
	Synopsis string
	Hosted   bool

	// Rank is the result's 0-based position in its source's own order
	// (full-text relevance for hosted modules, pkg.go.dev's ranking for
	// public ones), and Of the number of results that source returned.
	Rank, Of int

	// Signals known for hosted modules.
	Downloads30 int
	UsedBy      int
	Verified    bool
	Deprecated  bool
	Vulnerable  bool

	Score float64 // set by Merge, for tests and debugging
}

// name returns a module's last path element without its /vN suffix, and
// its owner (the element before), lower-cased.
func name(modPath string) (owner, name string) {
	prefix, _, ok := module.SplitPathVersion(modPath)
	if !ok {
		prefix = modPath
	}
	prefix = strings.ToLower(prefix)
	return path.Base(path.Dir(prefix)), path.Base(prefix)
}

func terms(query string) []string {
	return strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return r == ' ' || r == '\t' || r == ',' || r == '"'
	})
}

// textScore is how well a candidate matches the query's words, 0–100.
func textScore(ts []string, c Candidate) float64 {
	if len(ts) == 0 {
		return 0
	}
	owner, n := name(c.Path)
	full := strings.ToLower(c.Path)
	syn := strings.ToLower(c.Synopsis)
	total := 0.0
	for _, t := range ts {
		switch {
		case n == t:
			total += 70
		case strings.HasPrefix(n, t):
			total += 45
		case strings.Contains(n, t):
			total += 32
		case owner == t:
			total += 25
		case strings.Contains(full, t):
			total += 18
		}
		if strings.Contains(syn, t) {
			total += 12
		}
	}
	score := total / float64(len(ts))
	// "go-retry" or "go retry" typed for a module named go-retry.
	if joined := strings.Join(ts, "-"); len(ts) > 1 && (n == joined || n == strings.Join(ts, "")) {
		score += 30
	}
	return min(score, 100)
}

// score combines text match, popularity and quality signals.
func score(ts []string, c Candidate) float64 {
	s := textScore(ts, c)
	position := 0.0
	if c.Of > 0 {
		position = 1 - float64(c.Rank)/float64(c.Of)
	}
	if c.Hosted {
		s += 12 * position                            // full-text relevance order
		s += 6 * math.Log10(1+float64(c.Downloads30)) // 1k downloads a month: +18
		s += 10 * math.Log10(1+float64(c.UsedBy))     // used by 10 modules: +10
		s += 8                                        // it's on this registry
		if c.Verified {
			s += 4
		}
	} else {
		s += 26 * position // pkg.go.dev ranks by relevance and importers
	}
	if c.Deprecated {
		s -= 30
	}
	if c.Vulnerable {
		s -= 15
	}
	return s
}

// Merge scores hosted and public candidates, drops public duplicates of
// hosted modules, and returns the best limit, highest score first. Ties
// keep hosted modules first, then each source's own order.
func Merge(query string, hosted, public []Candidate, limit int) []Candidate {
	ts := terms(query)
	seen := map[string]bool{}
	var all []Candidate
	for _, c := range hosted {
		c.Hosted = true
		c.Score = score(ts, c)
		seen[c.Path] = true
		all = append(all, c)
	}
	for _, c := range public {
		if seen[c.Path] {
			continue
		}
		seen[c.Path] = true
		c.Hosted = false
		c.Score = score(ts, c)
		all = append(all, c)
	}
	slices.SortStableFunc(all, func(a, b Candidate) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return 0
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}
