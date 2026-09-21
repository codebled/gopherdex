// Package discovery answers the questions the landing page asks: which
// libraries match a search, which versions a library has, and what
// documentation is available for it.
//
// Modules hosted in the local store come first. Other modules are looked up
// on a public proxy (proxy.golang.org) and pkg.go.dev when those are
// configured.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/godoc"
	"github.com/parthiban-sivakumar/gopherdex/internal/gomod"
	"github.com/parthiban-sivakumar/gopherdex/internal/module"
)

// ErrUpstream reports that the public proxy failed to answer.
var ErrUpstream = errors.New("public module proxy unavailable")

const (
	maxResults      = 20
	maxHistory      = 50
	infoConcurrency = 8
	maxReadmeSize   = 64 << 10
)

// ModuleSource reads module metadata. *store.Store and *goproxy.Client
// implement it.
type ModuleSource interface {
	Versions(ctx context.Context, modPath string) ([]string, error)
	Info(ctx context.Context, modPath, version string) (module.Info, error)
	Latest(ctx context.Context, modPath string) (module.Info, error)
	GoMod(ctx context.Context, modPath, version string) ([]byte, error)
}

// LocalStore is a ModuleSource that can also list its modules and expose
// their source for documentation.
type LocalStore interface {
	ModuleSource
	Modules(ctx context.Context) ([]string, error)
	VersionFS(ctx context.Context, modPath, version string) (fs.FS, io.Closer, error)
}

// Index searches public modules.
type Index interface {
	Search(ctx context.Context, query string, limit int) ([]Result, error)
}

// Origin says where a module's data comes from.
type Origin string

const (
	Hosted Origin = "hosted" // served by this registry
	Public Origin = "public" // proxy.golang.org / pkg.go.dev
)

// Result is one search hit.
type Result struct {
	Path     string `json:"path"`
	Version  string `json:"version,omitempty"`
	Synopsis string `json:"synopsis,omitempty"`
	Origin   Origin `json:"origin"`
}

// SearchResponse is the answer to a search.
type SearchResponse struct {
	Query   string   `json:"query"`
	Results []Result `json:"results"`
	Warning string   `json:"warning,omitempty"`
}

// Version is one entry in a module's release history.
type Version struct {
	Version          string    `json:"version"`
	Time             time.Time `json:"time"`
	Commit           string    `json:"commit,omitempty"`
	Prerelease       bool      `json:"prerelease"`
	Retracted        bool      `json:"retracted"`
	RetractRationale string    `json:"retractRationale,omitempty"`
	DocsURL          string    `json:"docsURL,omitempty"`
}

// Module is everything the library view shows.
type Module struct {
	Path          string          `json:"path"`
	Origin        Origin          `json:"origin"`
	Synopsis      string          `json:"synopsis,omitempty"`
	Latest        string          `json:"latest"`
	Version       string          `json:"version"`  // version whose docs and go.mod are shown
	Versions      []Version       `json:"versions"` // newest first, at most 50
	TotalVersions int             `json:"totalVersions"`
	Deprecated    string          `json:"deprecated,omitempty"`
	Repository    string          `json:"repository,omitempty"`
	GoMod         *gomod.File     `json:"goMod,omitempty"`
	Packages      []godoc.Package `json:"packages"`
	Readme        string          `json:"readme,omitempty"`
	DocsURL       string          `json:"docsURL,omitempty"`
	VersionsURL   string          `json:"versionsURL,omitempty"`
	ModURL        string          `json:"modURL,omitempty"`
	ZipURL        string          `json:"zipURL,omitempty"`
	Warnings      []string        `json:"warnings,omitempty"`
}

// Service combines the local store with optional public sources.
type Service struct {
	Local       LocalStore
	Public      ModuleSource // nil disables lookups of public modules
	Index       Index        // nil disables public search
	ProxyPrefix string       // URL prefix of this registry's GOPROXY, e.g. "/api/proxy"
	DocsBase    string       // e.g. "https://pkg.go.dev"
	Log         *slog.Logger

	docs sync.Map // "path@version" → docEntry; hosted versions are immutable
}

