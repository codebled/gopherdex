package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/module"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
)

// The public JSON API, version 1. It only grows: fields are added, never
// renamed or removed, so tools written against it keep working. Anything
// incompatible goes into /api/v2. Every endpoint is a GET, needs no
// token, and can be called from any website (CORS).

type v1Module struct {
	Path         string         `json:"path"`
	Namespace    string         `json:"namespace"`
	Latest       string         `json:"latest"` // "" when every version is yanked
	Synopsis     string         `json:"synopsis"`
	Licenses     []string       `json:"licenses"`
	Repository   string         `json:"repository,omitempty"`
	Deprecated   string         `json:"deprecated,omitempty"` // the owners' message
	Successor    string         `json:"successor,omitempty"`
	Owners       []string       `json:"owners"`
	Organization string         `json:"organization,omitempty"`
	CreatedAt    time.Time      `json:"createdAt"`
	Versions     []v1VersionRef `json:"versions"` // newest first
	Downloads    v1Downloads    `json:"downloads"`
	UsedBy       int            `json:"usedBy"` // hosted modules whose latest release requires this one
	Advisories   []v1Advisory   `json:"advisories"`
	URLs         v1URLs         `json:"urls"`
}

type v1VersionRef struct {
	Version     string    `json:"version"`
	PublishedAt time.Time `json:"publishedAt"`
	Prerelease  bool      `json:"prerelease"`
	Yanked      bool      `json:"yanked"`
	YankReason  string    `json:"yankReason,omitempty"`
	Retracted   bool      `json:"retracted"`
	Verified    bool      `json:"verified"` // published by a trusted publisher
}

type v1Downloads struct {
	LastDay   int `json:"lastDay"`
	LastWeek  int `json:"lastWeek"`
	LastMonth int `json:"lastMonth"`
	Total     int `json:"total"`
}

type v1Advisory struct {
	ID        string   `json:"id"`
	Summary   string   `json:"summary"`
	Aliases   []string `json:"aliases"`
	Fixed     string   `json:"fixed,omitempty"`
	Withdrawn bool     `json:"withdrawn"`
	URL       string   `json:"url"`
}

type v1URLs struct {
	Project string `json:"project"`
	Docs    string `json:"docs"`
	Proxy   string `json:"proxy"` // GOPROXY base
	Badge   string `json:"badge"`
}

type v1Version struct {
	Module      string        `json:"module"`
	Version     string        `json:"version"`
	PublishedAt time.Time     `json:"publishedAt"`
	PublishedBy string        `json:"publishedBy,omitempty"`
	Yanked      bool          `json:"yanked"`
	YankReason  string        `json:"yankReason,omitempty"`
	Retracted   bool          `json:"retracted"`
	GoVersion   string        `json:"goVersion,omitempty"`
	Licenses    []string      `json:"licenses"`
	Requires    []v1Require   `json:"requires"`
	Packages    []v1Package   `json:"packages"`
	Files       int           `json:"files"`
	Size        int64         `json:"size"`
	H1          string        `json:"h1"`      // go.sum hash of the zip
	GoModH1     string        `json:"goModH1"` // go.sum hash of go.mod
	SHA256      string        `json:"sha256"`
	Provenance  any           `json:"provenance"` // null unless published by a trusted publisher
	Advisories  []v1Advisory  `json:"advisories"` // active ones affecting this version
	Downloads   int           `json:"downloads"`  // all time
	URLs        v1VersionURLs `json:"urls"`
}

type v1Require struct {
	Path     string `json:"path"`
	Version  string `json:"version"`
	Indirect bool   `json:"indirect"`
}

type v1Package struct {
	ImportPath string `json:"importPath"`
	Name       string `json:"name"`
	Synopsis   string `json:"synopsis"`
	Deprecated bool   `json:"deprecated"`
}

type v1VersionURLs struct {
	Project string `json:"project"`
	Zip     string `json:"zip"`
	Mod     string `json:"mod"`
}

// v1JSON writes a public API response.
func v1JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, status, v)
}

func v1Error(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, status, apiError{msg})
}

// v1ModulePath accepts a full module path or one without the registry's
// host: "alice/retry" means "<host>/alice/retry".
func (s *server) v1ModulePath(p string) string {
	p = strings.Trim(p, "/")
	if !strings.HasPrefix(p, s.moduleHost+"/") {
		p = s.moduleHost + "/" + p
	}
	return p
}

