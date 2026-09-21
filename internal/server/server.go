// Package server wires the HTTP routes: the landing page, accounts, the
// discovery JSON API, the GOPROXY endpoints and static assets.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/discovery"
	"github.com/parthiban-sivakumar/gopherdex/internal/module"
	"github.com/parthiban-sivakumar/gopherdex/internal/oidc"
	"github.com/parthiban-sivakumar/gopherdex/internal/project"
	"github.com/parthiban-sivakumar/gopherdex/internal/ratelimit"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
	"github.com/parthiban-sivakumar/gopherdex/internal/tokens"
	"github.com/parthiban-sivakumar/gopherdex/internal/vulndb"
	"github.com/parthiban-sivakumar/gopherdex/web"
)

const (
	proxyPrefix   = "/api/proxy"
	maxQueryLen   = 200
	moduleTimeout = 20 * time.Second
)

// Config holds the server's dependencies.
type Config struct {
	Discovery *discovery.Service
	Accounts  *accounts.Service
	Proxy     http.Handler // GOPROXY handler; request paths arrive without proxyPrefix
	Log       *slog.Logger

	Registry *registry.Registry

	// SiteURL is the public URL of the site, e.g. https://gopherdex.dev.
	SiteURL string
	// ModuleHost is the domain modules are published under, e.g.
	// "gopherdex.dev" for gopherdex.dev/<owner>/<module>.
	ModuleHost string
	// SecureCookies marks session cookies Secure. Enable it whenever the
	// site is served over HTTPS.
	SecureCookies bool
	// TrustProxy reads the client IP from the last X-Forwarded-For entry.
	// Enable it only behind a reverse proxy that sets that header.
	TrustProxy bool
	// Mirror, when set, is told about each new version so the public Go
	// module mirror and checksum database pick it up right away.
	Mirror interface{ Notify(modPath, version string) }
	// Admins are the usernames allowed into the review queue at /admin.
	Admins []string
	// GitHubOIDC verifies GitHub Actions ID tokens for trusted publishing.
	// Nil turns trusted publishing off.
	GitHubOIDC *oidc.Verifier
	// VulnDB is a copy of the public Go vulnerability database, merged into
	// /vulndb and used to flag vulnerable dependencies. Nil when offline.
	VulnDB *vulndb.Upstream
	// PlaygroundURL is the Go Playground that "Run" buttons on examples
	// share code with, e.g. https://play.golang.org. Empty hides them.
	PlaygroundURL string
}

type server struct {
	disc          *discovery.Service
	accounts      *accounts.Service
	log           *slog.Logger
	pages         map[string]*template.Template
	tokensCSS     []byte
	tokensTag     string
	registry      *registry.Registry
	project       *project.Service
	siteURL       string
	moduleHost    string
	zeroConfig    bool // modules install with plain go get
	secureCookies bool
	trustProxy    bool
	mirror        interface{ Notify(modPath, version string) }

	loginLimit  *ratelimit.Limiter
	signupLimit *ratelimit.Limiter
	emailLimit  *ratelimit.Limiter
	uploadLimit *ratelimit.Limiter
	resetLimit  *ratelimit.Limiter
	reportLimit *ratelimit.Limiter
	searchLimit *ratelimit.Limiter
	mintLimit   *ratelimit.Limiter

	admins       map[string]bool
	githubOIDC   *oidc.Verifier
	vulnUpstream *vulndb.Upstream

	playground    bool
	playgroundURL string
}

