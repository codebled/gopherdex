package ranking

import (
	"testing"
)

func paths(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Path)
	}
	return out
}

func TestMerge(t *testing.T) {
	hosted := []Candidate{
		{Path: "gopherdex.dev/alice/retry", Synopsis: "Retry with backoff.", Rank: 0, Of: 2, Downloads30: 1200, UsedBy: 4, Verified: true},
		{Path: "gopherdex.dev/bob/httpx", Synopsis: "HTTP helpers, including retry.", Rank: 1, Of: 2},
	}
	public := []Candidate{
		{Path: "github.com/avast/retry-go", Synopsis: "Simple library for retry mechanism", Rank: 0, Of: 4},
		{Path: "github.com/hashicorp/go-retryablehttp", Synopsis: "Retryable HTTP client", Rank: 1, Of: 4},
		{Path: "gopherdex.dev/alice/retry", Synopsis: "duplicate of the hosted one", Rank: 2, Of: 4},
		{Path: "github.com/cenkalti/backoff/v4", Synopsis: "Exponential backoff algorithm, with retry", Rank: 3, Of: 4},
	}
	got := Merge("retry", hosted, public, 10)
	if len(got) != 5 {
		t.Fatalf("got %d results, want 5 (the duplicate dropped): %v", len(got), paths(got))
	}
	// Exact name match, hosted and popular, first.
	if got[0].Path != "gopherdex.dev/alice/retry" || !got[0].Hosted {
		t.Errorf("first = %s", got[0].Path)
	}
	// A name that starts with the word beats one that only mentions it.
	pos := map[string]int{}
	for i, c := range got {
		pos[c.Path] = i
	}
	if pos["github.com/avast/retry-go"] > pos["gopherdex.dev/bob/httpx"] {
		t.Errorf("retry-go ranked below httpx: %v", paths(got))
	}
	if pos["github.com/cenkalti/backoff/v4"] < pos["github.com/hashicorp/go-retryablehttp"] {
		t.Errorf("backoff (summary match only) above go-retryablehttp (name match): %v", paths(got))
	}

	// Deprecated and vulnerable modules sink below an equal match.
	sink := Merge("cache", []Candidate{
		{Path: "gopherdex.dev/a/cache", Rank: 0, Of: 2, Deprecated: true},
		{Path: "gopherdex.dev/b/cache", Rank: 1, Of: 2},
	}, nil, 10)
	if sink[0].Path != "gopherdex.dev/b/cache" {
		t.Errorf("deprecated module ranked first: %v", paths(sink))
	}

	// Multi-word queries match hyphenated names.
	multi := Merge("go retry", nil, []Candidate{
		{Path: "github.com/x/retry", Rank: 0, Of: 2},
		{Path: "github.com/y/go-retry", Rank: 1, Of: 2},
	}, 10)
	if multi[0].Path != "github.com/y/go-retry" {
		t.Errorf("go retry: %v", paths(multi))
	}

	// Major version suffixes don't hide the name.
	if _, n := name("gopherdex.dev/alice/retry/v2"); n != "retry" {
		t.Errorf("name of v2 path = %q", n)
	}

	if len(Merge("retry", hosted, public, 2)) != 2 {
		t.Error("limit ignored")
	}
}
