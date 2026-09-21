package server

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/parthiban-sivakumar/gopherdex/internal/godoc"
	"github.com/parthiban-sivakumar/gopherdex/internal/module"
	"github.com/parthiban-sivakumar/gopherdex/internal/project"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
	"github.com/parthiban-sivakumar/gopherdex/internal/scan"
)

// goImportPage answers the go command's ?go-get=1 lookup. The "mod" form
// tells it to download the module from this registry's GOPROXY endpoint, so
// plain `go get <host>/<owner>/<module>` needs no configuration. See
// `go help importpath`.
var goImportPage = template.Must(template.New("go-import").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="go-import" content="{{.Root}} mod {{.ProxyURL}}">
<meta http-equiv="refresh" content="0; url={{.PageURL}}">
<title>{{.Root}}</title>
</head>
<body>
<p>{{.Root}} is a Go module on Gopherdex. Install it with <code>go get {{.Root}}</code>, or <a href="{{.PageURL}}">see its project page</a>.</p>
</body>
</html>
`))

// handlePath serves every path no other route claims:
//
//	/<owner>                              the owner's modules
//	/<owner>/<module>[/vN][@version]      the project page (?tab=docs|versions|files)
//	/<owner>/<module>/<package>           redirect to that package's docs
//	…?go-get=1                            the go-import tag for the go command
func (s *server) handlePath(w http.ResponseWriter, r *http.Request) {
	rel, version, hasVersion := strings.Cut(strings.Trim(r.URL.Path, "/"), "@")
	goGet := r.URL.Query().Get("go-get") == "1"
	if rel == "" || (hasVersion && goGet) {
		s.notFound(w, r)
		return
	}
	if !strings.Contains(rel, "/") && !hasVersion && !goGet {
		s.handleOwner(w, r, rel)
		return
	}

	importPath := s.moduleHost + "/" + rel
	if module.CheckPath(importPath) != nil {
		s.notFound(w, r)
		return
	}
	root, err := s.registry.ModuleRoot(r.Context(), importPath)
	if errors.Is(err, registry.ErrQuarantined) && !goGet {
		s.unavailable(w, r, importPath)
		return
	}
	if errors.Is(err, module.ErrNotFound) {
		if goGet {
			http.Error(w, "No module on this registry provides "+importPath+".", http.StatusNotFound)
			return
		}
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	if goGet {
		s.writeGoImport(w, r, root)
		return
	}
	if root != importPath {
		// A package inside the module: show it in the module's docs.
		target := s.project.URL(root, version) + "?tab=docs#" + pkgID(root, importPath)
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	if hasVersion && module.CheckVersion(version) != nil {
		s.notFound(w, r)
		return
	}
	s.handleProject(w, r, root, version)
}

func (s *server) writeGoImport(w http.ResponseWriter, r *http.Request, root string) {
	pageURL := s.siteURL + s.project.URL(root, "")
	var buf bytes.Buffer
	if err := goImportPage.Execute(&buf, struct{ Root, ProxyURL, PageURL string }{root, s.siteURL + proxyPrefix, pageURL}); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	buf.WriteTo(w)
}

// pkgID is the anchor of a package on the docs tab.
func pkgID(modPath, importPath string) string { return godoc.PackageAnchor(modPath, importPath) }

var projectTabs = map[string]bool{"": true, "docs": true, "deps": true, "versions": true, "files": true, "source": true, "security": true, "manage": true}

type projectData struct {
	*project.Page
	Tab        string
	VersionURL string // project URL pinned to the viewed version when it isn't the latest
	Install    string
	Synopsis   string
	ModuleHost string
	Downloads  registry.DownloadStats
	Chart      downloadChart
	Role       string // viewer's role: "owner", "maintainer" or "none"
	CanManage  bool
	IsOwner    bool
	Notice     string
	Error      string

	Publishers        []registry.Publisher  // for owners, on the manage tab
	Teams             []registry.TeamAccess // manage tab of organization modules
	TrustedPublishing bool
	SiteURL           string

	Advisories    []*registry.Advisory // every advisory of the module, withdrawn too
	Affecting     []*registry.Advisory // active ones that affect the viewed version
	Vulnerable    map[string]bool      // versions affected by an active advisory
	ActiveCount   int
	DepVulns      []depVuln // security tab: vulnerable dependencies of the viewed version
	DepVulnsError string
	VulnDBOn      bool
	AdvisoryForm  advisoryForm
	AdvisoryField string

	UsedBy       int // hosted modules whose latest release requires this one
	UsedByDirect int
	Dependents   []dependentRow // deps tab
	Source       *project.SourceFile
	SourcePath   string
	Playground   bool                      // examples can be opened in the Go Playground
	Findings     map[string][]scan.Finding // manage tab: what publish checks flagged, by version
}

// dependentRow is a module that uses the one shown, and whether the
// version it requires has a known vulnerability.
type dependentRow struct {
	registry.Dependent
	Vulnerable bool
}

func (s *server) handleProject(w http.ResponseWriter, r *http.Request, modPath, version string) {
	s.renderProject(w, r, modPath, version, r.URL.Query().Get("tab"), http.StatusOK, "")
}

// renderProject shows a project page tab. The manage tab only exists for the
// module's owners and maintainers.
func (s *server) renderProject(w http.ResponseWriter, r *http.Request, modPath, version, tab string, status int, errMsg string) {
	s.renderProjectFull(w, r, modPath, version, tab, status, errMsg, nil)
}

// renderProjectWith renders the latest version's tab after adjusting the
// page data, e.g. to keep what was typed into a form.
func (s *server) renderProjectWith(w http.ResponseWriter, r *http.Request, modPath, tab string, status int, errMsg string, adjust func(*projectData)) {
	s.renderProjectFull(w, r, modPath, "", tab, status, errMsg, adjust)
}

func (s *server) renderProjectFull(w http.ResponseWriter, r *http.Request, modPath, version, tab string, status int, errMsg string, adjust func(*projectData)) {
	if !projectTabs[tab] {
		tab = ""
	}
	p, err := s.project.Page(r.Context(), modPath, version)
	if errors.Is(err, registry.ErrQuarantined) {
		s.unavailable(w, r, modPath)
		return
	}
	if errors.Is(err, module.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	role, err := s.registry.Role(r.Context(), s.currentUser(r), modPath)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if tab == "manage" && role < registry.RoleMaintainer {
		s.notFound(w, r)
		return
	}
	data := projectData{
		Page: p, Tab: tab, ModuleHost: s.moduleHost, VersionURL: p.URL,
		Role: role.String(), CanManage: role >= registry.RoleMaintainer, IsOwner: role == registry.RoleOwner,
		Notice: manageNotices[r.URL.Query().Get("done")], Error: errMsg,
	}
	if data.Downloads, err = s.registry.ModuleDownloads(r.Context(), modPath); err != nil {
		s.serverError(w, r, err)
		return
	}
	data.Chart = buildChart(data.Downloads.Daily)
	if tab == "manage" {
		if data.Findings, err = s.registry.VersionFindings(r.Context(), modPath); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if tab == "manage" && p.Org != nil {
		if data.Teams, err = s.registry.ModuleTeams(r.Context(), modPath); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if tab == "manage" && data.IsOwner {
		if data.Publishers, err = s.registry.Publishers(r.Context(), modPath); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	data.TrustedPublishing, data.SiteURL = s.githubOIDC != nil, s.siteURL
	if data.Advisories, err = s.registry.ModuleAdvisories(r.Context(), modPath); err != nil {
		s.serverError(w, r, err)
		return
	}
	data.Vulnerable = map[string]bool{}
	for _, a := range data.Advisories {
		if a.WithdrawnAt == nil {
			data.ActiveCount++
		}
		if a.Affects(p.Version) {
			data.Affecting = append(data.Affecting, a)
		}
		for _, v := range p.Versions {
			if a.Affects(v.Version) {
				data.Vulnerable[v.Version] = true
			}
		}
	}
	data.VulnDBOn = s.vulnUpstream != nil
	data.Playground = s.playground
	if data.UsedByDirect, data.UsedBy, err = s.registry.DependentCounts(r.Context(), modPath); err != nil {
		s.serverError(w, r, err)
		return
	}
	switch tab {
	case "deps":
		deps, err := s.registry.Dependents(r.Context(), modPath, 500)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for _, d := range deps {
			row := dependentRow{Dependent: d}
			for _, a := range data.Advisories {
				if a.Affects(d.Requires) {
					row.Vulnerable = true
				}
			}
			data.Dependents = append(data.Dependents, row)
		}
	case "source":
		data.SourcePath = r.URL.Query().Get("file")
		if data.SourcePath == "" {
			data.SourcePath = "go.mod"
		}
		if data.Source, err = s.project.Source(r.Context(), modPath, p.Version, data.SourcePath); errors.Is(err, module.ErrNotFound) {
			data.Source, err = nil, nil
		} else if err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if tab == "security" {
		reqs := make([]struct{ Path, Version string }, len(p.Requires))
		for i, req := range p.Requires {
			reqs[i].Path, reqs[i].Version = req.Path, req.Version
		}
		if data.DepVulns, err = s.dependencyVulns(r.Context(), reqs); err != nil {
			s.log.Warn("check dependency vulnerabilities", "module", modPath, "err", err)
			data.DepVulnsError = "The public vulnerability database didn't answer in time, so this list may be incomplete. Reload to try again."
		}
	}
	if adjust != nil {
		adjust(&data)
	}
	if !p.IsLatest {
		data.VersionURL = s.project.URL(modPath, p.Version)
	}
	installVersion := p.Version
	if p.IsLatest {
		installVersion = "latest"
	}
	data.Install = s.installCommand(modPath, installVersion)
	for _, pkg := range p.Packages {
		if pkg.ImportPath == modPath {
			data.Synopsis = pkg.Synopsis
		}
	}
	title := p.Path
	if !p.IsLatest {
		title += "@" + p.Version
	}
	s.render(w, r, status, "project", title, data)
}

type ownerData struct {
	Namespace   string
	Modules     []ownerModule
	IsSelf      bool
	Org         *registry.Org
	Members     []registry.OrgMember
	Teams       []registry.Team // shown to the organization's members
	IsOrgOwner  bool
	IsOrgMember bool
	Notice      string
	Error       string
}

type ownerModule struct {
	registry.ModuleSummary
	URL string
}

func (s *server) handleOwner(w http.ResponseWriter, r *http.Request, name string) {
	s.renderOwner(w, r, name, http.StatusOK, "")
}

// renderOwner shows a user's or organization's page.
func (s *server) renderOwner(w http.ResponseWriter, r *http.Request, name string, status int, errMsg string) {
	ctx := r.Context()
	exists, err := s.registry.NamespaceExists(ctx, name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !exists {
		s.notFound(w, r)
		return
	}
	mods, err := s.registry.NamespaceModules(ctx, name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data := ownerData{Namespace: name, Notice: manageNotices[r.URL.Query().Get("done")], Error: errMsg}
	for _, m := range mods {
		data.Modules = append(data.Modules, ownerModule{ModuleSummary: m, URL: s.project.URL(m.Path, "")})
	}
	u := s.currentUser(r)
	if u != nil && u.Username == name {
		data.IsSelf = true
	}
	if org, ok, err := s.registry.OrgByName(ctx, name); err != nil {
		s.serverError(w, r, err)
		return
	} else if ok {
		data.Org = org
		if data.Members, err = s.registry.OrgMembers(ctx, name); err != nil {
			s.serverError(w, r, err)
			return
		}
		if data.IsOrgOwner, err = s.registry.IsOrgOwner(ctx, u, name); err != nil {
			s.serverError(w, r, err)
			return
		}
		if data.IsOrgMember, err = s.registry.IsOrgMember(ctx, u, name); err != nil {
			s.serverError(w, r, err)
			return
		}
		if data.IsOrgMember {
			if data.Teams, err = s.registry.Teams(ctx, name); err != nil {
				s.serverError(w, r, err)
				return
			}
		}
	}
	s.render(w, r, status, "owner", "@"+name, data)
}

func (s *server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, "message", "Not found", messageData{
		Kicker:      "404",
		Heading:     "There's nothing at this address.",
		Body:        "The module, version or page may not exist, or the link has a typo. Search for the library instead.",
		ActionURL:   "/",
		ActionLabel: "Search Gopherdex",
	})
}

// installCommand is what someone runs to add a module. Modules under a
// public host install with plain go get; local development hosts need the
// registry's proxy named explicitly.
func (s *server) installCommand(modPath, version string) string {
	cmd := "go get " + modPath + "@" + version
	if strings.HasPrefix(modPath, s.moduleHost+"/") && s.zeroConfig {
		return cmd
	}
	host, _, _ := strings.Cut(modPath, "/")
	return "GOPROXY=" + s.siteURL + proxyPrefix + " GONOSUMDB=" + host + " " + cmd
}

// downloadChart is a 30-day bar chart drawn as SVG on the server.
type downloadChart struct {
	Width, Height float64
	Bars          []chartBar
	Max           int
	Summary       string // text alternative for screen readers
}

type chartBar struct {
	X, Y, W, H float64
	Label      string
}

func buildChart(days []registry.DayCount) downloadChart {
	const width, height, gap = 300.0, 64.0, 2.0
	c := downloadChart{Width: width, Height: height}
	total := 0
	for _, d := range days {
		c.Max = max(c.Max, d.Count)
		total += d.Count
	}
	if len(days) == 0 {
		return c
	}
	bw := width/float64(len(days)) - gap
	for i, d := range days {
		h := 0.0
		if c.Max > 0 {
			h = float64(d.Count) / float64(c.Max) * (height - 4)
		}
		if d.Count > 0 && h < 2 {
			h = 2
		}
		c.Bars = append(c.Bars, chartBar{
			X: float64(i) * (bw + gap), Y: height - h, W: bw, H: h,
			Label: fmt.Sprintf("%s: %s download%s", d.Day.Format("2 Jan"), formatNumber(d.Count), plural(d.Count)),
		})
	}
	c.Summary = fmt.Sprintf("%s downloads in the last 30 days; busiest day %s", formatNumber(total), formatNumber(c.Max))
	return c
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// unavailable is the page for a quarantined module.
func (s *server) unavailable(w http.ResponseWriter, r *http.Request, modPath string) {
	s.render(w, r, http.StatusGone, "message", "Unavailable", messageData{
		Kicker:      "Under review",
		Heading:     modPath + " is unavailable.",
		Body:        "The registry's administrators are reviewing a report about this module. It can't be viewed or installed until they finish.",
		ActionURL:   "/search",
		ActionLabel: "Search for another module",
	})
}
