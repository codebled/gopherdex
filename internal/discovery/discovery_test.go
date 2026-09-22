package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/module"
	"github.com/parthiban-sivakumar/gopherdex/internal/store"
)

func newService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"example.com/hello/@v/v1.0.0/go.mod":    "module example.com/hello\n\ngo 1.21\n",
		"example.com/hello/@v/v1.0.0/hello.go":  "// Package hello returns friendly greetings.\npackage hello\n",
		"example.com/hello/@v/v1.0.0.json":      `{"Time":"2026-03-02T10:15:00Z","Origin":{"Hash":"aaa111"}}`,
		"example.com/hello/@v/v1.1.0/go.mod":    "module example.com/hello\n\ngo 1.21\n\n// Greeting was broken.\nretract v1.0.0\n",
		"example.com/hello/@v/v1.1.0/hello.go":  "// Package hello returns friendly greetings.\npackage hello\n\n// Greeting greets.\nfunc Greeting() string { return \"hi\" }\n",
		"example.com/hello/@v/v1.1.0/README.md": "# hello\n",
		"example.com/hello/@v/v1.1.0.json":      `{"Time":"2026-05-18T14:40:00Z","Origin":{"URL":"https://example.com/hello.git","Hash":"bbb222"}}`,
	}
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	svc := &Service{Local: st, Public: fakePublic{}, Index: fakeIndex{}, ProxyPrefix: "/api/proxy", DocsBase: "https://pkg.go.dev"}
	// In the server this is the registry's full-text index.
	svc.HostedSearch = func(ctx context.Context, query string, limit int) ([]Result, error) {
		m, err := svc.Module(ctx, "example.com/hello", "")
		if err != nil || !strings.Contains(strings.ToLower(m.Path+" "+m.Synopsis), strings.ToLower(strings.Fields(query)[0])) {
			return nil, err
		}
		return []Result{{Path: m.Path, Version: m.Version, Synopsis: m.Synopsis, Origin: Hosted}}, nil
	}
	return svc
}

type fakePublic struct{}

func (fakePublic) Versions(_ context.Context, p string) ([]string, error) {
	if p == "github.com/acme/router" {
		return []string{"v0.9.0", "v1.0.0"}, nil
	}
	if p == "github.com/acme/down" {
		return nil, errors.New("connection refused")
	}
	return nil, module.ErrNotFound
}
func (fakePublic) Info(_ context.Context, _, v string) (module.Info, error) {
	return module.Info{Version: v, Time: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
}
func (fakePublic) Latest(context.Context, string) (module.Info, error) {
	return module.Info{}, module.ErrNotFound
}
func (fakePublic) GoMod(_ context.Context, p, _ string) ([]byte, error) {
	return []byte("module " + p + "\n"), nil
}

type fakeIndex struct{}

func (fakeIndex) Search(context.Context, string, int) ([]Result, error) {
	return []Result{{Path: "github.com/acme/router", Version: "v1.0.0", Origin: Public}, {Path: "example.com/hello", Origin: Public}}, nil
}

func TestSearch(t *testing.T) {
	s := newService(t)
	resp, err := s.Search(context.Background(), "hello greetings", ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results = %+v", resp.Results)
	}
	if r := resp.Results[0]; r.Path != "example.com/hello" || r.Origin != Hosted || r.Version != "v1.1.0" || r.Synopsis == "" {
		t.Errorf("hosted result = %+v", r)
	}
	if r := resp.Results[1]; r.Path != "github.com/acme/router" {
		t.Errorf("public result = %+v (hosted duplicates must be dropped)", r)
	}

	hosted, _ := s.Search(context.Background(), "hello", ScopeHosted)
	if len(hosted.Results) != 1 || hosted.Results[0].Origin != Hosted {
		t.Errorf("hosted scope = %+v", hosted.Results)
	}
	public, _ := s.Search(context.Background(), "hello", ScopePublic)
	if len(public.Results) != 1 || public.Results[0].Path != "github.com/acme/router" {
		t.Errorf("public scope = %+v (must skip modules hosted here)", public.Results)
	}
}

func TestHostedModule(t *testing.T) {
	s := newService(t)
	m, err := s.Module(context.Background(), "example.com/hello", "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Origin != Hosted || m.Latest != "v1.1.0" || m.Version != "v1.1.0" || m.TotalVersions != 2 {
		t.Fatalf("module = %+v", m)
	}
	if len(m.Versions) != 2 || m.Versions[0].Version != "v1.1.0" || m.Versions[0].Commit != "bbb222" {
		t.Fatalf("versions = %+v", m.Versions)
	}
	if old := m.Versions[1]; !old.Retracted || old.RetractRationale != "Greeting was broken." {
		t.Errorf("v1.0.0 should be retracted: %+v", old)
	}
	if len(m.Packages) != 1 || len(m.Packages[0].Funcs) != 1 || m.Readme != "# hello\n" {
		t.Errorf("docs = %+v readme=%q", m.Packages, m.Readme)
	}
	if m.Repository != "https://example.com/hello" || m.ZipURL != "/api/proxy/example.com/hello/@v/v1.1.0.zip" {
		t.Errorf("links = %q %q", m.Repository, m.ZipURL)
	}
	if len(m.Warnings) != 0 {
		t.Errorf("warnings = %v", m.Warnings)
	}
}

func TestPublicModule(t *testing.T) {
	s := newService(t)
	ctx := context.Background()
	m, err := s.Module(ctx, "github.com/acme/router", "v0.9.0")
	if err != nil {
		t.Fatal(err)
	}
	if m.Origin != Public || m.Latest != "v1.0.0" || m.DocsURL != "https://pkg.go.dev/github.com/acme/router@v0.9.0" {
		t.Fatalf("module = %+v", m)
	}

	if _, err := s.Module(ctx, "github.com/acme/missing", ""); !errors.Is(err, module.ErrNotFound) {
		t.Errorf("missing module: %v, want ErrNotFound", err)
	}
	if _, err := s.Module(ctx, "github.com/acme/down", ""); !errors.Is(err, ErrUpstream) {
		t.Errorf("proxy down: %v, want ErrUpstream", err)
	}
	if _, err := s.Module(ctx, "example.com/hello", "v7.0.0"); !errors.Is(err, module.ErrNotFound) {
		t.Errorf("unknown version: %v, want ErrNotFound", err)
	}
	if _, err := s.Module(ctx, "not a path", ""); !errors.Is(err, module.ErrInvalid) {
		t.Errorf("bad path: %v, want ErrInvalid", err)
	}
}

func TestOwnPathsNeverComeFromPublic(t *testing.T) {
	s := newService(t)
	// Pretend the registry's host is github.com, where the fake mirror has a module.
	if _, err := s.Module(context.Background(), "github.com/acme/router", ""); err != nil {
		t.Fatalf("without a module host, public lookup should apply: %v", err)
	}
	s.ModuleHost = "github.com"
	if _, err := s.Module(context.Background(), "github.com/acme/router", ""); !errors.Is(err, module.ErrNotFound) {
		t.Fatalf("a path under the registry's host came from the public mirror: %v", err)
	}
}
