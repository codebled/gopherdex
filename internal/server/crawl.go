package server

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"time"
)

// maxSitemapURLs is the sitemap protocol's limit for one file.
const maxSitemapURLs = 50_000

// handleRobots keeps crawlers on public pages: project and owner pages,
// search and the home page, not accounts, forms or the API.
func (s *server) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	fmt.Fprintf(w, `User-agent: *
Disallow: /account
Disallow: /admin
Disallow: /api/
Disallow: /-/
Disallow: /login
Disallow: /signup
Disallow: /forgot-password
Disallow: /reset-password
Disallow: /report

Sitemap: %s/sitemap.xml
`, s.siteURL)
}

// handleSitemap lists the home page and every module's project page, with
// its latest release date, so search engines find new modules quickly.
func (s *server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	listings, err := s.registry.RecentlyUpdated(r.Context(), maxSitemapURLs-1, 0)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	type entry struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod,omitempty"`
	}
	set := struct {
		XMLName xml.Name `xml:"urlset"`
		NS      string   `xml:"xmlns,attr"`
		URLs    []entry  `xml:"url"`
	}{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	set.URLs = append(set.URLs, entry{Loc: s.siteURL + "/"})
	for _, l := range listings {
		set.URLs = append(set.URLs, entry{Loc: s.siteURL + s.project.URL(l.Path, ""), LastMod: l.PublishedAt.UTC().Format(time.DateOnly)})
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	fmt.Fprint(w, xml.Header)
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(set); err != nil {
		s.log.Warn("write sitemap", "err", err)
	}
}

// handleHealthz reports whether the server can reach its database, for
// load balancers and container health checks.
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var one int
	if err := s.registry.DB.QueryRowContext(r.Context(), `SELECT 1`).Scan(&one); err != nil {
		s.log.Error("health check: database", "err", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintln(w, "database unavailable")
		return
	}
	fmt.Fprintln(w, "ok")
}
