// Package goproxy implements both sides of the GOPROXY protocol described by
// `go help goproxy`: a Handler that serves modules from a Source, and a
// Client that reads modules from a remote proxy such as proxy.golang.org.
package goproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/codebled/gopherdex/internal/module"
)

// Source is where the Handler finds modules. *store.Store implements it.
type Source interface {
	Versions(ctx context.Context, modPath string) ([]string, error)
	Info(ctx context.Context, modPath, version string) (module.Info, error)
	Latest(ctx context.Context, modPath string) (module.Info, error)
	GoMod(ctx context.Context, modPath, version string) ([]byte, error)
	Zip(ctx context.Context, modPath, version string) (io.WriterTo, error)
}

// Handler serves the GOPROXY endpoints. Mount it with the URL prefix
// stripped, so request paths look like "example.com/hello/@v/list":
//
//	<module>/@v/list               tagged versions, one per line
//	<module>/@v/<version>.info     JSON: Version, Time, Origin
//	<module>/@v/<version>.mod      the go.mod file
//	<module>/@v/<version>.zip      the module source zip
//	<module>/@latest               JSON info of the latest version
//
// Module paths contain slashes, so Go 1.22 mux wildcards cannot capture them
// ({module} matches one segment and "{version}.info" is not a valid
// pattern). The handler therefore parses the path itself.
type Handler struct {
	src Source
	log *slog.Logger

	// OnZip, when set, is called after a module zip was sent completely,
	// for download counting.
	OnZip func(modPath, version string)
}

// NewHandler returns a Handler serving modules from src.
func NewHandler(src Source, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{src: src, log: log}
}

type endpoint int

const (
	endpointList endpoint = iota
	endpointInfo
	endpointMod
	endpointZip
	endpointLatest
)

type request struct {
	endpoint endpoint
	module   string
	version  string
}

var errUnknownEndpoint = errors.New("unknown proxy endpoint")

// Cache policy: a published version never changes, but the version list and
// @latest do.
const (
	cacheImmutable = "public, max-age=31536000, immutable"
	cacheMutable   = "no-cache"
)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, err := parseRequest(strings.TrimPrefix(r.URL.Path, "/"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ctx := r.Context()

	switch req.endpoint {
	case endpointList:
		versions, err := h.src.Versions(ctx, req.module)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		var body bytes.Buffer
		for _, v := range versions {
			body.WriteString(v)
			body.WriteByte('\n')
		}
		write(w, "text/plain; charset=utf-8", cacheMutable, body.Bytes())

	case endpointInfo, endpointLatest:
		var info module.Info
		cache := cacheImmutable
		if req.endpoint == endpointLatest {
			info, err = h.src.Latest(ctx, req.module)
			cache = cacheMutable
		} else {
			info, err = h.src.Info(ctx, req.module, req.version)
		}
		if err != nil {
			h.fail(w, r, err)
			return
		}
		body, err := json.Marshal(info)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		write(w, "application/json", cache, body)

	case endpointMod:
		body, err := h.src.GoMod(ctx, req.module, req.version)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		write(w, "text/plain; charset=utf-8", cacheImmutable, body)

	case endpointZip:
		z, err := h.src.Zip(ctx, req.module, req.version)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Cache-Control", cacheImmutable)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			// WriteTo is what closes the zip, and a HEAD never calls it.
			if c, ok := z.(io.Closer); ok {
				c.Close()
			}
			return
		}
		if n, err := z.WriteTo(w); err != nil {
			// The status line is already sent. Abort the connection so the
			// go command sees a failed download instead of a truncated zip.
			h.log.Error("goproxy: zip stream failed",
				"module", req.module, "version", req.version, "bytes_sent", n, "err", err)
			panic(http.ErrAbortHandler)
		}
		if h.OnZip != nil {
			h.OnZip(req.module, req.version)
		}
	}
}

// parseRequest splits "<escaped module>/@v/<file>" or
// "<escaped module>/@latest" and validates both parts.
func parseRequest(p string) (request, error) {
	if escMod, ok := strings.CutSuffix(p, "/@latest"); ok {
		mod, err := module.UnescapePath(escMod)
		return request{endpoint: endpointLatest, module: mod}, err
	}
	i := strings.LastIndex(p, "/@v/")
	if i < 0 {
		return request{}, errUnknownEndpoint
	}
	mod, err := module.UnescapePath(p[:i])
	if err != nil {
		return request{}, err
	}
	req := request{module: mod}
	file := p[i+len("/@v/"):]
	if file == "list" {
		req.endpoint = endpointList
		return req, nil
	}
	ext := path.Ext(file)
	switch ext {
	case ".info":
		req.endpoint = endpointInfo
	case ".mod":
		req.endpoint = endpointMod
	case ".zip":
		req.endpoint = endpointZip
	default:
		return request{}, errUnknownEndpoint
	}
	req.version, err = module.UnescapeVersion(strings.TrimSuffix(file, ext))
	return req, err
}

func write(w http.ResponseWriter, contentType, cacheControl string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(body)
}

// fail maps errors to the status codes the go command understands. 404 and
// 410 mean "not here", which lets it try the next proxy listed in GOPROXY;
// anything else stops the download.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, module.ErrNotFound), errors.Is(err, errUnknownEndpoint):
		h.log.Debug("goproxy: not found", "path", r.URL.Path, "err", err)
		http.Error(w, "not found: "+err.Error(), http.StatusNotFound)
	case errors.Is(err, module.ErrInvalid):
		h.log.Warn("goproxy: bad request", "path", r.URL.Path, "err", err)
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
	case errors.Is(err, context.Canceled):
		h.log.Debug("goproxy: client went away", "path", r.URL.Path)
	default:
		h.log.Error("goproxy: internal error", "path", r.URL.Path, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
