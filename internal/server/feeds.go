package server

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/module"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
)

// Atom feeds, like PyPI's RSS feeds, for following new releases in a feed
// reader:
//
//	/feeds/releases.atom            every new release
//	/feeds/new.atom                 new modules
//	/feeds/<owner>.atom             releases by a user or organization
//	/feeds/<owner>/<module>.atom    releases of one module (…/<module>/v2.atom for v2)
const feedSize = 50

// feedLink is advertised in a page's <head> for feed readers.
type feedLink struct{ Title, URL string }

func (d indexData) Feed() *feedLink {
	return &feedLink{"New releases on Gopherdex", "/feeds/releases.atom"}
}
func (d ownerData) Feed() *feedLink {
	return &feedLink{"Releases by @" + d.Namespace, "/feeds/" + d.Namespace + ".atom"}
}
func (d projectData) Feed() *feedLink {
	return &feedLink{"Releases of " + d.Path, "/feeds" + d.URL + ".atom"}
}

type atomFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	Title   string      `xml:"title"`
	ID      string      `xml:"id"`
	Updated string      `xml:"updated"`
	Links   []atomLink  `xml:"link"`
	Author  atomAuthor  `xml:"author"`
	Entries []atomEntry `xml:"entry"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr,omitempty"`
	Href string `xml:"href,attr"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

type atomEntry struct {
	Title   string      `xml:"title"`
	ID      string      `xml:"id"`
	Updated string      `xml:"updated"`
	Link    atomLink    `xml:"link"`
	Author  *atomAuthor `xml:"author,omitempty"`
	Summary string      `xml:"summary,omitempty"`
}

func (s *server) handleFeed(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutSuffix(r.PathValue("path"), ".atom")
	if !ok || name == "" {
		s.notFound(w, r)
		return
	}
	ctx := r.Context()
	feed := atomFeed{ID: s.siteURL + r.URL.Path, Author: atomAuthor{"Gopherdex"}}
	var page string
	var releases []registry.Release
	var err error
	switch {
	case name == "releases":
		feed.Title, page = "New releases on Gopherdex", "/"
		releases, err = s.registry.Releases(ctx, "", "", feedSize)
	case name == "new":
		feed.Title, page = "New modules on Gopherdex", "/"
		var listings []registry.Listing
		if listings, err = s.registry.NewModules(ctx, feedSize); err == nil {
			for _, l := range listings {
				releases = append(releases, registry.Release{Path: l.Path, Version: l.Version, Synopsis: l.Synopsis, PublishedAt: l.CreatedAt})
			}
		}
	case !strings.Contains(name, "/"):
		exists, e := s.registry.NamespaceExists(ctx, name)
		if e != nil || !exists {
			err = errors.Join(e, module.ErrNotFound)
			break
		}
		feed.Title, page = "Releases by @"+name, "/"+name
		releases, err = s.registry.Releases(ctx, name, "", feedSize)
	default:
		modPath := s.moduleHost + "/" + name
		if _, err = s.registry.Module(ctx, modPath); err != nil {
			break // not found or quarantined
		}
		feed.Title, page = "Releases of "+modPath, s.project.URL(modPath, "")
		releases, err = s.registry.Releases(ctx, "", modPath, feedSize)
	}
	switch {
	case errors.Is(err, module.ErrNotFound):
		s.notFound(w, r)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}

	feed.Links = []atomLink{{Rel: "self", Href: s.siteURL + r.URL.Path}, {Rel: "alternate", Href: s.siteURL + page}}
	updated := time.Unix(0, 0).UTC()
	for _, rel := range releases {
		if rel.PublishedAt.After(updated) {
			updated = rel.PublishedAt
		}
		e := atomEntry{
			Title:   rel.Path + " " + rel.Version,
			ID:      s.siteURL + s.project.URL(rel.Path, rel.Version),
			Updated: rel.PublishedAt.Format(time.RFC3339),
			Link:    atomLink{Rel: "alternate", Href: s.siteURL + s.project.URL(rel.Path, rel.Version)},
			Summary: rel.Synopsis,
		}
		switch {
		case rel.Trusted:
			e.Author = &atomAuthor{"GitHub Actions"}
		case rel.PublishedBy != "":
			e.Author = &atomAuthor{"@" + rel.PublishedBy}
		}
		if name == "new" {
			e.Title = rel.Path
			e.ID = s.siteURL + s.project.URL(rel.Path, "")
			e.Link.Href = e.ID
		}
		feed.Entries = append(feed.Entries, e)
	}
	feed.Updated = updated.Format(time.RFC3339)

	w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	fmt.Fprint(w, xml.Header)
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(feed); err != nil {
		s.log.Warn("write feed", "path", r.URL.Path, "err", err)
	}
}
