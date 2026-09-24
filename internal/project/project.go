// Package project builds the data behind a module's project page: README,
// license, documentation, release history and downloadable files.
package project

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	xmodule "golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/codebled/gopherdex/internal/godoc"
	"github.com/codebled/gopherdex/internal/gomod"
	"github.com/codebled/gopherdex/internal/license"
	"github.com/codebled/gopherdex/internal/module"
	"github.com/codebled/gopherdex/internal/registry"
)

// Page is everything the project page template needs.
type Page struct {
	Path              string // module path, e.g. gopherdex.dev/alice/retry/v2
	Namespace         string
	Name              string // "retry"
	Major             string // "v2", or "" for v0/v1
	Version           string // version being viewed
	Latest            string
	IsLatest          bool
	URL               string // site-relative project URL, e.g. /alice/retry/v2
	Detail            registry.VersionDetail
	Versions          []Version
	MajorVersions     []MajorVersion
	Deprecated        string         // from // Deprecated: in the latest go.mod
	Retracted         *gomod.Retract // the viewed version is retracted in go.mod
	DeprecationNotice string         // deprecation set on the website
	Successor         string         // module to use instead
	SuccessorURL      string
	Yanked            bool // the viewed version is yanked
	YankReason        string
	AllYanked         bool // no installable versions remain
	Collaborators     []registry.Collaborator
	Org               *registry.Org // set when the module lives in an organization

	Readme      template.HTML
	ReadmeName  string
	Licenses    []string // SPDX IDs; empty when none were recognized
	LicenseFile string
	GoVersion   string
	Requires    []gomod.Require
	Packages    []godoc.Package
	DocsError   string
	FilesCount  int
	FileList    []string // paths in the module zip

	ZipURL  string
	ModURL  string
	GoSum   []string // lines to paste into go.sum
	RepoURL string   // repository home
	TagURL  string   // repository at the version's tag, when known
}

// Version is one row of the release history.
type Version struct {
	registry.VersionDetail
	Prerelease       bool
	Retracted        bool
	RetractRationale string
	URL              string
}

// MajorVersion links to another major version of the same module.
type MajorVersion struct {
	Path, Label, URL string
	Current          bool
}

// Service builds pages. Parts derived from a version's zip never change, so
// they are cached.
type Service struct {
	Registry   *registry.Registry
	ModuleHost string
	SiteURL    string
	Log        *slog.Logger

	mu    sync.Mutex
	cache map[string]*versionContent // "path@version"
	order []string
}

type versionContent struct {
	readme      template.HTML
	readmeName  string
	licenses    []string
	licenseFile string
	packages    []godoc.Package
	docsErr     string
	files       int
	fileList    []string // every file in the zip, sorted
	goMod       *gomod.File
}

const cacheSize = 256

// URL returns the site-relative project URL for a module path, e.g.
// gopherdex.dev/alice/retry → /alice/retry.
func (s *Service) URL(modPath, version string) string {
	u := strings.TrimPrefix(modPath, s.ModuleHost)
	if version != "" {
		u += "@" + version
	}
	return u
}