type docEntry struct {
	pkgs   []godoc.Package
	readme string
	err    error
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// HostedModules lists modules served by this registry with their latest
// version and synopsis.
func (s *Service) HostedModules(ctx context.Context) ([]Result, error) {
	paths, err := s.Local.Modules(ctx)
	if err != nil {
		return nil, err
	}
	out := []Result{}
	for _, p := range paths {
		info, err := s.Local.Latest(ctx, p)
		if errors.Is(err, module.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("latest version of %s: %w", p, err)
		}
		out = append(out, Result{Path: p, Version: info.Version, Synopsis: s.synopsis(ctx, p, info.Version), Origin: Hosted})
	}
	return out, nil
}

// Scope limits a search to one source.
type Scope string

const (
	ScopeAll    Scope = ""
	ScopeHosted Scope = "hosted" // this registry only; fast
	ScopePublic Scope = "public" // pkg.go.dev only; can take seconds
)

// Search finds hosted modules whose path or synopsis contains every word of
// query, then public modules from the index. Public results never repeat a
// hosted module.
func (s *Service) Search(ctx context.Context, query string, scope Scope) (SearchResponse, error) {
	query = strings.TrimSpace(query)
	resp := SearchResponse{Query: query, Results: []Result{}}
	if query == "" {
		return resp, nil
	}
	hosted, err := s.HostedModules(ctx)
	if err != nil {
		return resp, err
	}
	seen := map[string]bool{}
	terms := strings.Fields(strings.ToLower(query))
	for _, r := range hosted {
		seen[r.Path] = true
		text := strings.ToLower(r.Path + " " + r.Synopsis)
		if scope != ScopePublic && !slices.ContainsFunc(terms, func(t string) bool { return !strings.Contains(text, t) }) {
			resp.Results = append(resp.Results, r)
		}
	}
	if scope == ScopeHosted || s.Index == nil {
		return resp, nil
	}
	public, err := s.Index.Search(ctx, query, maxResults)
	if err != nil {
		if ctx.Err() != nil {
			return resp, ctx.Err()
		}
		s.log().Warn("public search failed", "query", query, "err", err)
		resp.Warning = "Public search on pkg.go.dev isn't available right now."
		return resp, nil
	}
	for _, r := range public {
		if len(resp.Results) == maxResults {
			break
		}
		if !seen[r.Path] {
			seen[r.Path] = true
			resp.Results = append(resp.Results, r)
		}
	}
	return resp, nil
}

// Module returns the release history and documentation of modPath. An empty
// version selects the latest one.
func (s *Service) Module(ctx context.Context, modPath, version string) (*Module, error) {
	if err := module.CheckPath(modPath); err != nil {
		return nil, err
	}
	if version != "" {
		if err := module.CheckVersion(version); err != nil {
			return nil, err
		}
	}

	var src ModuleSource = s.Local
	origin := Hosted
	versions, err := s.Local.Versions(ctx, modPath)
	if errors.Is(err, module.ErrNotFound) && s.Public != nil {
		src, origin = s.Public, Public
		versions, err = s.Public.Versions(ctx, modPath)
		if err != nil && !errors.Is(err, module.ErrNotFound) && ctx.Err() == nil {
			err = fmt.Errorf("%w: %w", ErrUpstream, err)
		}
	}
	if err != nil {
		return nil, err
	}

	latest := module.Latest(versions)
	if latest == "" {
		// Modules without tags only have pseudo-versions, found via @latest.
		info, err := src.Latest(ctx, modPath)
		if err != nil {
			return nil, err
		}
		versions, latest = []string{info.Version}, info.Version
	}
	if version == "" {
		version = latest
	} else if !slices.Contains(versions, version) {
		return nil, fmt.Errorf("%s@%s: %w", modPath, version, module.ErrNotFound)
	}

	m := &Module{
		Path:          modPath,
		Origin:        origin,
		Latest:        latest,
		Version:       version,
		TotalVersions: len(versions),
		Packages:      []godoc.Package{},
	}

	// Deprecation and retractions are declared in the latest go.mod.
	latestMod, err := s.goMod(ctx, src, modPath, latest)
	if err != nil {
		m.warn(s.log(), "The go.mod file for "+latest+" couldn't be read.", err)
	} else {
		m.Deprecated = latestMod.Deprecated
	}
	m.GoMod = latestMod
	if version != latest {
		if m.GoMod, err = s.goMod(ctx, src, modPath, version); err != nil {
			m.warn(s.log(), "The go.mod file for "+version+" couldn't be read.", err)
		}
	}

	s.loadHistory(ctx, m, src, versions, latestMod)
	if info, err := src.Info(ctx, modPath, latest); err == nil && info.Origin != nil {
		m.Repository = strings.TrimSuffix(info.Origin.URL, ".git")
	}

	switch origin {
	case Hosted:
		e := s.localDocs(ctx, modPath, version)
		if e.err != nil {
			m.warn(s.log(), "Documentation couldn't be generated for "+version+".", e.err)
		}
		m.Packages, m.Readme = e.pkgs, e.readme
		m.Synopsis = rootSynopsis(modPath, e.pkgs)
		if esc, err := module.EscapePath(modPath); err == nil {
			escV, _ := module.EscapeVersion(version)
			base := s.ProxyPrefix + "/" + esc + "/@v/" + escV
			m.ModURL, m.ZipURL = base+".mod", base+".zip"
		}
	case Public:
		m.DocsURL = s.DocsBase + "/" + modPath + "@" + version
		m.VersionsURL = s.DocsBase + "/" + modPath + "?tab=versions"
	}
	return m, nil
}

func (m *Module) warn(log *slog.Logger, msg string, err error) {
	log.Warn("module view incomplete", "module", m.Path, "msg", msg, "err", err)
	m.Warnings = append(m.Warnings, msg)
}

// loadHistory fetches .info for the newest versions concurrently. A version
// whose info fails still appears, without a date.
func (s *Service) loadHistory(ctx context.Context, m *Module, src ModuleSource, versions []string, latestMod *gomod.File) {
	newest := slices.Clone(versions)
	slices.Reverse(newest)
	newest = newest[:min(len(newest), maxHistory)]

	m.Versions = make([]Version, len(newest))
	failed := make([]bool, len(newest))
	sem := make(chan struct{}, infoConcurrency)
	var wg sync.WaitGroup
	for i, v := range newest {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			entry := Version{Version: v, Prerelease: module.IsPrerelease(v)}
			if info, err := src.Info(ctx, m.Path, v); err != nil {
				failed[i] = true
				s.log().Debug("version info failed", "module", m.Path, "version", v, "err", err)
			} else {
				entry.Time = info.Time
				if info.Origin != nil {
					entry.Commit = info.Origin.Hash
				}
			}
			if latestMod != nil {
				if r, ok := latestMod.Retracted(v); ok {
					entry.Retracted, entry.RetractRationale = true, r.Rationale
				}
			}
			if m.Origin == Public {
				entry.DocsURL = s.DocsBase + "/" + m.Path + "@" + v
			}
			m.Versions[i] = entry
		}()
	}
	wg.Wait()
	if slices.Contains(failed, true) {
		m.warn(s.log(), "Some release dates couldn't be loaded.", errors.New("one or more .info requests failed"))
	}
}