// handleV1Module serves GET /api/v1/modules/{module} and
// GET /api/v1/modules/{module}/@v/{version}.
func (s *server) handleV1Module(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("path")
	modPart, version, isVersion := strings.Cut(raw, "/@v/")
	modPath := s.v1ModulePath(modPart)
	if module.CheckPath(modPath) != nil {
		v1Error(w, http.StatusBadRequest, "That isn't a valid module path.")
		return
	}
	if isVersion && module.CheckVersion(version) != nil {
		v1Error(w, http.StatusBadRequest, "That isn't a valid version, like v1.2.3.")
		return
	}
	p, err := s.project.Page(r.Context(), modPath, version)
	if errors.Is(err, module.ErrNotFound) {
		what := modPath
		if isVersion {
			what += "@" + version
		}
		v1Error(w, http.StatusNotFound, "No "+what+" on this registry.")
		return
	}
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	ctx := r.Context()
	advisories, err := s.registry.ModuleAdvisories(ctx, modPath)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	stats, err := s.registry.ModuleDownloads(ctx, modPath)
	if err != nil {
		s.apiError(w, r, err)
		return
	}

	if isVersion {
		out := v1Version{
			Module: modPath, Version: p.Version, PublishedAt: p.Detail.PublishedAt, PublishedBy: p.Detail.PublishedBy,
			Yanked: p.Detail.Yanked, YankReason: p.Detail.YankReason, Retracted: p.Retracted != nil, GoVersion: p.GoVersion,
			Licenses: nonNil(p.Licenses), Requires: []v1Require{}, Packages: []v1Package{}, Files: p.FilesCount,
			Size: p.Detail.Size, H1: p.Detail.H1, GoModH1: p.Detail.GoModH1, SHA256: p.Detail.SHA256,
			Advisories: []v1Advisory{}, Downloads: stats.ByVersion[p.Version],
			URLs: v1VersionURLs{Project: s.siteURL + s.project.URL(modPath, p.Version), Zip: p.ZipURL, Mod: p.ModURL},
		}
		if p.Detail.Provenance != nil {
			out.Provenance = p.Detail.Provenance
		}
		for _, req := range p.Requires {
			out.Requires = append(out.Requires, v1Require{req.Path, req.Version, req.Indirect})
		}
		for _, pkg := range p.Packages {
			out.Packages = append(out.Packages, v1Package{pkg.ImportPath, pkg.Name, pkg.Synopsis, pkg.Deprecated})
		}
		for _, a := range advisories {
			if a.Affects(p.Version) {
				out.Advisories = append(out.Advisories, s.v1Advisory(a))
			}
		}
		v1JSON(w, http.StatusOK, out)
		return
	}

	out := v1Module{
		Path: modPath, Namespace: p.Namespace, Licenses: nonNil(p.Licenses), Repository: p.RepoURL,
		Deprecated: p.DeprecationNotice, Successor: p.Successor, Owners: []string{}, CreatedAt: p.Versions[len(p.Versions)-1].PublishedAt,
		Versions: []v1VersionRef{}, Advisories: []v1Advisory{},
		Downloads: v1Downloads{stats.LastDay, stats.LastWeek, stats.LastMonth, stats.Total},
		URLs: v1URLs{
			Project: s.siteURL + s.project.URL(modPath, ""), Docs: s.siteURL + s.project.URL(modPath, "") + "?tab=docs",
			Proxy: s.siteURL + proxyPrefix, Badge: s.siteURL + "/badge" + s.project.URL(modPath, "") + ".svg",
		},
	}
	if m, err := s.registry.Module(ctx, modPath); err == nil {
		out.CreatedAt = m.CreatedAt
		if latest := latestInstallable(m); latest != nil {
			out.Latest = latest.Version
		}
	}
	if p.Deprecated != "" && out.Deprecated == "" {
		out.Deprecated = p.Deprecated
	}
	for _, pkg := range p.Packages {
		if pkg.ImportPath == modPath {
			out.Synopsis = pkg.Synopsis
		}
	}
	if p.Org != nil {
		out.Organization = p.Namespace
	}
	for _, c := range p.Collaborators {
		if c.Role == "owner" && !c.Pending {
			out.Owners = append(out.Owners, c.Username)
		}
	}
	for _, v := range p.Versions {
		out.Versions = append(out.Versions, v1VersionRef{Version: v.Version, PublishedAt: v.PublishedAt, Prerelease: v.Prerelease,
			Yanked: v.Yanked, YankReason: v.YankReason, Retracted: v.Retracted, Verified: v.Provenance != nil})
	}
	for _, a := range advisories {
		out.Advisories = append(out.Advisories, s.v1Advisory(a))
	}
	if _, out.UsedBy, err = s.registry.DependentCounts(ctx, modPath); err != nil {
		s.apiError(w, r, err)
		return
	}
	v1JSON(w, http.StatusOK, out)
}

