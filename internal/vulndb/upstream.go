package vulndb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// DefaultUpstream is Go's public vulnerability database.
const DefaultUpstream = "https://vuln.go.dev"

// Upstream keeps a copy of a public database's indexes, refreshed in the
// background, and caches the entries it's asked for. Pages never wait on
// the network for the indexes; a failed refresh keeps the last good copy.
type Upstream struct {
	URL  string
	HTTP *http.Client
	Log  *slog.Logger

	mu       sync.Mutex
	meta     DBMeta
	modules  []ModulesEntry
	vulns    []VulnsEntry
	byModule map[string][]ModuleVuln
	entries  map[string]*Entry
}

func (u *Upstream) client() *http.Client {
	if u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Run refreshes the indexes now and then every interval until ctx ends.
func (u *Upstream) Run(ctx context.Context, interval time.Duration) {
	for {
		if err := u.Refresh(ctx); err != nil && ctx.Err() == nil && u.Log != nil {
			u.Log.Warn("refresh vulnerability database", "url", u.URL, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Refresh downloads the indexes if the database changed.
func (u *Upstream) Refresh(ctx context.Context) error {
	var meta DBMeta
	if err := u.get(ctx, "index/db.json", &meta); err != nil {
		return err
	}
	u.mu.Lock()
	same := !u.meta.Modified.IsZero() && meta.Modified.Equal(u.meta.Modified)
	u.mu.Unlock()
	if same {
		return nil
	}
	var modules []ModulesEntry
	if err := u.get(ctx, "index/modules.json", &modules); err != nil {
		return err
	}
	var vulns []VulnsEntry
	if err := u.get(ctx, "index/vulns.json", &vulns); err != nil {
		return err
	}
	byModule := make(map[string][]ModuleVuln, len(modules))
	for _, m := range modules {
		byModule[m.Path] = m.Vulns
	}
	u.mu.Lock()
	u.meta, u.modules, u.vulns, u.byModule = meta, modules, vulns, byModule
	u.mu.Unlock()
	if u.Log != nil {
		u.Log.Info("vulnerability database refreshed", "url", u.URL, "modules", len(modules), "vulns", len(vulns), "modified", meta.Modified)
	}
	return nil
}

// Indexes returns the current copy of the indexes; empty until the first
// successful refresh.
func (u *Upstream) Indexes() (DBMeta, []ModulesEntry, []VulnsEntry) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.meta, u.modules, u.vulns
}

// ModuleVulns lists the known vulnerabilities of a module path.
func (u *Upstream) ModuleVulns(modPath string) []ModuleVuln {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.byModule[modPath]
}

// Entry returns an upstream report, fetching it once per modification.
func (u *Upstream) Entry(ctx context.Context, id string, modified time.Time) (*Entry, error) {
	u.mu.Lock()
	e, ok := u.entries[id]
	u.mu.Unlock()
	if ok && !e.Modified.Before(modified) {
		return e, nil
	}
	e = new(Entry)
	if err := u.get(ctx, "ID/"+url.PathEscape(id)+".json", e); err != nil {
		return nil, err
	}
	u.mu.Lock()
	if u.entries == nil {
		u.entries = map[string]*Entry{}
	}
	u.entries[id] = e
	u.mu.Unlock()
	return e, nil
}

// EntryURL is where the upstream serves an entry, for redirects.
func (u *Upstream) EntryURL(id, suffix string) string {
	return strings.TrimSuffix(u.URL, "/") + "/ID/" + url.PathEscape(id) + suffix
}

func (u *Upstream) get(ctx context.Context, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(u.URL, "/")+"/"+path, nil)
	if err != nil {
		return err
	}
	resp, err := u.client().Do(req) // the transport asks for and decodes gzip
	if err != nil {
		return fmt.Errorf("vulndb: fetch %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vulndb: fetch %s: %s", path, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(dst); err != nil {
		return fmt.Errorf("vulndb: decode %s: %w", path, err)
	}
	return nil
}

// Merge combines the registry's own entries with an upstream copy of the
// indexes into the three index files. Module lists stay sorted by path and
// vulnerability lists by ID, as in the public database.
func Merge(own []*Entry, upMeta DBMeta, upModules []ModulesEntry, upVulns []VulnsEntry) (DBMeta, []ModulesEntry, []VulnsEntry) {
	meta := upMeta
	vulns := slices.Clone(upVulns)
	extra := map[string][]ModuleVuln{} // module path → our entries
	for _, e := range own {
		if e.Modified.After(meta.Modified) {
			meta.Modified = e.Modified
		}
		vulns = append(vulns, VulnsEntry{ID: e.ID, Modified: e.Modified, Aliases: e.Aliases})
		for _, a := range e.Affected {
			extra[a.Package.Name] = append(extra[a.Package.Name], ModuleVuln{ID: e.ID, Modified: e.Modified, Fixed: e.LatestFixed(a.Package.Name)})
		}
	}
	modules := make([]ModulesEntry, 0, len(upModules)+len(extra))
	for _, m := range upModules {
		modules = append(modules, ModulesEntry{Path: m.Path, Vulns: append(slices.Clone(m.Vulns), extra[m.Path]...)})
		delete(extra, m.Path)
	}
	for path, vs := range extra {
		modules = append(modules, ModulesEntry{Path: path, Vulns: vs})
	}
	slices.SortFunc(modules, func(a, b ModulesEntry) int { return strings.Compare(a.Path, b.Path) })
	for i := range modules {
		slices.SortFunc(modules[i].Vulns, func(a, b ModuleVuln) int { return strings.Compare(a.ID, b.ID) })
	}
	slices.SortFunc(vulns, func(a, b VulnsEntry) int { return strings.Compare(a.ID, b.ID) })
	return meta, modules, vulns
}
