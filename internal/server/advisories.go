package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/codebled/gopherdex/internal/accounts"
	"github.com/codebled/gopherdex/internal/module"
	"github.com/codebled/gopherdex/internal/registry"
	"github.com/codebled/gopherdex/internal/vulndb"
)

// ---- The vulnerability database, for govulncheck ----

var vulnIDPattern = regexp.MustCompile(`^[A-Z]+-\d{4}-\d{4,}$`)

// handleVulnDB serves the Go vulnerability database format: the
// registry's advisories merged with the public database. Every file is
// also available gzipped at <name>.json.gz, which is what govulncheck asks for.
func (s *server) handleVulnDB(w http.ResponseWriter, r *http.Request) {
	path := r.PathValue("path")
	gz := strings.HasSuffix(path, ".json.gz")
	name, ok := strings.CutSuffix(strings.TrimSuffix(path, ".gz"), ".json")
	if !ok {
		s.notFound(w, r)
		return
	}
	ctx := r.Context()
	if id, ok := strings.CutPrefix(name, "ID/"); ok {
		if !vulnIDPattern.MatchString(id) {
			s.notFound(w, r)
			return
		}
		if !strings.HasPrefix(id, registry.AdvisoryPrefix+"-") {
			if s.vulnUpstream == nil {
				s.notFound(w, r)
				return
			}
			http.Redirect(w, r, s.vulnUpstream.EntryURL(id, strings.TrimPrefix(path, "ID/"+id)), http.StatusFound)
			return
		}
		a, err := s.registry.Advisory(ctx, id)
		if errors.Is(err, module.ErrNotFound) {
			s.notFound(w, r)
			return
		} else if err != nil {
			s.serverError(w, r, err)
			return
		}
		s.writeVulnJSON(w, r, gz, a.OSV(s.advisoryURL(a.ID)))
		return
	}

	own, err := s.ownEntries(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	var upMeta vulndb.DBMeta
	var upModules []vulndb.ModulesEntry
	var upVulns []vulndb.VulnsEntry
	if s.vulnUpstream != nil {
		upMeta, upModules, upVulns = s.vulnUpstream.Indexes()
	}
	meta, modules, vulns := vulndb.Merge(own, upMeta, upModules, upVulns)
	switch name {
	case "index/db":
		if meta.Modified.IsZero() {
			meta.Modified = time.Unix(0, 0).UTC()
		}
		s.writeVulnJSON(w, r, gz, meta)
	case "index/modules":
		s.writeVulnJSON(w, r, gz, modules)
	case "index/vulns":
		s.writeVulnJSON(w, r, gz, vulns)
	default:
		s.notFound(w, r)
	}
}

func (s *server) ownEntries(ctx context.Context) ([]*vulndb.Entry, error) {
	list, err := s.registry.AllAdvisories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*vulndb.Entry, len(list))
	for i, a := range list {
		out[i] = a.OSV(s.advisoryURL(a.ID))
	}
	return out, nil
}

// writeVulnJSON writes v as JSON, or as gzipped JSON for the .json.gz
// names. Like vuln.go.dev, the gzip bytes are the body itself, not a
// Content-Encoding, because govulncheck unzips them explicitly.
func (s *server) writeVulnJSON(w http.ResponseWriter, r *http.Request, gz bool, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if !gz {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			s.log.Warn("write vulndb", "path", r.URL.Path, "err", err)
		}
		return
	}
	zw := gzip.NewWriter(w)
	if err := json.NewEncoder(zw).Encode(v); err != nil {
		s.log.Warn("write vulndb", "path", r.URL.Path, "err", err)
		return
	}
	zw.Close()
}

func (s *server) advisoryURL(id string) string { return s.siteURL + "/advisories/" + id }

// ---- Advisory pages ----

// advisoryForm keeps what was typed after an error, and pre-fills edits.
type advisoryForm struct {
	Summary, Details, Aliases, Packages, References, Credits string
	Introduced, Fixed                                        [3]string
}