// New returns the application's root handler.
func New(cfg Config) (http.Handler, error) {
	if cfg.Discovery == nil || cfg.Proxy == nil || cfg.Accounts == nil || cfg.Registry == nil {
		return nil, errors.New("server: Discovery, Accounts, Registry and Proxy are required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.ModuleHost == "" || cfg.SiteURL == "" {
		return nil, errors.New("server: ModuleHost and SiteURL are required")
	}
	pages, err := parsePages()
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(web.FS, "static")
	if err != nil {
		return nil, fmt.Errorf("static assets: %w", err)
	}
	favicon, err := fs.ReadFile(static, "favicon.ico")
	if err != nil {
		return nil, fmt.Errorf("favicon: %w", err)
	}
	css := []byte(tokens.CSS())
	sum := sha256.Sum256(css)

	s := &server{
		disc:          cfg.Discovery,
		accounts:      cfg.Accounts,
		log:           cfg.Log,
		pages:         pages,
		tokensCSS:     css,
		tokensTag:     `"` + hex.EncodeToString(sum[:8]) + `"`,
		registry:      cfg.Registry,
		project:       &project.Service{Registry: cfg.Registry, ModuleHost: cfg.ModuleHost, SiteURL: strings.TrimSuffix(cfg.SiteURL, "/"), Log: cfg.Log},
		siteURL:       strings.TrimSuffix(cfg.SiteURL, "/"),
		moduleHost:    cfg.ModuleHost,
		zeroConfig:    !registry.IsLocalHost(cfg.ModuleHost) && strings.HasPrefix(cfg.SiteURL, "https://"),
		secureCookies: cfg.SecureCookies,
		trustProxy:    cfg.TrustProxy,
		mirror:        cfg.Mirror,
		loginLimit:    ratelimit.New(10, 5*time.Minute),
		signupLimit:   ratelimit.New(5, time.Hour),
		emailLimit:    ratelimit.New(3, time.Hour),
		uploadLimit:   ratelimit.New(60, time.Hour),
		resetLimit:    ratelimit.New(5, time.Hour),
		reportLimit:   ratelimit.New(10, 24*time.Hour),
		searchLimit:   ratelimit.New(120, time.Minute),
		mintLimit:     ratelimit.New(60, time.Hour),
		admins:        map[string]bool{},
		githubOIDC:    cfg.GitHubOIDC,
		vulnUpstream:  cfg.VulnDB,
		playground:    cfg.PlaygroundURL != "",
		playgroundURL: strings.TrimSuffix(cfg.PlaygroundURL, "/"),
	}
	for _, a := range cfg.Admins {
		s.admins[accounts.NormalizeUsername(a)] = true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /search", s.limited(s.handleSearchPage))
	mux.HandleFunc("GET /modules", s.handleModules)
	mux.HandleFunc("GET /", s.handlePath) // owners, project pages, go-import tags

	mux.HandleFunc("GET /signup", s.handleSignupForm)
	mux.HandleFunc("POST /signup", s.handleSignup)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /verify-email", s.handleVerifyEmail)
	mux.HandleFunc("GET /forgot-password", s.handleForgotForm)
	mux.HandleFunc("POST /forgot-password", s.handleForgot)
	mux.HandleFunc("GET /reset-password", s.handleResetForm)
	mux.HandleFunc("POST /reset-password", s.handleReset)
	mux.HandleFunc("GET /login/2fa", s.handleTwoFactorForm)
	mux.HandleFunc("POST /login/2fa", s.handleTwoFactor)
	mux.HandleFunc("GET /account/security", s.handleSecurity)
	mux.HandleFunc("POST /account/security/2fa", s.handleConfirmTOTP)
	mux.HandleFunc("POST /account/security/2fa/disable", s.handleDisableTOTP)
	mux.HandleFunc("POST /account/security/password", s.handleChangePassword)
	mux.HandleFunc("POST /account/security/sessions", s.handleSignOutOthers)
	mux.HandleFunc("GET /report", s.handleReportForm)
	mux.HandleFunc("POST /report", s.handleReport)
	mux.HandleFunc("GET /admin", s.handleAdmin)
	mux.HandleFunc("POST /-/admin/dismiss", s.handleDismissReport)
	mux.HandleFunc("POST /-/admin/quarantine", s.handleQuarantine)
	mux.HandleFunc("POST /-/admin/release", s.handleRelease)
	mux.HandleFunc("GET /account", s.handleAccount)
	mux.HandleFunc("POST /account/email/resend", s.handleResendVerification)
	mux.HandleFunc("POST /account/tokens", s.handleCreateToken)
	mux.HandleFunc("POST /account/email", s.handleChangeEmail)
	mux.HandleFunc("POST /account/preferences", s.handlePreferences)
	mux.HandleFunc("POST /account/delete", s.handleDeleteAccount)
	mux.HandleFunc("POST /account/tokens/{id}/revoke", s.handleRevokeToken)

	mux.HandleFunc("POST /-/yank", s.handleYank)
	mux.HandleFunc("POST /-/unyank", s.handleUnyank)
	mux.HandleFunc("POST /-/deprecate", s.handleDeprecate)
	mux.HandleFunc("POST /-/undeprecate", s.handleUndeprecate)
	mux.HandleFunc("POST /-/collaborators", s.handleSetCollaborator)
	mux.HandleFunc("POST /-/collaborators/remove", s.handleRemoveCollaborator)
	mux.HandleFunc("POST /-/orgs", s.handleCreateOrg)
	mux.HandleFunc("POST /-/orgs/members", s.handleSetOrgMember)
	mux.HandleFunc("POST /-/orgs/members/remove", s.handleRemoveOrgMember)
	mux.HandleFunc("POST /-/publishers", s.handleAddPublisher)
	mux.HandleFunc("POST /-/publishers/remove", s.handleRemovePublisher)

	mux.HandleFunc("GET /api/whoami", s.handleWhoami)
	mux.HandleFunc("POST /api/yank", s.handleAPIYank)
	mux.HandleFunc("POST /api/upload", s.handleUpload)
	mux.HandleFunc("GET /api/oidc/audience", s.handleOIDCAudience)
	mux.HandleFunc("POST /api/oidc/mint-token", s.limited(s.handleMintToken))
	mux.HandleFunc("GET /api/search", s.limited(s.handleSearch))
	mux.HandleFunc("GET /api/modules/{path...}", s.limited(s.handleModule))
	mux.Handle("GET "+proxyPrefix+"/", http.StripPrefix(proxyPrefix, cfg.Proxy))

	mux.HandleFunc("GET /tokens.css", s.handleTokens)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(favicon)
	})
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /robots.txt", s.handleRobots)
	mux.HandleFunc("GET /sitemap.xml", s.handleSitemap)
	mux.HandleFunc("GET /feeds/{path...}", s.limited(s.handleFeed))
	mux.HandleFunc("GET /vulndb/{path...}", s.handleVulnDB)
	mux.HandleFunc("GET /badge/{path...}", s.handleBadge)
	mux.HandleFunc("GET /api", s.handleAPIDocs)
	mux.HandleFunc("GET /api/v1/modules/{path...}", s.limited(s.handleV1Module))
	mux.HandleFunc("GET /api/v1/search", s.limited(s.handleV1Search))
	mux.HandleFunc("GET /api/v1/owners/{name}", s.limited(s.handleV1Owner))
	mux.HandleFunc("GET /api/v1/stats", s.limited(s.handleV1Stats))
	mux.HandleFunc("POST /-/play", s.limited(s.handlePlay))
	mux.HandleFunc("GET /advisories", s.handleAdvisories)
	mux.HandleFunc("GET /advisories/{id}", s.handleAdvisory)
	mux.HandleFunc("POST /-/advisories", s.handleCreateAdvisory)
	mux.HandleFunc("POST /-/advisories/{id}", s.handleUpdateAdvisory)
	mux.HandleFunc("POST /-/advisories/{id}/withdraw", s.handleWithdrawAdvisory)

	// Browsers send Sec-Fetch-Site/Origin on form posts, so cross-site
	// POSTs (CSRF) are rejected. API clients such as the CLI send neither
	// header and authenticate with a bearer token instead.
	csrf := http.NewCrossOriginProtection()
	csrf.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Cross-site request refused.", http.StatusForbidden)
	}))

	return s.logRequests(s.recoverPanics(securityHeaders(s.secureCookies, csrf.Handler(withUserCache(mux))))), nil
}

