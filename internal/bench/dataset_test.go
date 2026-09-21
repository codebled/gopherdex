package bench

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	xmodule "golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	modzip "golang.org/x/mod/zip"
)

func TestNames(t *testing.T) {
	for _, tc := range []struct{ path, owner, name, major string }{
		{"github.com/spf13/cobra", "spf13", "cobra", ""},
		{"go.uber.org/zap", "uber", "zap", ""},
		{"github.com/jackc/pgx/v5", "jackc", "pgx", "/v5"},
		{"gopkg.in/yaml.v3", "gopkg", "yaml", ""},
		{"k8s.io/client-go", "k8s", "client-go", ""},
	} {
		if o := owner(tc.path); o != tc.owner {
			t.Errorf("owner(%s) = %q, want %q", tc.path, o, tc.owner)
		}
		if n, m := baseName(tc.path); n != tc.name || m != tc.major {
			t.Errorf("baseName(%s) = %q, %q", tc.path, n, m)
		}
	}
	for in, want := range map[string]string{"spf13": "spf13", "API": "api-dev", "a": "ax", "Go_Kit": "go-kit"} {
		if got := namespaceName(in); got != want {
			t.Errorf("namespaceName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRewriteGoMod(t *testing.T) {
	real := []byte("module github.com/a/web\n\ngo 1.22.1\n\nrequire (\n\tgithub.com/a/log v1.2.0\n\tgolang.org/x/net v0.20.0 // indirect\n)\n\nreplace github.com/a/log => ../log\n")
	got := string(rewriteGoMod(real, "gdx.test/a/web", map[string]string{"github.com/a/log": "gdx.test/a/log"}))
	want := "module gdx.test/a/web\n\ngo 1.22.1\n\nrequire (\n\tgdx.test/a/log v1.2.0\n\tgolang.org/x/net v0.20.0 // indirect\n)\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestSyntheticModuleZip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	now := time.Now()
	vs := syntheticVersions(r, 30, now.Add(-365*24*time.Hour), now)
	for i := 1; i < len(vs); i++ {
		if semver.Compare(vs[i-1].V, vs[i].V) >= 0 || vs[i].Time.Before(vs[i-1].Time) {
			t.Fatalf("versions out of order: %v then %v", vs[i-1], vs[i])
		}
	}
	m := &Module{Path: "gdx.test/alice/fast-json", Namespace: "alice", Versions: vs, Synopsis: "Package fastjson provides JSON."}
	z, err := writeZip(r, t.TempDir(), m, m.Latest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The go command's own checks accept it.
	if _, err := modzip.CheckZip(xmodule.Version{Path: m.Path, Version: m.Latest()}, z); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pkgName("9lives"), "pkg") || pkgName("fast-json") != "fastjson" {
		t.Error("pkgName")
	}
}
