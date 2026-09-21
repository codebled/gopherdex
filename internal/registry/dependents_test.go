package registry

import (
	"context"
	"testing"
)

func TestDependents(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	retry := host + "/alice/retry"
	f.publishFiles(t, retry, "v1.0.0", retryFiles(retry, ""))
	gomod := func(mod, requires string) map[string]string {
		return map[string]string{"go.mod": "module " + mod + "\n\ngo 1.22\n\nrequire (\n" + requires + ")\n", "a.go": "package a\n"}
	}
	app := host + "/alice/app"
	f.publishFiles(t, app, "v1.0.0", gomod(app, "\t"+retry+" v1.0.0\n\tgithub.com/x/y v1.2.3 // indirect\n"))
	tool := host + "/alice/tool"
	f.publishFiles(t, tool, "v1.0.0", gomod(tool, "\tgithub.com/x/y v1.2.3\n\t"+retry+" v0.9.0 // indirect\n"))
	// old requires retry, but its latest release no longer does.
	old := host + "/alice/old"
	f.publishFiles(t, old, "v1.0.0", gomod(old, "\t"+retry+" v1.0.0\n"))
	f.publishFiles(t, old, "v1.1.0", gomod(old, "\tgithub.com/x/y v1.2.3\n"))

	deps, err := f.reg.Dependents(ctx, retry, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 2 || deps[0].Path != app || deps[0].Indirect || deps[0].Requires != "v1.0.0" || deps[1].Path != tool || !deps[1].Indirect || deps[1].Requires != "v0.9.0" {
		t.Errorf("dependents = %+v", deps)
	}
	if direct, total, err := f.reg.DependentCounts(ctx, retry); err != nil || direct != 1 || total != 2 {
		t.Errorf("counts = %d/%d, %v", direct, total, err)
	}
	if direct, total, _ := f.reg.DependentCounts(ctx, "github.com/x/y"); direct != 2 || total != 3 {
		t.Errorf("x/y counts = %d/%d, want 2 direct of 3", direct, total)
	}

	// Yanking app's only release drops it from the graph.
	if err := f.reg.Yank(ctx, f.alice, app, "v1.0.0", "", client); err != nil {
		t.Fatal(err)
	}
	if _, total, _ := f.reg.DependentCounts(ctx, retry); total != 1 {
		t.Errorf("after yank: %d dependents", total)
	}

	// Versions published before the table existed are filled in.
	f.reg.DB.ExecContext(ctx, `DELETE FROM version_requires`)
	f.reg.DB.ExecContext(ctx, `UPDATE versions SET requires_indexed = 0`)
	if err := f.reg.BackfillRequires(ctx); err != nil {
		t.Fatal(err)
	}
	if _, total, _ := f.reg.DependentCounts(ctx, "github.com/x/y"); total != 2 {
		t.Errorf("after backfill: %d dependents of x/y, want 2 (tool, old)", total)
	}
}