func (s *server) v1Advisory(a *registry.Advisory) v1Advisory {
	return v1Advisory{ID: a.ID, Summary: a.Summary, Aliases: nonNil(a.Aliases), Fixed: a.Fixed(), Withdrawn: a.WithdrawnAt != nil, URL: s.advisoryURL(a.ID)}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

type v1SearchHit struct {
	Path        string    `json:"path"`
	Version     string    `json:"version"`
	Synopsis    string    `json:"synopsis"`
	License     string    `json:"license,omitempty"`
	GoVersion   string    `json:"goVersion,omitempty"`
	Downloads30 int       `json:"downloadsLastMonth"`
	Deprecated  bool      `json:"deprecated"`
	PublishedAt time.Time `json:"publishedAt"`
	URL         string    `json:"url"`
}

// handleV1Search serves GET /api/v1/search?q=&sort=&page=&per_page= over
// the modules hosted here.
func (s *server) handleV1Search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	text := q.Get("q")
	if len(text) > maxQueryLen {
		v1Error(w, http.StatusBadRequest, "q can be at most "+strconv.Itoa(maxQueryLen)+" characters.")
		return
	}
	sort := registry.Sort(q.Get("sort"))
	switch sort {
	case "":
		sort = registry.SortRelevance
		if text == "" {
			sort = registry.SortUpdated
		}
	case registry.SortRelevance, registry.SortDownloads, registry.SortUpdated, registry.SortNew:
	default:
		v1Error(w, http.StatusBadRequest, "sort must be relevance, downloads, updated or new.")
		return
	}
	page, _ := strconv.Atoi(q.Get("page"))
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	if page < 1 {
		page = 1
	}
	if perPage < 1 || perPage > 100 {
		perPage = 20
	}
	hits, total, err := s.registry.Search(r.Context(), registry.SearchQuery{Text: text, Sort: sort, Limit: perPage, Offset: (page - 1) * perPage})
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	out := struct {
		Query   string        `json:"query"`
		Sort    string        `json:"sort"`
		Total   int           `json:"total"`
		Page    int           `json:"page"`
		PerPage int           `json:"perPage"`
		Results []v1SearchHit `json:"results"`
	}{text, string(sort), total, page, perPage, []v1SearchHit{}}
	for _, h := range hits {
		out.Results = append(out.Results, v1SearchHit{h.Path, h.Version, h.Synopsis, h.License, h.GoVersion, h.Downloads30, h.Deprecated,
			h.PublishedAt, s.siteURL + s.project.URL(h.Path, "")})
	}
	v1JSON(w, http.StatusOK, out)
}

// handleV1Owner serves GET /api/v1/owners/{name}: a user's or
// organization's modules.
func (s *server) handleV1Owner(w http.ResponseWriter, r *http.Request) {
	name := strings.ToLower(r.PathValue("name"))
	ctx := r.Context()
	exists, err := s.registry.NamespaceExists(ctx, name)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	if !exists {
		v1Error(w, http.StatusNotFound, "No user or organization @"+name+".")
		return
	}
	kind := "user"
	if _, isOrg, err := s.registry.OrgByName(ctx, name); err != nil {
		s.apiError(w, r, err)
		return
	} else if isOrg {
		kind = "organization"
	}
	const perPage = 100
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	mods, total, err := s.registry.NamespaceModulesPage(ctx, name, perPage, (page-1)*perPage)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	next := ""
	if page*perPage < total {
		next = s.siteURL + "/api/v1/owners/" + name + "?page=" + strconv.Itoa(page+1)
	}
	type ownedModule struct {
		Path        string    `json:"path"`
		Latest      string    `json:"latest"`
		Versions    int       `json:"versions"`
		PublishedAt time.Time `json:"publishedAt"`
		URL         string    `json:"url"`
	}
	out := struct {
		Name    string        `json:"name"`
		Kind    string        `json:"kind"`
		URL     string        `json:"url"`
		Total   int           `json:"total"` // modules in all; Modules holds up to 100
		Next    string        `json:"next,omitempty"`
		Modules []ownedModule `json:"modules"`
	}{name, kind, s.siteURL + "/" + name, total, next, []ownedModule{}}
	for _, m := range mods {
		out.Modules = append(out.Modules, ownedModule{m.Path, m.Latest, m.Versions, m.PublishedAt, s.siteURL + s.project.URL(m.Path, "")})
	}
	v1JSON(w, http.StatusOK, out)
}

// handleV1Stats serves GET /api/v1/stats.
func (s *server) handleV1Stats(w http.ResponseWriter, r *http.Request) {
	st, err := s.registryStats(r.Context())
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	v1JSON(w, http.StatusOK, struct {
		Modules    int    `json:"modules"`
		Releases   int    `json:"releases"`
		Publishers int    `json:"publishers"`
		ModuleHost string `json:"moduleHost"`
		Proxy      string `json:"proxy"`
		VulnDB     string `json:"vulndb"`
	}{st.Modules, st.Releases, st.Publishers, s.moduleHost, s.siteURL + proxyPrefix, s.siteURL + "/vulndb"})
}

// handleAPIDocs is the human-readable API reference at /api.
func (s *server) handleAPIDocs(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "api", "API", struct{ SiteURL, ModuleHost string }{s.siteURL, s.moduleHost})
}
