package registry

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/module"
)

func TestTokenScopeAndRequire2FA(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	retry, cache := host+"/alice/retry", host+"/alice/cache"
	_, scoped, err := f.accts.CreateToken(ctx, f.alice, "ci", 0, accounts.ScopeModulePrefix+retry, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.publishAs(t, f.alice, scoped, retry, "v1.0.0"); err != nil {
		t.Fatalf("module token on its module: %v", err)
	}
	if err := f.publishAs(t, f.alice, scoped, cache, "v1.0.0"); !isReject(err, http.StatusForbidden, "token_scope") {
		t.Fatalf("module token on another module: %v", err)
	}

	f.reg.Require2FA = true
	if err := f.publishAs(t, f.alice, f.token, cache, "v1.0.0"); !isReject(err, http.StatusForbidden, "2fa_required") {
		t.Fatalf("publish without 2FA: %v", err)
	}
	f.alice.TwoFactor = true
	if err := f.publishAs(t, f.alice, f.token, cache, "v1.0.0"); err != nil {
		t.Fatalf("publish with 2FA: %v", err)
	}
}

func TestReportsAndQuarantine(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mod := host + "/alice/retry"
	f.publishFiles(t, mod, "v1.0.0", moduleFiles(mod, "1.22", "Package lib retries.", "# retry", mit))
	bob, _ := f.user(t, "bob")
	admin, _ := f.user(t, "carol") // admin rights are checked by the server, not here

	if err := f.reg.ReportModule(ctx, bob, mod, "cryptojacking", "It mines coins.", client); !isReject(err, http.StatusBadRequest, "invalid_category") {
		t.Fatalf("bad category: %v", err)
	}
	if err := f.reg.ReportModule(ctx, bob, mod, "malware", "bad", client); !isReject(err, http.StatusBadRequest, "missing_details") {
		t.Fatalf("short details: %v", err)
	}
	if err := f.reg.ReportModule(ctx, bob, host+"/alice/nope", "spam", "Nothing but ads here.", client); !errors.Is(err, module.ErrNotFound) {
		t.Fatalf("report unknown module: %v", err)
	}
	for _, details := range []string{"init() downloads and runs a binary from a pastebin URL.", "Same code as github.com/x/y with a typo'd name."} {
		if err := f.reg.ReportModule(ctx, bob, mod, "malware", details, client); err != nil {
			t.Fatal(err)
		}
	}
	reports, _ := f.reg.OpenReports(ctx)
	if len(reports) != 2 || reports[0].Reporter != "bob" || reports[0].Module != mod {
		t.Fatalf("open reports = %+v", reports)
	}
	if err := f.reg.DismissReport(ctx, admin, reports[1].ID, "Not a typosquat.", client); err != nil {
		t.Fatal(err)
	}

	if err := f.reg.Quarantine(ctx, admin, mod, "", client); !isReject(err, http.StatusBadRequest, "missing_reason") {
		t.Fatalf("quarantine without reason: %v", err)
	}
	if err := f.reg.Quarantine(ctx, admin, mod, "Downloads a binary at init.", client); err != nil {
		t.Fatal(err)
	}
	// Gone for the go command, the website and search…
	if _, err := f.reg.Versions(ctx, mod); !errors.Is(err, ErrQuarantined) || !errors.Is(err, module.ErrNotFound) {
		t.Fatalf("Versions: %v", err)
	}
	if _, err := f.reg.Zip(ctx, mod, "v1.0.0"); !errors.Is(err, module.ErrNotFound) {
		t.Fatalf("Zip: %v", err)
	}
	if _, err := f.reg.GoMod(ctx, mod, "v1.0.0"); !errors.Is(err, module.ErrNotFound) {
		t.Fatalf("GoMod: %v", err)
	}
	if _, err := f.reg.Module(ctx, mod); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("Module: %v", err)
	}
	if hits, _, _ := f.reg.Search(ctx, SearchQuery{Text: "retry"}); len(hits) != 0 {
		t.Fatalf("search still finds it: %+v", hits)
	}
	if listed, _ := f.reg.RecentlyUpdated(ctx, 10, 0); len(listed) != 0 {
		t.Fatalf("listings still show it: %+v", listed)
	}
	if err := f.publishAs(t, f.alice, f.token, mod, "v1.0.1"); !isReject(err, http.StatusForbidden, "quarantined") {
		t.Fatalf("publish while quarantined: %v", err)
	}
	if reports, _ := f.reg.OpenReports(ctx); len(reports) != 0 {
		t.Fatalf("reports should close on quarantine: %+v", reports)
	}
	q, _ := f.reg.QuarantinedModules(ctx)
	if len(q) != 1 || q[0].Reason != "Downloads a binary at init." {
		t.Fatalf("quarantined = %+v", q)
	}

	// …and back after release.
	if err := f.reg.Release(ctx, admin, mod, client); err != nil {
		t.Fatal(err)
	}
	if versions, err := f.reg.Versions(ctx, mod); err != nil || len(versions) != 1 {
		t.Fatalf("after release: %v %v", versions, err)
	}
	if hits, _, _ := f.reg.Search(ctx, SearchQuery{Text: "retry"}); len(hits) != 1 {
		t.Fatalf("search after release: %+v", hits)
	}
	names, _ := f.reg.Maintainers(ctx, mod)
	if len(names) != 1 || names[0] != "alice" {
		t.Fatalf("maintainers = %v", names)
	}
}
