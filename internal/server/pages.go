package server

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/project"
	"github.com/parthiban-sivakumar/gopherdex/internal/version"
	"github.com/parthiban-sivakumar/gopherdex/web"
)

// page is the data every template receives. Data holds the page's own
// fields.
type page struct {
	Title      string
	User       *accounts.User
	Data       any
	ProxyURL   string
	ModuleHost string
	ZeroConfig bool
}

// parsePages builds one template set per page: the shared layout plus that
// page's "content" block.
func parsePages() (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"cliInstall": func() string { return version.CLIInstall },
		"host":       func(p string) string { h, _, _ := strings.Cut(p, "/"); return h },
		"docBlocks":  project.DocBlocks,
		"since":      project.Since,
		"bytes":      humanBytes,
		"pkgID":      pkgID,
		"join":       strings.Join,
		"number":     formatNumber,
		"hasPrefix":  strings.HasPrefix,
		"trimPrefix": strings.TrimPrefix,
		// breakable lets long module paths wrap only after a slash.
		"breakable": func(p string) template.HTML {
			return template.HTML(strings.ReplaceAll(template.HTMLEscapeString(p), "/", "/<wbr>"))
		},
		"short": func(s string, n int) string {
			if len(s) > n {
				return s[:n]
			}
			return s
		},
		// projectURL links a module: its project page when published under
		// moduleHost, otherwise the search page's library view.
		"projectURL": func(moduleHost, modPath string) string {
			if rest, ok := strings.CutPrefix(modPath, moduleHost); ok && strings.HasPrefix(rest, "/") {
				return rest
			}
			return "/?" + url.Values{"m": {modPath}}.Encode()
		},
		"projectPage": func(moduleHost, modPath string) bool {
			return strings.HasPrefix(modPath, moduleHost+"/")
		},
		"relPath": func(modPath, importPath string) string {
			if rel := strings.TrimPrefix(strings.TrimPrefix(importPath, modPath), "/"); rel != "" {
				return rel
			}
			return "(root)"
		},
	}
	layout, err := template.New("").Funcs(funcs).ParseFS(web.FS, "templates/layout/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse layout templates: %w", err)
	}
	files, err := fs.Glob(web.FS, "templates/pages/*.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*template.Template{}
	for _, file := range files {
		t, err := layout.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := t.ParseFS(web.FS, file); err != nil {
			return nil, fmt.Errorf("parse %s: %w", file, err)
		}
		pages[strings.TrimSuffix(path.Base(file), ".html")] = t
	}
	return pages, nil
}

// render executes a page into a buffer first, so a template error produces
// a clean 500 instead of half a page.
func (s *server) render(w http.ResponseWriter, r *http.Request, status int, name, title string, data any) {
	t, ok := s.pages[name]
	if !ok {
		s.serverError(w, r, fmt.Errorf("no page template %q", name))
		return
	}
	user := s.currentUser(r)
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", page{Title: title, User: user, Data: data, ProxyURL: s.siteURL + proxyPrefix, ModuleHost: s.moduleHost, ZeroConfig: s.zeroConfig}); err != nil {
		s.serverError(w, r, fmt.Errorf("render %s: %w", name, err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if user != nil {
		w.Header().Set("Cache-Control", "no-store") // personalized
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (s *server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, r.Context().Err()) && r.Context().Err() != nil {
		return // client went away
	}
	s.log.Error("internal error", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "Something went wrong on the server.", http.StatusInternalServerError)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// formatNumber writes 12345 as "12,345".
func formatNumber(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + formatNumber(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