type indexData struct {
	ModuleHost string
	Stats      registry.Stats
	Recent     []registry.Listing
	New        []registry.Listing
	Popular    []registry.SearchHit
}

const showcaseSize = 8

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Old library-view links for modules published here go to their project page.
	if m := r.URL.Query().Get("m"); strings.HasPrefix(m, s.moduleHost+"/") && module.CheckPath(m) == nil {
		target := s.project.URL(m, "")
		if v := r.URL.Query().Get("v"); module.CheckVersion(v) == nil {
			target = s.project.URL(m, v)
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	// Searches used to live on the home page.
	if q := r.URL.Query().Get("q"); q != "" && r.URL.Query().Get("m") == "" {
		http.Redirect(w, r, "/search?"+url.Values{"q": {q}}.Encode(), http.StatusFound)
		return
	}
	ctx := r.Context()
	data := indexData{ModuleHost: s.moduleHost}
	var err error
	if data.Stats, err = s.registry.Stats(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	if data.Recent, err = s.registry.RecentlyUpdated(ctx, showcaseSize, 0); err != nil {
		s.serverError(w, r, err)
		return
	}
	if data.New, err = s.registry.NewModules(ctx, showcaseSize); err != nil {
		s.serverError(w, r, err)
		return
	}
	if data.Popular, err = s.registry.PopularModules(ctx, showcaseSize); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "index", "", data)
}

// handleModules is the old browse page; the search page now lists every
// module when there's no query.
func (s *server) handleModules(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/search?sort=updated", http.StatusMovedPermanently)
}

type searchData struct {
	Query, License, GoMax, Updated, Sort string
	HideDeprecated, Filtered, Public     bool
	Hits                                 []registry.SearchHit
	Total, Page, Pages                   int
	PrevURL, NextURL                     string
	Licenses, GoVersions                 []registry.Facet
	ModuleHost                           string
}

const resultsPerPage = 20

var updatedWindows = map[string]time.Duration{"month": 30 * 24 * time.Hour, "year": 365 * 24 * time.Hour}

// handleSearchPage is the registry's search and browse page. It works
// without JavaScript: filters and sorting are plain GET forms.
func (s *server) handleSearchPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	data := searchData{
		Query:          strings.TrimSpace(q.Get("q")),
		License:        q.Get("license"),
		GoMax:          q.Get("go"),
		Updated:        q.Get("updated"),
		Sort:           q.Get("sort"),
		HideDeprecated: q.Get("hide_deprecated") == "1",
		ModuleHost:     s.moduleHost,
		Public:         s.disc.Index != nil,
	}
	if len(data.Query) > maxQueryLen {
		data.Query = data.Query[:maxQueryLen]
	}
	if _, ok := updatedWindows[data.Updated]; !ok {
		data.Updated = ""
	}
	if registry.GoNum(data.GoMax) == 0 {
		data.GoMax = ""
	}
	switch registry.Sort(data.Sort) {
	case registry.SortDownloads, registry.SortUpdated, registry.SortNew:
	default:
		data.Sort = string(registry.SortRelevance)
		if data.Query == "" {
			data.Sort = string(registry.SortUpdated)
		}
	}
	data.Filtered = data.License != "" || data.GoMax != "" || data.Updated != "" || data.HideDeprecated
	data.Page, _ = strconv.Atoi(q.Get("page"))
	if data.Page < 1 {
		data.Page = 1
	}

	ctx := r.Context()
	hits, total, err := s.registry.Search(ctx, registry.SearchQuery{
		Text: data.Query, License: data.License, GoMax: data.GoMax, UpdatedWithin: updatedWindows[data.Updated],
		HideDeprecated: data.HideDeprecated, Sort: registry.Sort(data.Sort),
		Limit: resultsPerPage, Offset: (data.Page - 1) * resultsPerPage,
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data.Hits, data.Total = hits, total
	data.Pages = max(1, (total+resultsPerPage-1)/resultsPerPage)
	if data.Page > data.Pages {
		s.notFound(w, r)
		return
	}
	pageURL := func(page int) string {
		v := url.Values{}
		for _, k := range []string{"q", "license", "go", "updated", "sort", "hide_deprecated"} {
			if val := q.Get(k); val != "" {
				v.Set(k, val)
			}
		}
		v.Set("page", strconv.Itoa(page))
		return "/search?" + v.Encode()
	}
	if data.Page > 1 {
		data.PrevURL = pageURL(data.Page - 1)
	}
	if data.Page < data.Pages {
		data.NextURL = pageURL(data.Page + 1)
	}
	if data.Licenses, data.GoVersions, err = s.registry.Facets(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	title := "Search"
	if data.Query != "" {
		title = data.Query
	}
	s.render(w, r, http.StatusOK, "search", title, data)
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if len(q) > maxQueryLen {
		writeJSON(w, http.StatusBadRequest, apiError{fmt.Sprintf("Search text can be at most %d characters.", maxQueryLen)})
		return
	}
	scope := discovery.Scope(r.URL.Query().Get("scope"))
	if scope != discovery.ScopeAll && scope != discovery.ScopeHosted && scope != discovery.ScopePublic {
		writeJSON(w, http.StatusBadRequest, apiError{"scope must be hosted or public."})
		return
	}
	if scope == discovery.ScopeHosted {
		s.searchHostedJSON(w, r, q)
		return
	}
	resp, err := s.disc.Search(r.Context(), q, scope)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *server) handleModule(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), moduleTimeout)
	defer cancel()
	m, err := s.disc.Module(ctx, r.PathValue("path"), r.URL.Query().Get("version"))
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *server) handleTokens(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", s.tokensTag)
	if r.Header.Get("If-None-Match") == s.tokensTag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(s.tokensCSS)
}

type apiError struct {
	Error string `json:"error"`
}

func (s *server) apiError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, module.ErrInvalid):
		writeJSON(w, http.StatusBadRequest, apiError{"That isn't a valid module path or version. Module paths look like github.com/owner/repo."})
	case errors.Is(err, module.ErrNotFound):
		writeJSON(w, http.StatusNotFound, apiError{"No module or version matches that path, here or on the public Go proxy."})
	case errors.Is(err, discovery.ErrUpstream):
		s.log.Warn("upstream failure", "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusBadGateway, apiError{"The public Go module proxy isn't responding. Try again in a moment."})
	case errors.Is(err, context.DeadlineExceeded):
		s.log.Warn("request timed out", "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusGatewayTimeout, apiError{"Loading this module took too long. Try again in a moment."})
	case errors.Is(err, context.Canceled):
		// The client went away; nobody is reading the response.
	default:
		s.log.Error("api error", "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusInternalServerError, apiError{"Something went wrong on the server."})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	w.Write(body)
}

func securityHeaders(https bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if https {
			// Browsers that have seen the site once never fall back to
			// plain http, where a session cookie or token could leak.
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' https://fonts.googleapis.com; "+
			"font-src https://fonts.gstatic.com; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self' https://go.dev")
		next.ServeHTTP(w, r)
	})
}

