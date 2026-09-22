package goproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/module"
)

const (
	maxListSize = 4 << 20
	maxInfoSize = 64 << 10
	modSizeCap  = 16 << 20

	mutableTTL   = 5 * time.Minute // version lists and @latest
	immutableTTL = time.Hour       // .info and .mod of a published version
)

// Client reads module metadata from a remote GOPROXY. Responses are cached
// in memory.
type Client struct {
	base  string
	http  *http.Client
	cache *ttlCache
}

// NewClient returns a Client for the proxy at base, e.g.
// "https://proxy.golang.org".
func NewClient(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{
		base:  strings.TrimSuffix(base, "/"),
		http:  hc,
		cache: &ttlCache{items: map[string]cacheItem{}, max: 4096, maxBytes: 64 << 20},
	}
}

// Versions returns the tagged versions of a module, lowest first.
func (c *Client) Versions(ctx context.Context, modPath string) ([]string, error) {
	esc, err := module.EscapePath(modPath)
	if err != nil {
		return nil, err
	}
	body, err := c.get(ctx, esc+"/@v/list", mutableTTL, maxListSize)
	if err != nil {
		return nil, err
	}
	versions := []string{}
	for _, line := range strings.Split(string(body), "\n") {
		v := strings.TrimSpace(line)
		if module.CheckVersion(v) == nil {
			versions = append(versions, v)
		}
	}
	module.Sort(versions)
	return versions, nil
}

// Info returns metadata for one version.
func (c *Client) Info(ctx context.Context, modPath, version string) (module.Info, error) {
	esc, err := module.EscapePath(modPath)
	if err != nil {
		return module.Info{}, err
	}
	escV, err := module.EscapeVersion(version)
	if err != nil {
		return module.Info{}, err
	}
	return c.info(ctx, esc+"/@v/"+escV+".info", immutableTTL)
}

// Latest returns metadata for the version the proxy reports as latest.
func (c *Client) Latest(ctx context.Context, modPath string) (module.Info, error) {
	esc, err := module.EscapePath(modPath)
	if err != nil {
		return module.Info{}, err
	}
	return c.info(ctx, esc+"/@latest", mutableTTL)
}

// GoMod returns the go.mod file of one version.
func (c *Client) GoMod(ctx context.Context, modPath, version string) ([]byte, error) {
	esc, err := module.EscapePath(modPath)
	if err != nil {
		return nil, err
	}
	escV, err := module.EscapeVersion(version)
	if err != nil {
		return nil, err
	}
	return c.get(ctx, esc+"/@v/"+escV+".mod", immutableTTL, modSizeCap)
}

func (c *Client) info(ctx context.Context, rel string, ttl time.Duration) (module.Info, error) {
	body, err := c.get(ctx, rel, ttl, maxInfoSize)
	if err != nil {
		return module.Info{}, err
	}
	var info module.Info
	if err := json.Unmarshal(body, &info); err != nil {
		return module.Info{}, fmt.Errorf("decode %s: %w", rel, err)
	}
	if err := module.CheckVersion(info.Version); err != nil {
		return module.Info{}, fmt.Errorf("%s: proxy returned %w", rel, err)
	}
	return info, nil
}

func (c *Client) get(ctx context.Context, rel string, ttl time.Duration, limit int64) ([]byte, error) {
	if body, ok := c.cache.get(rel); ok {
		return body, nil
	}
	url := c.base + "/" + rel
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", url, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, fmt.Errorf("%s: %s: %w", rel, firstLine(body), module.ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, firstLine(body))
	case int64(len(body)) > limit:
		return nil, fmt.Errorf("GET %s: response is over the %d byte limit", url, limit)
	}
	c.cache.put(rel, body, ttl)
	return body, nil
}

func firstLine(b []byte) string {
	line, _, _ := bytes.Cut(bytes.TrimSpace(b), []byte("\n"))
	if len(line) > 200 {
		line = line[:200]
	}
	return string(line)
}

type ttlCache struct {
	mu       sync.Mutex
	items    map[string]cacheItem
	max      int // entries
	maxBytes int // total body bytes: public go.mod files can be up to 16 MB each
	bytes    int
}

// maxCachedBody is the largest response kept: anything bigger is refetched.
const maxCachedBody = 1 << 20

type cacheItem struct {
	body    []byte
	expires time.Time
}

func (c *ttlCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[key]
	if !ok || time.Now().After(it.expires) {
		return nil, false
	}
	return it.body, true
}

func (c *ttlCache) put(key string, body []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(body) > maxCachedBody {
		return
	}
	now := time.Now()
	if old, ok := c.items[key]; ok {
		c.bytes -= len(old.body)
		delete(c.items, key)
	}
	if len(c.items) >= c.max || (c.maxBytes > 0 && c.bytes+len(body) > c.maxBytes) {
		for k, it := range c.items {
			if now.After(it.expires) {
				c.bytes -= len(it.body)
				delete(c.items, k)
			}
		}
		if len(c.items) >= c.max || (c.maxBytes > 0 && c.bytes+len(body) > c.maxBytes) {
			clear(c.items)
			c.bytes = 0
		}
	}
	c.items[key] = cacheItem{body: body, expires: now.Add(ttl)}
	c.bytes += len(body)
}
