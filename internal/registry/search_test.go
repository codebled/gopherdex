package registry

import (
	"context"
	"slices"
	"testing"
	"time"
)

const mit = `MIT License

Copyright (c) 2026 Alice

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`

func (f *fixture) publishFiles(t *testing.T, modPath, version string, files map[string]string) {
	t.Helper()
	up := f.upload(modPath, version, f.zipModule(t, modPath, version, files))
	if _, err := f.reg.Publish(context.Background(), up); err != nil {
		t.Fatal(err)
	}
}

func moduleFiles(modPath, goVersion, doc, readme, lic string) map[string]string {
	files := map[string]string{
		"go.mod":    "module " + modPath + "\n\ngo " + goVersion + "\n",
		"lib.go":    "// " + doc + "\npackage lib\n",
		"README.md": readme,
	}
	if lic != "" {
		files["LICENSE"] = lic
	}
	return files
}

func paths(hits []SearchHit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Path)
	}
	return out
}

func TestSearch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	f.reg.Now = func() time.Time { return now }

	retry := host + "/alice/retry"
	cache := host + "/alice/cache"
	router := host + "/alice/router"
	f.publishFiles(t, retry, "v1.0.0", moduleFiles(retry, "1.21", "Package lib retries operations with exponential backoff.", "# retry\nRetry flaky HTTP calls.", mit))
	f.publishFiles(t, cache, "v1.0.0", moduleFiles(cache, "1.23", "Package lib is an LRU cache.", "# cache\nFast in-memory cache with TTL and backoff-free eviction.", ""))
	now = now.Add(48 * time.Hour)
	f.publishFiles(t, router, "v0.3.0", moduleFiles(router, "1.22", "Package lib routes HTTP requests.", "# router\nA tiny HTTP router.", mit))

	search := func(q SearchQuery) []string {
		t.Helper()
		hits, _, err := f.reg.Search(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return paths(hits)
	}

	// Name matches outrank README mentions; prefixes match.
	if got := search(SearchQuery{Text: "retr", Sort: SortRelevance}); !slices.Equal(got, []string{retry}) {
		t.Errorf("prefix search = %v", got)
	}
	if got := search(SearchQuery{Text: "backoff", Sort: SortRelevance}); len(got) != 2 || got[0] != retry {
		t.Errorf("summary match should outrank README match: %v", got)
	}
	if got := search(SearchQuery{Text: "http", Sort: SortRelevance}); len(got) != 2 {
		t.Errorf("http = %v", got)
	}
	// Typed operators and punctuation are plain text, not FTS syntax.
	for _, weird := range []string{`"`, `retry OR *`, `NEAR(a b)`, `-- ; DROP TABLE modules`, `^`} {
		if _, _, err := f.reg.Search(ctx, SearchQuery{Text: weird}); err != nil {
			t.Errorf("search %q: %v", weird, err)
		}
	}

	// Filters.
	if got := search(SearchQuery{License: "MIT"}); len(got) != 2 || slices.Contains(got, cache) {
		t.Errorf("license filter = %v", got)
	}
	if got := search(SearchQuery{GoMax: "1.22"}); len(got) != 2 || slices.Contains(got, cache) {
		t.Errorf("go filter = %v", got)
	}
	if got := search(SearchQuery{UpdatedWithin: 24 * time.Hour}); !slices.Equal(got, []string{router}) {
		t.Errorf("updated filter = %v", got)
	}

	// Sorting by downloads.
	d := &Downloads{Registry: f.reg}
	for range 3 {
		d.Count(cache, "v1.0.0")
	}
	d.Count(retry, "v1.0.0")
	if err := d.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := search(SearchQuery{Sort: SortDownloads}); got[0] != cache || got[1] != retry {
		t.Errorf("downloads sort = %v", got)
	}
	popular, _ := f.reg.PopularModules(ctx, 10)
	if len(popular) != 2 || popular[0].Downloads30 != 3 {
		t.Errorf("popular = %+v", popular)
	}

	// Yanking and deprecating update the index.
	f.reg.Yank(ctx, f.alice, router, "v0.3.0", "", client)
	if got := search(SearchQuery{Text: "router"}); len(got) != 0 {
		t.Errorf("fully yanked module still searchable: %v", got)
	}
	f.reg.Deprecate(ctx, f.alice, retry, "Use the backoff module.", "", client)
	if got := search(SearchQuery{HideDeprecated: true}); !slices.Equal(got, []string{cache}) {
		t.Errorf("hide deprecated = %v", got)
	}

	licenses, goVersions, err := f.reg.Facets(ctx)
	if err != nil || len(licenses) != 1 || licenses[0] != (Facet{"MIT", 1}) || len(goVersions) != 2 || goVersions[0].Value != "1.23" {
		t.Errorf("facets = %+v %+v %v", licenses, goVersions, err)
	}
}

func TestDownloads(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	day := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	f.reg.Now = func() time.Time { return day }
	mod := host + "/alice/retry"
	f.publishFiles(t, mod, "v1.0.0", moduleFiles(mod, "1.22", "Package lib retries.", "", ""))

	d := &Downloads{Registry: f.reg}
	d.Count(mod, "v1.0.0")
	d.Count(mod, "v1.0.0")
	d.Flush(ctx)
	day = day.Add(-10 * 24 * time.Hour)
	d.Count(mod, "v1.0.0")
	d.Count(host+"/alice/unknown", "v1.0.0") // ignored: no such module
	d.Flush(ctx)
	day = day.Add(10 * 24 * time.Hour)

	stats, err := f.reg.ModuleDownloads(ctx, mod)
	if err != nil {
		t.Fatal(err)
	}
	if stats.LastDay != 2 || stats.LastWeek != 2 || stats.LastMonth != 3 || stats.Total != 3 || stats.ByVersion["v1.0.0"] != 3 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(stats.Daily) != 30 || stats.Daily[29].Count != 2 || stats.Daily[19].Count != 1 {
		t.Fatalf("daily = %+v", stats.Daily)
	}
}

func TestBackfill(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mod := host + "/alice/retry"
	f.publishFiles(t, mod, "v1.0.0", moduleFiles(mod, "1.22", "Package lib retries.", "# retry", mit))
	// Simulate a version published before search existed.
	f.reg.DB.Exec(`UPDATE versions SET readme_text = '', license = '', go_version = '', indexed = 0`)
	f.reg.DB.Exec(`DELETE FROM module_meta`)
	f.reg.DB.Exec(`DELETE FROM module_fts`)

	if err := f.reg.Backfill(ctx); err != nil {
		t.Fatal(err)
	}
	hits, _, _ := f.reg.Search(ctx, SearchQuery{Text: "retry", License: "MIT", GoMax: "1.22"})
	if len(hits) != 1 {
		t.Fatalf("after backfill = %+v", hits)
	}
	if err := f.reg.Backfill(ctx); err != nil {
		t.Fatal(err)
	}
}
