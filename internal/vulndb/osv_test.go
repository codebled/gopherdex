package vulndb

import (
	"testing"
	"time"
)

func TestAffectsVersion(t *testing.T) {
	semverRange := func(events ...Event) []Range { return []Range{{Type: "SEMVER", Events: events}} }
	for _, tc := range []struct {
		name    string
		ranges  []Range
		version string
		want    bool
	}{
		{"all versions", semverRange(Event{Introduced: "0"}), "v0.1.0", true},
		{"before fix", semverRange(Event{Introduced: "0"}, Event{Fixed: "1.2.3"}), "v1.2.2", true},
		{"at fix", semverRange(Event{Introduced: "0"}, Event{Fixed: "1.2.3"}), "v1.2.3", false},
		{"after fix", semverRange(Event{Introduced: "0"}, Event{Fixed: "1.2.3"}), "v1.10.0", false},
		{"before introduced", semverRange(Event{Introduced: "1.1.0"}, Event{Fixed: "1.2.3"}), "v1.0.9", false},
		{"at introduced", semverRange(Event{Introduced: "1.1.0"}, Event{Fixed: "1.2.3"}), "v1.1.0", true},
		{"prerelease of fix", semverRange(Event{Introduced: "0"}, Event{Fixed: "1.2.3"}), "v1.2.3-rc.1", true},
		{"events out of order", semverRange(Event{Fixed: "1.2.3"}, Event{Introduced: "0"}), "v1.0.0", true},
		{"reintroduced", semverRange(Event{Introduced: "0"}, Event{Fixed: "1.1.0"}, Event{Introduced: "1.3.0"}), "v1.4.0", true},
		{"between ranges", semverRange(Event{Introduced: "0"}, Event{Fixed: "1.1.0"}, Event{Introduced: "1.3.0"}), "v1.2.0", false},
		{"no ranges", nil, "v9.9.9", true},
	} {
		if got := AffectsVersion(tc.ranges, tc.version); got != tc.want {
			t.Errorf("%s: AffectsVersion(%s) = %v, want %v", tc.name, tc.version, got, tc.want)
		}
	}
}

func TestMerge(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	own := []*Entry{{
		ID: "GDX-2026-0001", Modified: t1, Aliases: []string{"CVE-2026-1"},
		Affected: []Affected{{Package: Package{Name: "gopherdex.dev/alice/retry"},
			Ranges: []Range{{Type: "SEMVER", Events: []Event{{Introduced: "0"}, {Fixed: "1.2.0"}, {Introduced: "1.5.0"}, {Fixed: "1.5.2"}}}}}},
	}}
	up := []ModulesEntry{{Path: "github.com/x/y", Vulns: []ModuleVuln{{ID: "GO-2020-0001", Modified: t0}}}}
	meta, modules, vulns := Merge(own, DBMeta{Modified: t0}, up, []VulnsEntry{{ID: "GO-2020-0001", Modified: t0}})
	if !meta.Modified.Equal(t1) {
		t.Errorf("modified = %v", meta.Modified)
	}
	if len(modules) != 2 || modules[0].Path != "github.com/x/y" || modules[1].Path != "gopherdex.dev/alice/retry" {
		t.Fatalf("modules = %+v", modules)
	}
	if v := modules[1].Vulns[0]; v.ID != "GDX-2026-0001" || v.Fixed != "1.5.2" {
		t.Errorf("our module vuln = %+v", v)
	}
	if len(vulns) != 2 || vulns[0].ID != "GDX-2026-0001" || vulns[1].ID != "GO-2020-0001" {
		t.Errorf("vulns = %+v", vulns)
	}
	if len(up[0].Vulns) != 1 {
		t.Error("Merge modified the upstream copy")
	}
}
