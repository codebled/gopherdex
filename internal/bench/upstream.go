// Package bench builds large, realistic datasets for Gopherdex and drives
// load against a running server, to measure how it behaves at scale.
//
// Seeding samples real modules from the Go module index and mirror
// (index.golang.org, proxy.golang.org), remaps them under the registry's own
// module host, pads the set with synthetic modules, and publishes it all
// through Registry.Publish, the same code path uploads take. Everything
// fetched upstream is cached on disk, so a dataset can be rebuilt without
// downloading it again.
package bench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	xmodule "golang.org/x/mod/module"
)

const userAgent = "gopherdex-bench (+https://github.com/parthiban-sivakumar/gopherdex)"

// Upstream reads the public module index and mirror, caching every
// response under Cache.
type Upstream struct {
	Index string // e.g. https://index.golang.org
	Proxy string // e.g. https://proxy.golang.org
	Cache string
	HTTP  *http.Client

	// MaxZip skips module zips larger than this, in bytes.
	MaxZip int64
}

// IndexEntry is one line of the module index: a version the mirror saw.
type IndexEntry struct {
	Path      string
	Version   string
	Timestamp time.Time
}

var errTooLarge = errors.New("zip too large")

// IndexPage returns up to 2000 index entries from since onwards.
func (u *Upstream) IndexPage(ctx context.Context, since time.Time) ([]IndexEntry, error) {
	key := filepath.Join(u.Cache, "index", since.UTC().Format("20060102T150405.000000000Z")+".jsonl")
	src := u.Index + "/index?limit=2000&since=" + url.QueryEscape(since.UTC().Format(time.RFC3339Nano))
	body, err := u.cached(ctx, key, src, 0)
	if err != nil {
		return nil, err
	}
	var out []IndexEntry
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e IndexEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("index %s: %w", since, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// GoMod returns a version's go.mod file.
func (u *Upstream) GoMod(ctx context.Context, modPath, version string) ([]byte, error) {
	rel, err := proxyPath(modPath, version, ".mod")
	if err != nil {
		return nil, err
	}
	return u.cached(ctx, filepath.Join(u.Cache, "proxy", filepath.FromSlash(rel)), u.Proxy+"/"+rel, 0)
}

// Zip returns the path of a version's module zip in the cache, downloading
// it first if needed. Zips over MaxZip return errTooLarge.
func (u *Upstream) Zip(ctx context.Context, modPath, version string) (string, error) {
	rel, err := proxyPath(modPath, version, ".zip")
	if err != nil {
		return "", err
	}
	file := filepath.Join(u.Cache, "proxy", filepath.FromSlash(rel))
	if _, err := os.Stat(file); err == nil {
		return file, nil
	}
	if _, err := os.Stat(file + ".toolarge"); err == nil {
		return "", errTooLarge
	}
	if _, err := u.cached(ctx, file, u.Proxy+"/"+rel, u.MaxZip); err != nil {
		if errors.Is(err, errTooLarge) {
			os.WriteFile(file+".toolarge", nil, 0o644)
		}
		return "", err
	}
	return file, nil
}

func proxyPath(modPath, version, ext string) (string, error) {
	p, err := xmodule.EscapePath(modPath)
	if err != nil {
		return "", err
	}
	v, err := xmodule.EscapeVersion(version)
	if err != nil {
		return "", err
	}
	return p + "/@v/" + v + ext, nil
}

// cached returns the file at key, or fetches src into it. limit > 0 caps
// the response size.
func (u *Upstream) cached(ctx context.Context, key, src string, limit int64) ([]byte, error) {
	if b, err := os.ReadFile(key); err == nil {
		return b, nil
	}
	var body []byte
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt*attempt) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		body, err = u.get(ctx, src, limit)
		if err == nil || !retryable(err) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(key), 0o755); err != nil {
		return nil, err
	}
	tmp := key + ".part"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return nil, err
	}
	return body, os.Rename(tmp, key)
}

type statusError struct {
	url  string
	code int
}

func (e *statusError) Error() string { return fmt.Sprintf("GET %s: %d", e.url, e.code) }

func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusTooManyRequests || se.code >= 500
	}
	return !errors.Is(err, errTooLarge) && !errors.Is(err, context.Canceled)
}

func (u *Upstream) get(ctx context.Context, src string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, &statusError{src, resp.StatusCode}
	}
	if limit > 0 && resp.ContentLength > limit {
		return nil, errTooLarge
	}
	r := io.Reader(resp.Body)
	if limit > 0 {
		r = io.LimitReader(resp.Body, limit+1)
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if limit > 0 && int64(len(body)) > limit {
		return nil, errTooLarge
	}
	return body, nil
}
