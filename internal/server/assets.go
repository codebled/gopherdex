package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
	"sync"

	"github.com/codebled/gopherdex/internal/tokens"
	"github.com/codebled/gopherdex/web"
)

// Static files are linked with a fingerprint of their content
// (/static/app.css?v=3f9a2c1d0e), so browsers can cache them for a year and
// still pick up a new logo or stylesheet the moment it changes: a changed
// file has a new URL.

var (
	assetOnce     sync.Once
	assetVersions map[string]string // "app.css" → "3f9a2c1d0e"
	tokensVersion string
)

func loadAssetVersions() {
	assetOnce.Do(func() {
		assetVersions = map[string]string{}
		static, err := fs.Sub(web.FS, "static")
		if err != nil {
			return
		}
		// The files are embedded, so reading them does not fail in practice; if
		// it did, the remaining assets would be served without a version.
		_ = fs.WalkDir(static, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := fs.ReadFile(static, p)
			if err != nil {
				return err
			}
			assetVersions[p] = fingerprint(data)
			return nil
		})
		tokensVersion = fingerprint([]byte(tokens.CSS()))
	})
}

func fingerprint(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:5])
}

// assetURL is the fingerprinted URL of a static file, for templates:
// {{asset "app.css"}}. favicon.ico lives at the root, where browsers
// also look for it on their own.
func assetURL(name string) string {
	loadAssetVersions()
	if name == "tokens.css" {
		return "/tokens.css?v=" + tokensVersion
	}
	prefix := "/static/"
	if name == "favicon.ico" {
		prefix = "/"
	}
	if v, ok := assetVersions[name]; ok {
		return prefix + name + "?v=" + v
	}
	return prefix + name
}

// staticCache sets caching headers for static files: a year when the URL
// carries the file's current fingerprint, and "check every time" when it
// doesn't, so an old unversioned link never pins a stale file.
func staticCache(name string, w http.ResponseWriter, r *http.Request) bool {
	loadAssetVersions()
	v := assetVersions[name]
	etag := `"` + v + `"`
	w.Header().Set("ETag", etag)
	if v != "" && r.URL.Query().Get("v") == v {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if match := r.Header.Get("If-None-Match"); v != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

// serveStatic serves /static/ with fingerprint-aware caching.
func serveStatic(static fs.FS) http.Handler {
	files := http.FileServerFS(static)
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if staticCache(strings.TrimPrefix(r.URL.Path, "/"), w, r) {
			return
		}
		if strings.HasSuffix(r.URL.Path, ".webmanifest") {
			w.Header().Set("Content-Type", "application/manifest+json")
		}
		files.ServeHTTP(w, r)
	}))
}
