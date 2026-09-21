package project

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	xmodule "golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/blob"
	"github.com/parthiban-sivakumar/gopherdex/internal/database"
	"github.com/parthiban-sivakumar/gopherdex/internal/license"
	"github.com/parthiban-sivakumar/gopherdex/internal/mail"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
)

const host = "gopherdex.test"

const mitLicense = `MIT License

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

const readme = "# retry\n\n" +
	"Retry with **backoff**. See [the guide](docs/guide.md) and [section](#usage).\n\n" +
	"![diagram](img/flow.png)\n\n" +
	"<script>alert('owned')</script>\n\n" +
	"[click me](javascript:alert(1)) and [site](https://example.com/x).\n\n" +
	"| a | b |\n|---|---|\n| 1 | 2 |\n"

type nopMailer struct{}

func (nopMailer) Send(context.Context, mail.Message) error { return nil }

func newService(t *testing.T) (*Service, func(modPath, version string, files map[string]string, repo string)) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := database.Open(ctx, filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	blobs, err := blob.OpenFS(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	accts := &accounts.Service{DB: db, Mailer: nopMailer{}}
	reg := &registry.Registry{DB: db, Blobs: blobs, ModuleHost: host}
	u, err := accts.Register(ctx, "alice", "alice@example.com", "correct horse battery", accounts.Client{})
	if err != nil {
		t.Fatal(err)
	}
	db.ExecContext(ctx, `UPDATE users SET email_verified_at = 1 WHERE id = ?`, u.ID)
	u.EmailVerified = true
	_, tok, err := accts.CreateToken(ctx, u, "t", 0, "", accounts.Client{})
	if err != nil {
		t.Fatal(err)
	}

	publish := func(modPath, version string, files map[string]string, repo string) {
		t.Helper()
		src := t.TempDir()
		for name, body := range files {
			p := filepath.Join(src, filepath.FromSlash(name))
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte(body), 0o644)
		}
		zipFile := filepath.Join(t.TempDir(), "m.zip")
		f, _ := os.Create(zipFile)
		if err := modzip.CreateFromDir(f, xmodule.Version{Path: modPath, Version: version}, src); err != nil {
			t.Fatal(err)
		}
		f.Close()
		up := registry.Upload{User: u, Token: tok, Module: modPath, Version: version, ZipFile: zipFile}
		if repo != "" {
			up.Repository, up.Commit, up.Ref = repo, "0123456789abcdef0123456789abcdef01234567", "refs/tags/"+version
		}
		if _, err := reg.Publish(ctx, up); err != nil {
			t.Fatal(err)
		}
	}
	return &Service{Registry: reg, ModuleHost: host, SiteURL: "https://" + host}, publish
}

func moduleFiles(modPath, goModExtra string) map[string]string {
	return map[string]string{
		"go.mod":             "module " + modPath + "\n\ngo 1.23\n\nrequire golang.org/x/sync v0.7.0\n" + goModExtra,
		"retry.go":           "// Package retry retries operations.\n//\n// Example:\n//\n//\tretry.Do(3, f)\npackage retry\n\n// Do runs fn.\nfunc Do(n int, fn func() error) error { return fn() }\n",
		"backoff/backoff.go": "// Package backoff computes delays.\npackage backoff\n\n// Double doubles d.\nfunc Double(d int) int { return 2 * d }\n",
		"README.md":          readme,
		"LICENSE":            mitLicense,
	}
}

func TestPage(t *testing.T) {
	s, publish := newService(t)
	ctx := context.Background()
	mod := host + "/alice/retry"
	publish(mod, "v1.0.0", moduleFiles(mod, ""), "https://github.com/alice/retry")
	publish(mod, "v1.1.0", moduleFiles(mod, "\n// Do ignored n.\nretract v1.0.0\n"), "https://github.com/alice/retry")
	v2files := moduleFiles(mod+"/v2", "")
	v2files["go.mod"] = "// Deprecated: use v3.\n" + v2files["go.mod"]
	publish(mod+"/v2", "v2.0.0", v2files, "")

	p, err := s.Page(ctx, mod, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "retry" || p.Namespace != "alice" || p.Version != "v1.1.0" || !p.IsLatest || p.URL != "/alice/retry" {
		t.Fatalf("page header = %+v", p)
	}
	if !slices.Equal(p.Licenses, []string{"MIT"}) || p.LicenseFile != "LICENSE" {
		t.Errorf("licenses = %v from %q", p.Licenses, p.LicenseFile)
	}
	if p.GoVersion != "1.23" || len(p.Requires) != 1 || len(p.Packages) != 2 || p.FilesCount != 5 {
		t.Errorf("go=%q requires=%v packages=%d files=%d", p.GoVersion, p.Requires, len(p.Packages), p.FilesCount)
	}
	if len(p.Versions) != 2 || p.Versions[0].Version != "v1.1.0" || !p.Versions[1].Retracted || p.Versions[1].RetractRationale != "Do ignored n." {
		t.Errorf("versions = %+v", p.Versions)
	}
	if p.Versions[0].PublishedBy != "alice" {
		t.Errorf("published by %q", p.Versions[0].PublishedBy)
	}
	if !strings.HasPrefix(p.GoSum[0], mod+" v1.1.0 h1:") || !strings.HasPrefix(p.GoSum[1], mod+" v1.1.0/go.mod h1:") {
		t.Errorf("go.sum lines = %q", p.GoSum)
	}
	if p.ZipURL != "https://gopherdex.test/api/proxy/gopherdex.test/alice/retry/@v/v1.1.0.zip" {
		t.Errorf("zip URL = %q", p.ZipURL)
	}
	if len(p.MajorVersions) != 2 || p.MajorVersions[0].Label != "v0/v1" || !p.MajorVersions[0].Current || p.MajorVersions[1].URL != "/alice/retry/v2" {
		t.Errorf("major versions = %+v", p.MajorVersions)
	}

	html := string(p.Readme)
	for _, want := range []string{
		"<strong>backoff</strong>",
		`href="https://github.com/alice/retry/blob/v1.1.0/docs/guide.md"`,
		`src="https://github.com/alice/retry/raw/v1.1.0/img/flow.png"`,
		`href="#usage"`,
		`<a rel="nofollow ugc noopener" href="https://example.com/x">`,
		"<table>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("README HTML is missing %s\n%s", want, html)
		}
	}
	for _, bad := range []string{"<script", "javascript:"} {
		if strings.Contains(html, bad) {
			t.Errorf("README HTML contains %q:\n%s", bad, html)
		}
	}

	// An older version.
	old, err := s.Page(ctx, mod, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if old.IsLatest || old.Retracted == nil || old.Latest != "v1.1.0" {
		t.Errorf("old version page = latest %v retracted %+v", old.IsLatest, old.Retracted)
	}

	// No repository: relative links become plain text.
	v2, err := s.Page(ctx, mod+"/v2", "")
	if err != nil {
		t.Fatal(err)
	}
	if v2.Deprecated != "use v3." || v2.Major != "v2" || v2.Name != "retry" {
		t.Errorf("v2 page = deprecated %q major %q name %q", v2.Deprecated, v2.Major, v2.Name)
	}
	if strings.Contains(string(v2.Readme), "docs/guide.md") || !strings.Contains(string(v2.Readme), "the guide") {
		t.Errorf("relative link without a repository should be unlinked:\n%s", v2.Readme)
	}

	if _, err := s.Page(ctx, mod, "v9.9.9"); err == nil {
		t.Error("unknown version should fail")
	}
}

func TestDocBlocks(t *testing.T) {
	got := DocBlocks("Package retry retries\noperations.\n\nExample:\n\n\tretry.Do(3, f)\n\tretry.Do(4, g)\n")
	want := []DocBlock{
		{Text: "Package retry retries operations."},
		{Text: "Example:"},
		{Code: true, Text: "retry.Do(3, f)\nretry.Do(4, g)"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("DocBlocks = %+v", got)
	}
}

func TestDetectLicenses(t *testing.T) {
	if ids := license.Detect([]byte(mitLicense)); !slices.Equal(ids, []string{"MIT"}) {
		t.Errorf("MIT = %v", ids)
	}
	if ids := license.Detect([]byte("All rights reserved. Ask me first.")); ids != nil {
		t.Errorf("custom text = %v, want none", ids)
	}
}