func formFromAdvisory(a *registry.Advisory) advisoryForm {
	f := advisoryForm{Summary: a.Summary, Details: a.Details, Aliases: strings.Join(a.Aliases, ", "),
		References: strings.Join(a.References, "\n"), Credits: a.Credits}
	var pkgs []string
	for _, p := range a.Packages {
		line := strings.TrimPrefix(strings.TrimPrefix(p.Path, a.ModulePath), "/")
		if line == "" {
			line = "."
		}
		if len(p.Symbols) > 0 {
			line += ": " + strings.Join(p.Symbols, ", ")
		}
		pkgs = append(pkgs, line)
	}
	f.Packages = strings.Join(pkgs, "\n")
	for i, rg := range a.Ranges {
		if i < len(f.Introduced) {
			f.Introduced[i], f.Fixed[i] = rg.Introduced, rg.Fixed
		}
	}
	return f
}

func advisoryFormFrom(r *http.Request) (advisoryForm, registry.AdvisoryInput) {
	f := advisoryForm{Summary: r.PostFormValue("summary"), Details: r.PostFormValue("details"), Aliases: r.PostFormValue("aliases"),
		Packages: r.PostFormValue("packages"), References: r.PostFormValue("references"), Credits: r.PostFormValue("credits")}
	in := registry.AdvisoryInput{Summary: f.Summary, Details: f.Details, Aliases: f.Aliases, Packages: f.Packages, References: f.References, Credits: f.Credits}
	for i := range f.Introduced {
		f.Introduced[i], f.Fixed[i] = r.PostFormValue(fmt.Sprintf("introduced_%d", i)), r.PostFormValue(fmt.Sprintf("fixed_%d", i))
		in.Ranges = append(in.Ranges, registry.VersionRange{Introduced: f.Introduced[i], Fixed: f.Fixed[i]})
	}
	return f, in
}

type advisoriesData struct {
	Advisories []*registry.Advisory
	SiteURL    string
}

// Feed advertises the security advisories feed on the advisories page.
func (d advisoriesData) Feed() *feedLink {
	return &feedLink{"Security advisories on Gopherdex", "/feeds/advisories.atom"}
}

func (s *server) handleAdvisories(w http.ResponseWriter, r *http.Request) {
	list, err := s.registry.AllAdvisories(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "advisories", "Security advisories", advisoriesData{Advisories: list, SiteURL: s.siteURL})
}

type advisoryData struct {
	A          *registry.Advisory
	ModuleURL  string
	CanEdit    bool
	Form       advisoryForm
	Field      string // field with an error
	Error      string
	Notice     string
	OSVURL     string
	ModuleHost string
}

var advisoryNotices = map[string]string{
	"published": "Advisory published. It's in the vulnerability database now, and every maintainer was emailed.",
	"updated":   "Advisory updated.",
	"withdrawn": "Advisory withdrawn. Tools stop reporting it; this page stays up, marked withdrawn.",
}

func (s *server) handleAdvisory(w http.ResponseWriter, r *http.Request) {
	s.renderAdvisory(w, r, r.PathValue("id"), http.StatusOK, nil, "")
}

func (s *server) renderAdvisory(w http.ResponseWriter, r *http.Request, id string, status int, form *advisoryForm, errMsg string) {
	a, err := s.registry.Advisory(r.Context(), id)
	if errors.Is(err, module.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, r, err)
		return
	}
	role, err := s.registry.Role(r.Context(), s.currentUser(r), a.ModulePath)
	if err != nil && !errors.Is(err, module.ErrNotFound) {
		s.serverError(w, r, err)
		return
	}
	d := advisoryData{A: a, ModuleURL: s.project.URL(a.ModulePath, ""), CanEdit: role == registry.RoleOwner,
		Notice: advisoryNotices[r.URL.Query().Get("done")], Error: errMsg, OSVURL: "/vulndb/ID/" + a.ID + ".json", ModuleHost: s.moduleHost}
	if form != nil {
		d.Form = *form
	} else {
		d.Form = formFromAdvisory(a)
	}
	s.render(w, r, status, "advisory", a.ID, d)
}

// handleCreateAdvisory publishes an advisory from a module's Security tab.
func (s *server) handleCreateAdvisory(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	modPath := r.PostFormValue("module")
	if module.CheckPath(modPath) != nil {
		s.notFound(w, r)
		return
	}
	form, in := advisoryFormFrom(r)
	a, err := s.registry.CreateAdvisory(r.Context(), u, modPath, in, s.clientOf(r))
	var fe *registry.FieldError
	var re *registry.Error
	switch {
	case err == nil:
		s.notifyAdvisory(a, u, "published")
		http.Redirect(w, r, "/advisories/"+a.ID+"?done=published", http.StatusSeeOther)
	case errors.Is(err, module.ErrNotFound):
		s.notFound(w, r)
	case errors.As(err, &fe):
		s.renderProjectWith(w, r, modPath, "security", http.StatusUnprocessableEntity, fe.Message, func(d *projectData) { d.AdvisoryForm, d.AdvisoryField = form, fe.Field })
	case errors.As(err, &re):
		s.renderProject(w, r, modPath, "", "security", re.Status, re.Message)
	default:
		s.serverError(w, r, err)
	}
}