// Page builds the project page for modPath at version ("" for latest).
func (s *Service) Page(ctx context.Context, modPath, version string) (*Page, error) {
	m, err := s.Registry.Module(ctx, modPath)
	if err != nil {
		return nil, err
	}
	var all, installable []string
	for _, v := range m.Versions {
		all = append(all, v.Version)
		if !v.Yanked {
			installable = append(installable, v.Version)
		}
	}
	latest := module.Latest(installable)
	allYanked := latest == ""
	if allYanked {
		latest = module.Latest(all)
	}
	if version == "" {
		version = latest
	}
	var detail *registry.VersionDetail
	for i := range m.Versions {
		if m.Versions[i].Version == version {
			detail = &m.Versions[i]
		}
	}
	if detail == nil {
		return nil, fmt.Errorf("%s@%s: %w", modPath, version, module.ErrNotFound)
	}

	content, err := s.content(ctx, modPath, version)
	if err != nil {
		return nil, err
	}
	latestContent := content
	if version != latest {
		if latestContent, err = s.content(ctx, modPath, latest); err != nil {
			return nil, err
		}
	}

	prefix, pathMajor, _ := xmodule.SplitPathVersion(modPath)
	p := &Page{
		Path:              modPath,
		Namespace:         m.Namespace,
		Name:              path.Base(prefix),
		Major:             strings.TrimPrefix(pathMajor, "/"),
		Version:           version,
		Latest:            latest,
		IsLatest:          version == latest,
		URL:               s.URL(modPath, ""),
		Detail:            *detail,
		Readme:            content.readme,
		ReadmeName:        content.readmeName,
		Licenses:          content.licenses,
		LicenseFile:       content.licenseFile,
		Packages:          content.packages,
		DocsError:         content.docsErr,
		FilesCount:        content.files,
		FileList:          content.fileList,
		RepoURL:           detail.Repository,
		DeprecationNotice: m.Deprecation,
		Successor:         m.Successor,
		Yanked:            detail.Yanked,
		YankReason:        detail.YankReason,
		AllYanked:         allYanked,
	}
	if strings.HasPrefix(m.Successor, s.ModuleHost+"/") {
		p.SuccessorURL = s.URL(m.Successor, "")
	}
	if p.Collaborators, err = s.Registry.Collaborators(ctx, modPath); err != nil {
		return nil, err
	}
	if org, ok, err := s.Registry.OrgByName(ctx, m.Namespace); err != nil {
		return nil, err
	} else if ok {
		p.Org = org
	}
	if content.goMod != nil {
		p.GoVersion = content.goMod.Go
		p.Requires = content.goMod.Require
	}
	// Deprecation and retractions come from the latest go.mod.
	if latestContent.goMod != nil {
		p.Deprecated = latestContent.goMod.Deprecated
		if r, ok := latestContent.goMod.Retracted(version); ok {
			p.Retracted = &r
		}
	}
	for _, v := range m.Versions {
		row := Version{VersionDetail: v, Prerelease: semver.Prerelease(v.Version) != "", URL: s.URL(modPath, v.Version)}
		if latestContent.goMod != nil {
			if r, ok := latestContent.goMod.Retracted(v.Version); ok {
				row.Retracted, row.RetractRationale = true, r.Rationale
			}
		}
		p.Versions = append(p.Versions, row)
	}

	escPath, _ := module.EscapePath(modPath)
	escVersion, _ := module.EscapeVersion(version)
	proxyBase := strings.TrimSuffix(s.SiteURL, "/") + "/api/proxy/" + escPath + "/@v/" + escVersion
	p.ZipURL, p.ModURL = proxyBase+".zip", proxyBase+".mod"
	p.GoSum = []string{
		modPath + " " + version + " " + detail.H1,
		modPath + " " + version + "/go.mod " + detail.GoModH1,
	}
	if tag := strings.TrimPrefix(detail.Ref, "refs/tags/"); tag != "" && strings.HasPrefix(detail.Repository, "https://github.com/") {
		p.TagURL = strings.TrimSuffix(detail.Repository, "/") + "/tree/" + url.PathEscape(tag)
	}

	majors, err := s.Registry.MajorVersions(ctx, modPath)
	if err != nil {
		return nil, err
	}
	if len(majors) > 1 {
		for _, mp := range majors {
			_, pm, _ := xmodule.SplitPathVersion(mp)
			label := strings.TrimPrefix(pm, "/")
			if label == "" {
				label = "v0/v1"
			}
			p.MajorVersions = append(p.MajorVersions, MajorVersion{Path: mp, Label: label, URL: s.URL(mp, ""), Current: mp == modPath})
		}
	}
	return p, nil
}

func (s *Service) content(ctx context.Context, modPath, version string) (*versionContent, error) {
	key := modPath + "@" + version
	s.mu.Lock()
	if c, ok := s.cache[key]; ok {
		s.mu.Unlock()
		return c, nil
	}
	s.mu.Unlock()

	m, err := s.Registry.Module(ctx, modPath)
	if err != nil {
		return nil, err
	}
	var detail registry.VersionDetail
	for _, v := range m.Versions {
		if v.Version == version {
			detail = v
		}
	}

	fsys, closer, err := s.Registry.VersionFS(ctx, modPath, version)
	if err != nil {
		return nil, err
	}
	defer closer.Close()

	c := &versionContent{}
	root, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("list files of %s: %w", key, err)
	}
	// The callback skips entries it cannot read and never fails, so the walk
	// cannot return an error.
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			c.files++
			c.fileList = append(c.fileList, p)
		}
		return nil
	})

	tag := strings.TrimPrefix(detail.Ref, "refs/tags/")
	dir := strings.TrimSuffix(strings.TrimSuffix(tag, version), "/")
	repo := newForge(detail.Repository, tag, dir)

	if name := findFile(root, readmeNames); name != "" {
		if src, err := readLimited(fsys, name, maxReadmeSize); err == nil {
			c.readme, c.readmeName = renderReadme(name, src, repo), name
		}
	}
	if name := findFile(root, licenseNames); name != "" {
		if src, err := readLimited(fsys, name, 1<<20); err == nil {
			c.licenseFile = name
			c.licenses = license.Detect(src)
		}
	}
	if src, err := readLimited(fsys, "go.mod", 16<<20); err == nil {
		if f, err := gomod.Parse(src); err == nil {
			c.goMod = f
		}
	}
	if c.packages, err = godoc.ExtractWith(ctx, fsys, modPath, godoc.Options{ExternalURL: s.docURL}); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.log().Warn("generate docs", "module", modPath, "version", version, "err", err)
		c.docsErr = "Documentation couldn't be generated for this version."
		c.packages = nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		s.cache = map[string]*versionContent{}
	}
	if _, ok := s.cache[key]; !ok {
		if len(s.order) >= cacheSize {
			delete(s.cache, s.order[0])
			s.order = s.order[1:]
		}
		s.cache[key] = c
		s.order = append(s.order, key)
	}
	return c, nil
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

