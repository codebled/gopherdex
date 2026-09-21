package registry

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/parthiban-sivakumar/gopherdex/internal/module"
)

func TestAdvisories(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mod := host + "/alice/retry"
	for _, v := range []string{"v1.0.0", "v1.1.0", "v1.2.0"} {
		if err := f.publishAs(t, f.alice, f.token, mod, v); err != nil {
			t.Fatal(err)
		}
	}
	bob, _ := f.user(t, "bob")

	in := AdvisoryInput{
		Summary: "Unbounded retries in retry.Do", Details: "Do retries forever when fn panics.",
		Aliases:  "cve-2026-12345, GHSA-2c4m-59x9-fr2g",
		Ranges:   []VersionRange{{Introduced: "1.1.0", Fixed: "v1.2.0"}, {}},
		Packages: ".: Do, Policy.Next\ninternal/backoff", References: "https://github.com/alice/retry/issues/7",
		Credits: "Mallory Researcher",
	}
	if _, err := f.reg.CreateAdvisory(ctx, bob, mod, in, client); !isReject(err, http.StatusForbidden, "forbidden") {
		t.Fatalf("stranger publishing: %v", err)
	}
	a, err := f.reg.CreateAdvisory(ctx, f.alice, mod, in, client)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(a.ID, "GDX-") || !strings.HasSuffix(a.ID, "-0001") {
		t.Errorf("id = %s", a.ID)
	}
	if strings.Join(a.Aliases, " ") != "CVE-2026-12345 GHSA-2c4m-59x9-fr2g" {
		t.Errorf("aliases = %v", a.Aliases)
	}
	if len(a.Packages) != 2 || a.Packages[0].Path != mod || a.Packages[1].Path != mod+"/internal/backoff" || strings.Join(a.Packages[0].Symbols, ",") != "Do,Policy.Next" {
		t.Errorf("packages = %+v", a.Packages)
	}
	for v, want := range map[string]bool{"v1.0.0": false, "v1.1.0": true, "v1.1.5": true, "v1.2.0": false} {
		if a.Affects(v) != want {
			t.Errorf("Affects(%s) = %v", v, !want)
		}
	}
	if a.Fixed() != "v1.2.0" {
		t.Errorf("fixed = %s", a.Fixed())
	}

	e := a.OSV("https://gopherdex.test/advisories/" + a.ID)
	if e.Affected[0].Package.Name != mod || e.Affected[0].Ranges[0].Events[0].Introduced != "1.1.0" || e.Affected[0].Ranges[0].Events[1].Fixed != "1.2.0" {
		t.Errorf("OSV affected = %+v", e.Affected)
	}
	if len(e.Affected[0].EcosystemSpecific.Imports) != 2 || e.References[len(e.References)-1].Type != "ADVISORY" {
		t.Errorf("OSV entry = %+v", e)
	}

	// Validation.
	for name, bad := range map[string]AdvisoryInput{
		"no summary":       {Details: "x"},
		"no details":       {Summary: "x"},
		"bad alias":        {Summary: "x", Details: "x", Aliases: "CVE-12"},
		"bad version":      {Summary: "x", Details: "x", Ranges: []VersionRange{{Fixed: "1.2"}}},
		"fix before":       {Summary: "x", Details: "x", Ranges: []VersionRange{{Introduced: "v1.2.0", Fixed: "v1.1.0"}}},
		"wrong major":      {Summary: "x", Details: "x", Ranges: []VersionRange{{Fixed: "v2.0.0"}}},
		"bad symbol":       {Summary: "x", Details: "x", Packages: ".: Do()"},
		"http reference":   {Summary: "x", Details: "x", References: "http://example.com"},
		"two-line summary": {Summary: "a\nb", Details: "x"},
	} {
		var fe *FieldError
		if _, err := f.reg.CreateAdvisory(ctx, f.alice, mod, bad, client); !errors.As(err, &fe) {
			t.Errorf("%s: err = %v, want a field error", name, err)
		}
	}

	// Every version, no fix yet.
	open, err := f.reg.CreateAdvisory(ctx, f.alice, mod, AdvisoryInput{Summary: "Unfixed", Details: "No fix yet."}, client)
	if err != nil {
		t.Fatal(err)
	}
	if !open.Affects("v1.2.0") || open.Fixed() != "" || !strings.HasSuffix(open.ID, "-0002") {
		t.Errorf("unfixed advisory = %+v", open)
	}

	// Update, then withdraw.
	in.Ranges = []VersionRange{{Introduced: "v1.0.0", Fixed: "v1.2.0"}}
	if a, err = f.reg.UpdateAdvisory(ctx, f.alice, a.ID, in, client); err != nil {
		t.Fatal(err)
	}
	if !a.Affects("v1.0.0") {
		t.Error("update didn't take")
	}
	if a, err = f.reg.WithdrawAdvisory(ctx, f.alice, a.ID, client); err != nil || a.WithdrawnAt == nil || a.Affects("v1.1.0") {
		t.Fatalf("withdraw: %+v, %v", a, err)
	}
	if _, err := f.reg.WithdrawAdvisory(ctx, f.alice, a.ID, client); !isReject(err, http.StatusConflict, "already_withdrawn") {
		t.Errorf("withdraw twice: %v", err)
	}

	list, err := f.reg.ModuleAdvisories(ctx, mod)
	if err != nil || len(list) != 2 || list[0].ID != open.ID {
		t.Fatalf("module advisories = %v, %v", list, err)
	}

	// Quarantined modules' advisories are hidden with them.
	admin, _ := f.user(t, "admin1")
	if err := f.reg.Quarantine(ctx, admin, mod, "review", client); err != nil {
		t.Fatal(err)
	}
	if all, _ := f.reg.AllAdvisories(ctx); len(all) != 0 {
		t.Errorf("advisories of a quarantined module listed: %d", len(all))
	}
	if _, err := f.reg.Advisory(ctx, open.ID); !errors.Is(err, module.ErrNotFound) {
		t.Errorf("advisory of a quarantined module: %v", err)
	}
}
