package bench

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

// Write scenarios, run at fixed rates next to the read clients:
//
//	publish-version  a publisher uploads the next patch release of one of its modules
//	publish-new      a fresh account publishes a brand-new module
//	login            a fresh account signs in with its password
//
// They go through the same endpoints as the CLI and browsers, so uploads
// are checked, stored, indexed and counted exactly like real ones.
const (
	scenarioPublishVersion = "publish-version"
	scenarioPublishNew     = "publish-new"
	scenarioLogin          = "login"
)

type writers struct {
	l          *loader
	client     *http.Client
	publishers []Account
	users      []Account
	owned      map[string][]*target // publisher → modules in their namespace
	host       string

	mu      sync.Mutex
	next    map[string]string // module → last version we published
	pubTurn int
	newSeq  int
}

func newWriters(l *loader, client *http.Client, path string) (*writers, error) {
	accts, err := readAccounts(path)
	if err != nil {
		return nil, fmt.Errorf("%w (run gdxbench accounts first)", err)
	}
	w := &writers{l: l, client: client, owned: map[string][]*target{}, next: map[string]string{}}
	w.host, _, _ = strings.Cut(l.targets[0].path, "/")
	for i := range l.targets {
		t := &l.targets[i]
		w.owned[t.ns] = append(w.owned[t.ns], t)
	}
	for _, a := range accts {
		switch {
		case a.Kind == "publisher" && len(w.owned[a.Username]) > 0:
			w.publishers = append(w.publishers, a)
		case a.Kind == "user":
			w.users = append(w.users, a)
		}
	}
	return w, nil
}

// run sends one scenario at rate per second until ctx ends, without
// waiting for responses: a slow server gets a queue, as it would in life.
func (w *writers) run(ctx context.Context, name string, rate float64, measureFrom time.Time, record func(string, sample)) {
	if rate <= 0 {
		return
	}
	t := time.NewTicker(time.Duration(float64(time.Second) / rate))
	defer t.Stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		seed := r.Int63()
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := w.once(ctx, name, rand.New(rand.NewSource(seed)))
			if ctx.Err() != nil && s.failed {
				return
			}
			if time.Now().After(measureFrom) {
				record(name, s)
			}
		}()
	}
}

func (w *writers) once(ctx context.Context, name string, r *rand.Rand) sample {
	switch name {
	case scenarioPublishVersion:
		if len(w.publishers) == 0 {
			return sample{status: "no-publishers", failed: true}
		}
		w.mu.Lock()
		a := w.publishers[w.pubTurn%len(w.publishers)] // round-robin: each account has an hourly upload limit
		w.pubTurn++
		mods := w.owned[a.Username]
		t := mods[r.Intn(len(mods))]
		last, known := w.next[t.path]
		w.mu.Unlock()
		if !known {
			// Earlier runs may have published past modules.tsv's latest.
			last = cmpOr(w.latestPublished(ctx, t), t.latest)
		}
		w.mu.Lock()
		if cur, ok := w.next[t.path]; ok && semver.Compare(cur, last) > 0 {
			last = cur
		}
		v := nextPatch(last)
		w.next[t.path] = v
		w.mu.Unlock()
		return w.upload(ctx, a, t.path, v)
	case scenarioPublishNew:
		if len(w.users) == 0 {
			return sample{status: "no-users", failed: true}
		}
		w.mu.Lock()
		w.newSeq++
		a := w.users[w.newSeq%len(w.users)]
		n := w.newSeq
		w.mu.Unlock()
		mod := fmt.Sprintf("%s/%s/%s-%s-%d", w.host, a.Username, nameWords[r.Intn(len(nameWords))], nameWords[r.Intn(len(nameWords))], n)
		return w.upload(ctx, a, mod, "v0.1.0")
	case scenarioLogin:
		if len(w.users) == 0 {
			return sample{status: "no-users", failed: true}
		}
		a := w.users[r.Intn(len(w.users))]
		form := url.Values{"login": {a.Username}, "password": {a.Password}}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, w.l.cfg.BaseURL+"/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return w.send(req, http.StatusSeeOther)
	}
	return sample{status: "unknown", failed: true}
}

// latestPublished asks the registry's proxy for a module's highest version.
func (w *writers) latestPublished(ctx context.Context, t *target) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.l.cfg.BaseURL+proxyURL(*t, "/@v/list"), nil)
	if err != nil {
		return ""
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	best := ""
	for _, v := range strings.Fields(string(body)) {
		if semver.IsValid(v) && (best == "" || semver.Compare(v, best) > 0) {
			best = v
		}
	}
	return best
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// nextPatch returns the release after v: v1.2.3 → v1.2.4, and
// v1.3.0-rc.1 → v1.3.0.
func nextPatch(v string) string {
	if semver.Prerelease(v) != "" {
		return strings.TrimSuffix(v, semver.Prerelease(v))
	}
	parts := strings.SplitN(strings.TrimPrefix(semver.Canonical(v), "v"), ".", 3)
	if len(parts) != 3 {
		return "v0.1.0"
	}
	p, _ := strconv.Atoi(parts[2])
	return fmt.Sprintf("v%s.%s.%d", parts[0], parts[1], p+1)
}

func (w *writers) upload(ctx context.Context, a Account, modPath, version string) sample {
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	mw.WriteField("module", modPath)
	mw.WriteField("version", version)
	zw, _ := mw.CreateFormFile("zip", "module.zip")
	if err := writeSmallZip(zw, modPath, version); err != nil {
		return sample{status: "zip-error", failed: true}
	}
	mw.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, w.l.cfg.BaseURL+"/api/upload", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+a.Token)
	return w.send(req, http.StatusOK, http.StatusCreated)
}

func (w *writers) send(req *http.Request, ok ...int) sample {
	req.Header.Set("User-Agent", userAgent)
	// Like the readers, each request comes from one of many clients, so
	// per-IP limits apply as they would to real users.
	n := rand.Intn(max(w.l.cfg.ClientIPs, 1))
	req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.%d.%d.%d", n>>16&255, n>>8&255, n&255))
	t0 := time.Now()
	resp, err := w.client.Do(req)
	if err != nil {
		return sample{d: time.Since(t0), status: "error", failed: true}
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	d := time.Since(t0)
	for _, code := range ok {
		if resp.StatusCode == code {
			return sample{d: d, status: strconv.Itoa(resp.StatusCode)}
		}
	}
	return sample{d: d, status: strconv.Itoa(resp.StatusCode), failed: true}
}

func writeSmallZip(out io.Writer, modPath, version string) error {
	zw := zip.NewWriter(out)
	prefix := modPath + "@" + version + "/"
	name := modPath[strings.LastIndex(modPath, "/")+1:]
	files := map[string]string{
		"go.mod":    "module " + modPath + "\n\ngo 1.22\n",
		"lib.go":    fmt.Sprintf("// Package %s is published by the load test.\npackage %s\n\n// Version is this release.\nconst Version = %q\n", pkgName(name), pkgName(name), version),
		"README.md": "# " + name + "\n\nPublished by gdxbench load at " + time.Now().UTC().Format(time.RFC3339) + ".\n",
	}
	for f, body := range files {
		fw, err := zw.Create(prefix + f)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(fw, body); err != nil {
			return err
		}
	}
	return zw.Close()
}