var (
	readmeNames  = []string{"README.md", "README.markdown", "README", "README.txt"}
	licenseNames = license.FileNames
)

// findFile returns the root file matching one of names, ignoring case, in
// the order names are listed.
func findFile(entries []fs.DirEntry, names []string) string {
	for _, want := range names {
		for _, e := range entries {
			if !e.IsDir() && strings.EqualFold(e.Name(), want) {
				return e.Name()
			}
		}
	}
	return ""
}

func readLimited(fsys fs.FS, name string, limit int64) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// DocBlock is a paragraph or code block of a Go doc comment.
type DocBlock struct {
	Code bool
	Text string
}

// DocBlocks splits a doc comment: blank lines separate blocks, indented
// blocks are code, and other line breaks are just wrapping.
func DocBlocks(doc string) []DocBlock {
	var blocks []DocBlock
	for _, para := range strings.Split(strings.TrimSpace(doc), "\n\n") {
		lines := strings.Split(strings.Trim(para, "\n"), "\n")
		if len(lines) == 0 || strings.TrimSpace(para) == "" {
			continue
		}
		code := true
		for _, l := range lines {
			if !strings.HasPrefix(l, "\t") && !strings.HasPrefix(l, " ") {
				code = false
			}
		}
		if code {
			for i, l := range lines {
				lines[i] = strings.TrimPrefix(strings.TrimPrefix(l, "\t"), "    ")
			}
			blocks = append(blocks, DocBlock{Code: true, Text: strings.Join(lines, "\n")})
			continue
		}
		for i, l := range lines {
			lines[i] = strings.TrimSpace(l)
		}
		blocks = append(blocks, DocBlock{Text: strings.Join(lines, " ")})
	}
	return blocks
}

// Since formats a time relative to now for release lists.
func Since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute") + " ago"
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	case d < 30*24*time.Hour:
		return plural(int(d/(24*time.Hour)), "day") + " ago"
	case d < 365*24*time.Hour:
		return plural(int(d/(30*24*time.Hour)), "month") + " ago"
	default:
		return plural(int(d/(365*24*time.Hour)), "year") + " ago"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// docURL links documentation to packages outside the module being shown:
// modules hosted here link to their own pages, everything else to
// pkg.go.dev.
func (s *Service) docURL(importPath, symbol string) string {
	if rest, ok := strings.CutPrefix(importPath, s.ModuleHost+"/"); ok {
		// /<owner>/<module>/<package> redirects to the package's docs.
		return "/" + rest
	}
	u := "https://pkg.go.dev/" + importPath
	if symbol != "" {
		u += "#" + symbol
	}
	return u
}

// maxSourceSize is the largest file the source view shows.
const maxSourceSize = 1 << 20

// SourceFile is one file of a published version, for the source view.
type SourceFile struct {
	Path     string
	Lines    []string
	Size     int64
	Binary   bool // not shown
	TooLarge bool // not shown
}

// Source returns a file from a published version's zip, exactly as the
// go command downloads it.
func (s *Service) Source(ctx context.Context, modPath, version, file string) (*SourceFile, error) {
	content, err := s.content(ctx, modPath, version)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(content.fileList, file) {
		return nil, fmt.Errorf("%s@%s/%s: %w", modPath, version, file, module.ErrNotFound)
	}
	fsys, closer, err := s.Registry.VersionFS(ctx, modPath, version)
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	st, err := fs.Stat(fsys, file)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", file, err)
	}
	sf := &SourceFile{Path: file, Size: st.Size()}
	if st.Size() > maxSourceSize {
		sf.TooLarge = true
		return sf, nil
	}
	data, err := fs.ReadFile(fsys, file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		sf.Binary = true
		return sf, nil
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	sf.Lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	return sf, nil
}