func (s *server) handleUpdateAdvisory(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	id := r.PathValue("id")
	form, in := advisoryFormFrom(r)
	_, err := s.registry.UpdateAdvisory(r.Context(), u, id, in, s.clientOf(r))
	var fe *registry.FieldError
	var re *registry.Error
	switch {
	case err == nil:
		http.Redirect(w, r, "/advisories/"+id+"?done=updated", http.StatusSeeOther)
	case errors.Is(err, module.ErrNotFound):
		s.notFound(w, r)
	case errors.As(err, &fe):
		s.renderAdvisory(w, r, id, http.StatusUnprocessableEntity, &form, fe.Message)
	case errors.As(err, &re):
		s.renderAdvisory(w, r, id, re.Status, &form, re.Message)
	default:
		s.serverError(w, r, err)
	}
}

func (s *server) handleWithdrawAdvisory(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	id := r.PathValue("id")
	a, err := s.registry.WithdrawAdvisory(r.Context(), u, id, s.clientOf(r))
	var re *registry.Error
	switch {
	case err == nil:
		s.notifyAdvisory(a, u, "withdrawn")
		http.Redirect(w, r, "/advisories/"+id+"?done=withdrawn", http.StatusSeeOther)
	case errors.Is(err, module.ErrNotFound):
		s.notFound(w, r)
	case errors.As(err, &re):
		s.renderAdvisory(w, r, id, re.Status, nil, re.Message)
	default:
		s.serverError(w, r, err)
	}
}

// notifyAdvisory emails every maintainer: advisories are security
// notices, so preferences don't apply.
func (s *server) notifyAdvisory(a *registry.Advisory, u *accounts.User, what string) {
	s.emailMaintainers(a.ModulePath, fmt.Sprintf("Security advisory %s %s for %s", a.ID, what, a.ModulePath),
		fmt.Sprintf("@%s %s the security advisory %s for %s:\n\n  %s\n\n%s", u.Username, what, a.ID, a.ModulePath, a.Summary, s.advisoryURL(a.ID)))
}

// ---- Vulnerabilities on project pages ----

// depVuln is a known vulnerability in a module's dependency.
type depVuln struct {
	Module, Version, ID, Summary, Fixed, URL string
}

// dependencyVulns checks each required module version against the
// registry's advisories and the public database. Entries it hasn't seen
// are fetched once and cached; the whole check gives up after a few
// seconds so a slow upstream can't stall the page.
func (s *server) dependencyVulns(ctx context.Context, requires []struct{ Path, Version string }) ([]depVuln, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var out []depVuln
	for _, req := range requires {
		if strings.HasPrefix(req.Path, s.moduleHost+"/") {
			list, err := s.registry.ModuleAdvisories(ctx, req.Path)
			if err != nil {
				return nil, err
			}
			for _, a := range list {
				if a.Affects(req.Version) {
					out = append(out, depVuln{req.Path, req.Version, a.ID, a.Summary, a.Fixed(), "/advisories/" + a.ID})
				}
			}
			continue
		}
		if s.vulnUpstream == nil {
			continue
		}
		for _, mv := range s.vulnUpstream.ModuleVulns(req.Path) {
			if mv.Fixed != "" && module.Compare(req.Version, vulndb.Canonical(mv.Fixed)) >= 0 {
				continue // at or after the latest fix
			}
			e, err := s.vulnUpstream.Entry(ctx, mv.ID, mv.Modified)
			if err != nil {
				return out, err
			}
			if e.AffectsModule(req.Path, req.Version) {
				out = append(out, depVuln{req.Path, req.Version, e.ID, e.Summary, vulndb.Canonical(e.LatestFixed(req.Path)), "https://pkg.go.dev/vuln/" + e.ID})
			}
		}
	}
	return out, nil
}
