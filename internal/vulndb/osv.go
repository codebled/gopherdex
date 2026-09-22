// Package vulndb implements the Go vulnerability database format
// (https://go.dev/security/vuln/database): OSV entries plus the index
// files govulncheck reads. The registry serves its own advisories in this
// format, merged with the public database at vuln.go.dev, so
//
//	govulncheck -db https://<registry>/vulndb ./...
//
// checks hosted modules and everything else in one run.
package vulndb

import (
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Entry is an OSV vulnerability report (schema 1.3.1, Go ecosystem).
type Entry struct {
	SchemaVersion string      `json:"schema_version"`
	ID            string      `json:"id"`
	Modified      time.Time   `json:"modified"`
	Published     time.Time   `json:"published"`
	Withdrawn     *time.Time  `json:"withdrawn,omitempty"`
	Aliases       []string    `json:"aliases,omitempty"`
	Summary       string      `json:"summary,omitempty"`
	Details       string      `json:"details"`
	Affected      []Affected  `json:"affected"`
	References    []Reference `json:"references,omitempty"`
	Credits       []Credit    `json:"credits,omitempty"`
	// DatabaseSpecific links to the human-readable report.
	DatabaseSpecific *DatabaseSpecific `json:"database_specific,omitempty"`
}

// Affected names one vulnerable module and the version ranges and
// packages the report applies to.
type Affected struct {
	Package           Package            `json:"package"`
	Ranges            []Range            `json:"ranges,omitempty"`
	EcosystemSpecific *EcosystemSpecific `json:"ecosystem_specific,omitempty"`
}

// Package identifies the affected module; in the Go ecosystem, Name is
// the module path.
type Package struct {
	Name      string `json:"name"` // module path
	Ecosystem string `json:"ecosystem"`
}

// Range is a SEMVER range. Versions in events have no "v" prefix;
// "introduced": "0" means from the first version.
type Range struct {
	Type   string  `json:"type"`
	Events []Event `json:"events"`
}

// Event is one boundary of a Range: the version a vulnerability was
// introduced in, or the version that fixed it. Exactly one field is set.
type Event struct {
	Introduced string `json:"introduced,omitempty"`
	Fixed      string `json:"fixed,omitempty"`
}

// EcosystemSpecific holds the Go-specific details of an Affected entry:
// the vulnerable packages and symbols.
type EcosystemSpecific struct {
	Imports []Import `json:"imports,omitempty"`
}

// Import is a vulnerable package, optionally narrowed to symbols, which
// lets govulncheck report only code that actually calls them.
type Import struct {
	Path    string   `json:"path"`
	Symbols []string `json:"symbols,omitempty"`
}

// Reference links to more information about a vulnerability, such as an
// advisory or the commit that fixed it.
type Reference struct {
	Type string `json:"type"` // WEB, FIX, REPORT, ADVISORY…
	URL  string `json:"url"`
}

// Credit names someone who found or reported a vulnerability.
type Credit struct {
	Name string `json:"name"`
}

// DatabaseSpecific holds fields particular to the database serving the
// entry: the URL of its human-readable report.
type DatabaseSpecific struct {
	URL string `json:"url,omitempty"`
}

// ModulesEntry is one element of index/modules.json.
type ModulesEntry struct {
	Path  string       `json:"path"`
	Vulns []ModuleVuln `json:"vulns"`
}

// ModuleVuln is one vulnerability listed for a module in
// index/modules.json.
type ModuleVuln struct {
	ID       string    `json:"id"`
	Modified time.Time `json:"modified"`
	Fixed    string    `json:"fixed,omitempty"` // latest fixed version, no "v"
}

// VulnsEntry is one element of index/vulns.json.
type VulnsEntry struct {
	ID       string    `json:"id"`
	Modified time.Time `json:"modified"`
	Aliases  []string  `json:"aliases,omitempty"`
}

// DBMeta is index/db.json.
type DBMeta struct {
	Modified time.Time `json:"modified"`
}

// Canonical adds the "v" prefix OSV leaves off, so versions compare with
// golang.org/x/mod/semver.
func Canonical(v string) string {
	if v == "" || strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

// Bare removes the "v" prefix, as OSV ranges and indexes expect.
func Bare(v string) string { return strings.TrimPrefix(v, "v") }

// AffectsModule reports whether version (with "v") of modPath is in any
// of e's ranges for that module. Withdrawn entries affect nothing.
func (e *Entry) AffectsModule(modPath, version string) bool {
	if e.Withdrawn != nil {
		return false
	}
	for _, a := range e.Affected {
		if a.Package.Name == modPath && AffectsVersion(a.Ranges, version) {
			return true
		}
	}
	return false
}

// AffectsVersion evaluates SEMVER ranges the way OSV defines them: each
// range is a sequence of introduced/fixed events, and a version is
// affected when the latest event at or below it is an introduction.
func AffectsVersion(ranges []Range, version string) bool {
	if len(ranges) == 0 {
		return true // no ranges: every version
	}
	for _, r := range ranges {
		if r.Type != "SEMVER" {
			continue
		}
		affected := false
		for _, ev := range sortedEvents(r.Events) {
			switch {
			case ev.Introduced != "":
				if ev.Introduced == "0" || semver.Compare(version, Canonical(ev.Introduced)) >= 0 {
					affected = true
				}
			case ev.Fixed != "":
				if semver.Compare(version, Canonical(ev.Fixed)) >= 0 {
					affected = false
				}
			}
		}
		if affected {
			return true
		}
	}
	return false
}

// sortedEvents orders events by version ("0" first), introductions before
// fixes at the same version.
func sortedEvents(events []Event) []Event {
	key := func(e Event) string { // "" sorts before every valid version
		if e.Introduced == "0" {
			return ""
		}
		if e.Introduced != "" {
			return Canonical(e.Introduced)
		}
		return Canonical(e.Fixed)
	}
	out := append([]Event(nil), events...)
	slices.SortStableFunc(out, func(a, b Event) int {
		if c := semver.Compare(key(a), key(b)); c != 0 {
			return c
		}
		if a.Introduced != "" && b.Fixed != "" {
			return -1
		}
		if a.Fixed != "" && b.Introduced != "" {
			return 1
		}
		return 0
	})
	return out
}

// LatestFixed returns the highest fixed version (no "v") in ranges for
// modPath, or "" when there is no fix, for index/modules.json.
func (e *Entry) LatestFixed(modPath string) string {
	best := ""
	for _, a := range e.Affected {
		if a.Package.Name != modPath {
			continue
		}
		for _, r := range a.Ranges {
			for _, ev := range r.Events {
				if ev.Fixed != "" && (best == "" || semver.Compare(Canonical(ev.Fixed), Canonical(best)) > 0) {
					best = ev.Fixed
				}
			}
		}
	}
	return best
}