func (s *server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v) // deliberate abort; net/http closes the connection quietly
			}
			s.log.Error("panic serving request", "method", r.Method, "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			level := slog.LevelInfo
			if status >= 500 {
				level = slog.LevelError
			}
			s.log.Log(r.Context(), level, "request",
				"method", r.Method, "path", r.URL.Path, "status", status,
				"bytes", rec.bytes, "duration", time.Since(start).Round(time.Microsecond))
		}()
		next.ServeHTTP(rec, r)
	})
}

// searchHostedJSON answers /api/search?scope=hosted from the search index.
func (s *server) searchHostedJSON(w http.ResponseWriter, r *http.Request, q string) {
	resp := discovery.SearchResponse{Query: strings.TrimSpace(q), Results: []discovery.Result{}}
	if resp.Query != "" {
		hits, _, err := s.registry.Search(r.Context(), registry.SearchQuery{Text: resp.Query, Sort: registry.SortRelevance, Limit: 20})
		if err != nil {
			s.apiError(w, r, err)
			return
		}
		for _, h := range hits {
			resp.Results = append(resp.Results, discovery.Result{Path: h.Path, Version: h.Version, Synopsis: h.Synopsis, Origin: discovery.Hosted})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// limited applies the per-IP search limit to expensive read endpoints. The
// GOPROXY endpoints are left unlimited: the go command and the public module
// mirror fetch many files at once.
func (s *server) limited(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.searchLimit.Allow(s.clientOf(r).IP) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Too many requests. Wait a minute and try again.", http.StatusTooManyRequests)
			return
		}
		h(w, r)
	}
}
