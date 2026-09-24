// Package mirror tells the public Go module mirror and checksum database
// about newly published versions.
//
// proxy.golang.org fetches a module the first time someone asks for it,
// following the registry's go-import tag, and sum.golang.org then records
// its hash in the transparency log. Asking for the version right after
// publishing means the first real user doesn't wait for that fetch, and
// the hash is logged while the registry is known to serve it.
package mirror

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codebled/gopherdex/internal/module"
)

// Notifier warms the mirror and checksum database in the background.
type Notifier struct {
	ProxyURL string // e.g. https://proxy.golang.org
	SumDBURL string // e.g. https://sum.golang.org
	HTTP     *http.Client
	Log      *slog.Logger
	Attempts int           // per request; default 3
	Backoff  time.Duration // before a retry; default 20s

	wg sync.WaitGroup
}

// Notify asks the mirror for modPath@version without blocking the caller.
func (n *Notifier) Notify(modPath, version string) {
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		n.notify(ctx, modPath, version)
	}()
}

// Wait blocks until pending notifications finish.
func (n *Notifier) Wait() { n.wg.Wait() }

func (n *Notifier) notify(ctx context.Context, modPath, version string) {
	escPath, err := module.EscapePath(modPath)
	if err != nil {
		n.log().Error("mirror notify: bad module path", "module", modPath, "err", err)
		return
	}
	escVersion, err := module.EscapeVersion(version)
	if err != nil {
		n.log().Error("mirror notify: bad version", "version", version, "err", err)
		return
	}
	info := strings.TrimSuffix(n.ProxyURL, "/") + "/" + escPath + "/@v/" + escVersion + ".info"
	if !n.get(ctx, "module mirror", info, modPath, version) {
		return
	}
	if n.SumDBURL != "" {
		lookup := strings.TrimSuffix(n.SumDBURL, "/") + "/lookup/" + escPath + "@" + escVersion
		n.get(ctx, "checksum database", lookup, modPath, version)
	}
}

// get requests url, retrying server errors and "not found yet" answers.
func (n *Notifier) get(ctx context.Context, what, url, modPath, version string) bool {
	attempts, backoff := n.Attempts, n.Backoff
	if attempts <= 0 {
		attempts = 3
	}
	if backoff <= 0 {
		backoff = 20 * time.Second
	}
	client := n.HTTP
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
	}
	var last string
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(backoff):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			last = err.Error()
			break
		}
		resp, err := client.Do(req)
		if err != nil {
			last = err.Error()
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			n.log().Info("mirror notified", "service", what, "module", modPath, "version", version)
			return true
		}
		last = fmt.Sprintf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusGone {
			break
		}
	}
	n.log().Warn("mirror notify failed; users can still install, the mirror will fetch on first request",
		"service", what, "module", modPath, "version", version, "last", last)
	return false
}

func (n *Notifier) log() *slog.Logger {
	if n.Log != nil {
		return n.Log
	}
	return slog.Default()
}