func (s *Service) goMod(ctx context.Context, src ModuleSource, modPath, version string) (*gomod.File, error) {
	data, err := src.GoMod(ctx, modPath, version)
	if err != nil {
		return nil, err
	}
	return gomod.Parse(data)
}

func (s *Service) localDocs(ctx context.Context, modPath, version string) docEntry {
	key := modPath + "@" + version
	if e, ok := s.docs.Load(key); ok {
		return e.(docEntry)
	}
	var e docEntry
	fsys, closer, err := s.Local.VersionFS(ctx, modPath, version)
	if err != nil {
		e.err = err
	} else {
		e.pkgs, e.err = godoc.Extract(ctx, fsys, modPath)
		e.readme = readme(fsys)
		closer.Close()
	}
	if e.pkgs == nil {
		e.pkgs = []godoc.Package{}
	}
	if ctx.Err() == nil {
		s.docs.Store(key, e)
	}
	return e
}

func (s *Service) synopsis(ctx context.Context, modPath, version string) string {
	e := s.localDocs(ctx, modPath, version)
	if e.err != nil {
		s.log().Debug("docs unavailable", "module", modPath, "version", version, "err", e.err)
	}
	return rootSynopsis(modPath, e.pkgs)
}

func rootSynopsis(modPath string, pkgs []godoc.Package) string {
	for _, p := range pkgs {
		if p.ImportPath == modPath {
			return p.Synopsis
		}
	}
	if len(pkgs) > 0 {
		return pkgs[0].Synopsis
	}
	return ""
}

func readme(fsys fs.FS) string {
	for _, name := range []string{"README.md", "README", "README.txt", "readme.md"} {
		st, err := fs.Stat(fsys, name)
		if err != nil || !st.Mode().IsRegular() || st.Size() > maxReadmeSize {
			continue
		}
		if data, err := fs.ReadFile(fsys, name); err == nil {
			return string(data)
		}
	}
	return ""
}
