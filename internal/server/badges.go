package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"github.com/codebled/gopherdex/internal/badge"
	"github.com/codebled/gopherdex/internal/module"
	"github.com/codebled/gopherdex/internal/registry"
)

// badgeKinds are the badges a module can show, with their default labels.
var badgeKinds = map[string]string{
	"version":   "gopherdex",
	"downloads": "downloads",
	"verified":  "source",
	"security":  "security",
}

// handleBadge draws a README badge: /badge/<owner>/<module>[/vN].svg with
// ?type=version (default), downloads, verified or security, and an
// optional ?label=.
func (s *server) handleBadge(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutSuffix(r.PathValue("path"), ".svg")
	kind := r.URL.Query().Get("type")
	if kind == "" {
		kind = "version"
	}
	label, known := badgeKinds[kind]
	if !ok || !known {
		s.notFound(w, r)
		return
	}
	if l := strings.TrimSpace(r.URL.Query().Get("label")); l != "" && len([]rune(l)) <= 40 && !strings.ContainsFunc(l, unicode.IsControl) {
		label = l
	}
	b := s.moduleBadge(r.Context(), s.moduleHost+"/"+name, kind)
	b.Label = label
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	// READMEs on GitHub fetch badges through its image proxy, which honors
	// this; five minutes keeps them fresh without hammering the registry.
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(b.SVG())
}

func (s *server) moduleBadge(ctx context.Context, modPath, kind string) badge.Badge {
	m, err := s.registry.Module(ctx, modPath)
	if errors.Is(err, module.ErrNotFound) {
		return badge.Badge{Value: "not found", Style: badge.Neutral}
	}
	if err != nil {
		s.log.Warn("badge", "module", modPath, "err", err)
		return badge.Badge{Value: "unavailable", Style: badge.Neutral}
	}
	latest := latestInstallable(m)
	switch kind {
	case "downloads":
		stats, err := s.registry.ModuleDownloads(ctx, modPath)
		if err != nil {
			return badge.Badge{Value: "unavailable", Style: badge.Neutral}
		}
		return badge.Badge{Value: badge.Count(stats.LastMonth) + "/month", Style: badge.Dark}
	case "verified":
		if latest != nil && latest.Provenance != nil {
			return badge.Badge{Value: "verified", Style: badge.Good}
		}
		return badge.Badge{Value: "not verified", Style: badge.Neutral}
	case "security":
		advisories, err := s.registry.ModuleAdvisories(ctx, modPath)
		if err != nil || latest == nil {
			return badge.Badge{Value: "unavailable", Style: badge.Neutral}
		}
		n := 0
		for _, a := range advisories {
			if a.Affects(latest.Version) {
				n++
			}
		}
		switch n {
		case 0:
			return badge.Badge{Value: "no known issues", Style: badge.Good}
		case 1:
			return badge.Badge{Value: "1 advisory", Style: badge.Bad}
		default:
			return badge.Badge{Value: badge.Count(n) + " advisories", Style: badge.Bad}
		}
	default:
		if latest == nil {
			return badge.Badge{Value: "yanked", Style: badge.Neutral}
		}
		return badge.Badge{Value: latest.Version, Style: badge.Blue}
	}
}

// latestInstallable is the version go get @latest picks from the
// registry: the highest release that isn't yanked.
func latestInstallable(m *registry.ModuleDetail) *registry.VersionDetail {
	var versions []string
	for _, v := range m.Versions {
		if !v.Yanked {
			versions = append(versions, v.Version)
		}
	}
	latest := module.Latest(versions)
	for i := range m.Versions {
		if m.Versions[i].Version == latest {
			return &m.Versions[i]
		}
	}
	return nil
}
