package store

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codebled/gopherdex/internal/module"
)

// writeFiles creates files under dir; keys are slash-separated paths.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"example.com/hello/@v/v1.0.0/go.mod":            "module example.com/hello\n",
		"example.com/hello/@v/v1.0.0/hello.go":          "package hello\n",
		"example.com/hello/@v/v1.0.0.json":              `{"Time":"2026-03-02T10:15:00Z","Origin":{"VCS":"git","Hash":"abc123"}}`,
		"example.com/hello/@v/v1.1.0/go.mod":            "module example.com/hello\n",
		"example.com/hello/@v/v1.1.0/hello.go":          "package hello\n",
		"example.com/hello/@v/v1.1.0/.git/HEAD":         "ref\n",
		"example.com/hello/@v/v1.1.0/tools/go.mod":      "module example.com/hello/tools\n",
		"example.com/hello/@v/v1.1.0/tools/tool.go":     "package tools\n",
		"example.com/hello/@v/v1.1.0/sub/sub.go":        "package sub\n",
		"example.com/hello/@v/v1.2.0-beta.1/hello.go":   "package hello\n",
		"example.com/!upper/@v/v0.1.0/go.mod":           "module example.com/Upper\n",
		"example.com/wrong/@v/v0.1.0/go.mod":            "module example.com/other\n",
		"example.com/hello/@v/not-a-version/ignored.go": "package x\n",
	})
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestModulesAndVersions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	mods, err := s.Modules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"example.com/Upper", "example.com/hello", "example.com/wrong"}; !slices.Equal(mods, want) {
		t.Fatalf("Modules = %v, want %v", mods, want)
	}

	versions, err := s.Versions(ctx, "example.com/hello")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"v1.0.0", "v1.1.0", "v1.2.0-beta.1"}; !slices.Equal(versions, want) {
		t.Fatalf("Versions = %v, want %v", versions, want)
	}

	if _, err := s.Versions(ctx, "example.com/missing"); !errors.Is(err, module.ErrNotFound) {
		t.Fatalf("Versions(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.Versions(ctx, "example.com/../../etc"); !errors.Is(err, module.ErrInvalid) {
		t.Fatalf("Versions(traversal) = %v, want ErrInvalid", err)
	}
}

func TestInfoAndLatest(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	info, err := s.Info(ctx, "example.com/hello", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !info.Time.Equal(time.Date(2026, 3, 2, 10, 15, 0, 0, time.UTC)) || info.Origin == nil || info.Origin.Hash != "abc123" {
		t.Fatalf("Info = %+v", info)
	}
	latest, err := s.Latest(ctx, "example.com/hello")
	if err != nil || latest.Version != "v1.1.0" {
		t.Fatalf("Latest = %+v, %v; want v1.1.0", latest, err)
	}
	if _, err := s.Info(ctx, "example.com/hello", "v9.9.9"); !errors.Is(err, module.ErrNotFound) {
		t.Fatalf("Info(v9.9.9) = %v, want ErrNotFound", err)
	}
}

func TestGoMod(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	got, err := s.GoMod(ctx, "example.com/hello", "v1.2.0-beta.1")
	if err != nil || string(got) != "module example.com/hello\n" {
		t.Fatalf("synthesized GoMod = %q, %v", got, err)
	}
	if _, err := s.GoMod(ctx, "example.com/Upper", "v0.1.0"); err != nil {
		t.Fatalf("GoMod(escaped module) = %v", err)
	}
	if _, err := s.GoMod(ctx, "example.com/wrong", "v0.1.0"); err == nil || !strings.Contains(err.Error(), "declares module") {
		t.Fatalf("GoMod(mismatched) = %v, want declares-module error", err)
	}
}

func TestZip(t *testing.T) {
	s := openTestStore(t)
	z, err := s.Zip(context.Background(), "example.com/hello", "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := z.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	want := []string{
		"example.com/hello@v1.1.0/go.mod",
		"example.com/hello@v1.1.0/hello.go",
		"example.com/hello@v1.1.0/sub/sub.go",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("zip files = %v\nwant %v", names, want)
	}
}
