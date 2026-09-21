package registry

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestPublishChecks(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mod := host + "/alice/retry"

	// A compiled program in the zip is refused, and nothing is stored.
	files := retryFiles(mod, "")
	files["bin/tool"] = "\x7fELF\x02\x01\x01\x00"
	up := f.upload(mod, "v1.0.0", f.zipModule(t, mod, "v1.0.0", files))
	if _, err := f.reg.Publish(ctx, up); !isReject(err, http.StatusUnprocessableEntity, "blocked_by_checks") || !strings.Contains(err.Error(), "bin/tool") {
		t.Fatalf("executable: %v", err)
	}
	if _, err := f.reg.Module(ctx, mod); err == nil {
		t.Fatal("a blocked upload created the module")
	}

	// Code that runs on import is published, flagged and reported.
	files = retryFiles(mod, "")
	files["init.go"] = "package retry\n\nimport \"os/exec\"\n\nfunc init() { exec.Command(\"sh\").Run() }\n"
	up = f.upload(mod, "v1.0.0", f.zipModule(t, mod, "v1.0.0", files))
	pub, err := f.reg.Publish(ctx, up)
	if err != nil {
		t.Fatal(err)
	}
	if len(pub.Warnings) != 1 || pub.Warnings[0].Rule != "runs-on-import" || pub.Warnings[0].File != "init.go" {
		t.Fatalf("warnings = %+v", pub.Warnings)
	}
	stored, err := f.reg.VersionFindings(ctx, mod)
	if err != nil || len(stored["v1.0.0"]) != 1 {
		t.Errorf("stored findings = %v, %v", stored, err)
	}
	reports, _ := f.reg.OpenReports(ctx)
	if len(reports) != 1 || reports[0].Reporter != "" || reports[0].Category != "malware" || !strings.Contains(reports[0].Details, "init.go:5") {
		t.Errorf("reports = %+v", reports)
	}

	// Names close to someone else's module are flagged when first published.
	alicee, alieeTok := f.user(t, "alicee")
	squat := host + "/alicee/retry"
	up = f.upload(squat, "v1.0.0", f.zipModule(t, squat, "v1.0.0", retryFiles(squat, "")))
	up.User, up.Token = alicee, alieeTok
	if pub, err = f.reg.Publish(ctx, up); err != nil {
		t.Fatal(err)
	}
	if len(pub.Warnings) != 1 || pub.Warnings[0].Rule != "typosquatting" || !strings.Contains(pub.Warnings[0].Message, "alice/retry") {
		t.Errorf("typosquat warnings = %+v", pub.Warnings)
	}
	// A later version of the same module isn't flagged for its name again.
	up = f.upload(squat, "v1.1.0", f.zipModule(t, squat, "v1.1.0", retryFiles(squat, " again")))
	up.User, up.Token = alicee, alieeTok
	if pub, err = f.reg.Publish(ctx, up); err != nil || len(pub.Warnings) != 0 {
		t.Errorf("second version: %+v, %v", pub.Warnings, err)
	}

	// Same owner/name as a well-known GitHub module.
	stretchr, stretchrTok := f.user(t, "stretchr")
	fake := host + "/stretchr/testify"
	up = f.upload(fake, "v1.0.0", f.zipModule(t, fake, "v1.0.0", retryFiles(fake, "")))
	up.User, up.Token = stretchr, stretchrTok
	if pub, err = f.reg.Publish(ctx, up); err != nil || len(pub.Warnings) != 1 || pub.Warnings[0].Rule != "impersonation" {
		t.Errorf("impersonation: %+v, %v", pub.Warnings, err)
	}
	if reports, _ = f.reg.OpenReports(ctx); len(reports) != 3 || reports[1].Category != "typosquatting" {
		t.Errorf("%d reports, want 3", len(reports))
	}

	// Checks can be turned off.
	f.reg.SkipChecks = true
	files = retryFiles(mod, "")
	files["bin/tool"] = "\x7fELF\x02\x01\x01\x00"
	up = f.upload(mod, "v1.1.0", f.zipModule(t, mod, "v1.1.0", files))
	if _, err := f.reg.Publish(ctx, up); err != nil {
		t.Errorf("with checks off: %v", err)
	}
}
