package bench

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	xmodule "golang.org/x/mod/module"
)

// LoadConfig describes a load run against a running server.
type LoadConfig struct {
	BaseURL     string
	Modules     string // modules.tsv from Seed
	Duration    time.Duration
	Warmup      time.Duration // requests before measuring starts, not counted
	Concurrency int           // simultaneous clients
	RPS         int           // total requests per second; 0 sends as fast as the clients can
	ClientIPs   int           // distinct X-Forwarded-For addresses (the server needs -trust-proxy)
	Mix         map[string]int
	Timeout     time.Duration
	Seed        int64
	Progress    io.Writer
}

// DefaultMix weighs scenarios roughly like a package registry's traffic:
// mostly the go command's proxy requests, project pages and search.
var DefaultMix = map[string]int{
	"home": 3, "search": 14, "search-sorted": 3, "project": 14, "project-version": 3, "docs": 6, "versions": 3,
	"deps": 2, "source": 1, "owner": 3, "api-module": 6, "api-search": 3, "badge": 4, "feed": 1, "go-get": 4,
	"proxy-list": 8, "proxy-latest": 4, "proxy-info": 5, "proxy-mod": 6, "proxy-zip": 5,
}

type target struct {
	path, rel, latest, ns string
}

type loader struct {
	cfg     LoadConfig
	targets []target
	queries []string
	names   []string
	weights []int
	total   int
}

// Latency is a latency distribution.
type Latency struct {
	Count                   int
	P50, P90, P99, Max, Avg time.Duration
}

func summarize(ds []time.Duration) Latency {
	if len(ds) == 0 {
		return Latency{}
	}
	slices.Sort(ds)
	q := func(p float64) time.Duration { return ds[min(len(ds)-1, int(p*float64(len(ds))))] }
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return Latency{len(ds), q(0.50), q(0.90), q(0.99), ds[len(ds)-1], sum / time.Duration(len(ds))}
}

// ScenarioResult is one scenario's measurements.
type ScenarioResult struct {
	Name     string
	Latency  Latency
	RPS      float64
	Errors   int            // transport errors and 4xx/5xx responses
	Statuses map[string]int // response codes and error kinds
	Bytes    int64
}

// LoadReport is a load run's results.
type LoadReport struct {
	Duration    time.Duration
	Concurrency int
	Requests    int
	Errors      int
	RPS         float64
	Overall     Latency
	Scenarios   []ScenarioResult
}

type sample struct {
	d      time.Duration
	status string
	bytes  int64
	failed bool
}

// Load runs cfg against its server and returns the measurements.
func Load(ctx context.Context, cfg LoadConfig) (*LoadReport, error) {
	l := &loader{cfg: cfg}
	if err := l.readTargets(); err != nil {
		return nil, err
	}
	for name := range cfg.Mix {
		if _, ok := scenarios[name]; !ok {
			return nil, fmt.Errorf("unknown scenario %q", name)
		}
	}
	for name, w := range cfg.Mix {
		if w > 0 {
			l.names = append(l.names, name)
		}
	}
	sort.Strings(l.names)
	for _, n := range l.names {
		l.total += cfg.Mix[n]
		l.weights = append(l.weights, l.total)
	}
	if l.total == 0 {
		return nil, errors.New("the scenario mix is empty")
	}

	client := &http.Client{
		Timeout: cfg.Timeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConns:        cfg.Concurrency * 2,
			MaxIdleConnsPerHost: cfg.Concurrency * 2,
			IdleConnTimeout:     30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	start := time.Now()
	measureFrom := start.Add(cfg.Warmup)
	deadline := measureFrom.Add(cfg.Duration)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var tokens chan struct{}
	if cfg.RPS > 0 {
		tokens = make(chan struct{}, cfg.RPS)
		go func() {
			const tick = 10 * time.Millisecond
			t := time.NewTicker(tick)
			defer t.Stop()
			carry := 0.0
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					carry += float64(cfg.RPS) * tick.Seconds()
					for ; carry >= 1; carry-- {
						select {
						case tokens <- struct{}{}:
						default:
						}
					}
				}
			}
		}()
	}

	results := make([]map[string][]sample, cfg.Concurrency)
	var wg sync.WaitGroup
	for w := range cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rand.New(rand.NewSource(cfg.Seed + int64(w)))
			zipf := rand.NewZipf(r, 1.2, 1, uint64(len(l.targets)-1))
			ip := fmt.Sprintf("10.%d.%d.%d", r.Intn(256), r.Intn(256), 1+r.Intn(250))
			mine := map[string][]sample{}
			defer func() { results[w] = mine }()
			for ctx.Err() == nil {
				if tokens != nil {
					select {
					case <-tokens:
					case <-ctx.Done():
						return
					}
				}
				if cfg.ClientIPs > 0 && r.Intn(20) == 0 { // clients come and go
					n := r.Intn(cfg.ClientIPs)
					ip = fmt.Sprintf("10.%d.%d.%d", n>>16&255, n>>8&255, n&255)
				}
				name := l.pick(r)
				t := l.targets[zipf.Uint64()]
				s := l.do(ctx, client, scenarios[name](l, r, t), ip)
				if ctx.Err() != nil && s.failed {
					return // cut off by the deadline
				}
				if time.Now().After(measureFrom) {
					mine[name] = append(mine[name], s)
				}
			}
		}()
	}
	if cfg.Progress != nil {
		go func() {
			t := time.NewTicker(5 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-t.C:
					phase := "measuring"
					if now.Before(measureFrom) {
						phase = "warming up"
					}
					fmt.Fprintf(cfg.Progress, "   %s… %s left\n", phase, deadline.Sub(now).Round(time.Second))
				}
			}
		}()
	}
	wg.Wait()

	rep := &LoadReport{Duration: cfg.Duration, Concurrency: cfg.Concurrency}
	var all []time.Duration
	byName := map[string][]sample{}
	for _, m := range results {
		for n, ss := range m {
			byName[n] = append(byName[n], ss...)
		}
	}
	for _, n := range l.names {
		ss := byName[n]
		res := ScenarioResult{Name: n, Statuses: map[string]int{}}
		ds := make([]time.Duration, 0, len(ss))
		for _, s := range ss {
			ds = append(ds, s.d)
			res.Statuses[s.status]++
			res.Bytes += s.bytes
			if s.failed {
				res.Errors++
			}
		}
		all = append(all, ds...)
		res.Latency = summarize(ds)
		res.RPS = float64(len(ss)) / cfg.Duration.Seconds()
		rep.Requests += len(ss)
		rep.Errors += res.Errors
		rep.Scenarios = append(rep.Scenarios, res)
	}
	rep.Overall = summarize(all)
	rep.RPS = float64(rep.Requests) / cfg.Duration.Seconds()
	return rep, nil
}

func (l *loader) pick(r *rand.Rand) string {
	n := r.Intn(l.total)
	i := sort.SearchInts(l.weights, n+1)
	return l.names[i]
}

func (l *loader) do(ctx context.Context, c *http.Client, path, ip string) sample {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.cfg.BaseURL+path, nil)
	if err != nil {
		return sample{status: "bad-request", failed: true}
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Forwarded-For", ip)
	t0 := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		kind := "error"
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			kind = "timeout"
		}
		return sample{d: time.Since(t0), status: kind, failed: true}
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	d := time.Since(t0)
	if err != nil {
		return sample{d: d, status: "read-error", failed: true}
	}
	return sample{d: d, status: strconv.Itoa(resp.StatusCode), bytes: n, failed: resp.StatusCode >= 400}
}

// readTargets loads modules.tsv and derives search queries from the names.
func (l *loader) readTargets() error {
	f, err := os.Open(l.cfg.Modules)
	if err != nil {
		return err
	}
	defer f.Close()
	words := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < 4 {
			continue
		}
		_, rel, _ := strings.Cut(fields[0], "/")
		l.targets = append(l.targets, target{path: fields[0], rel: rel, latest: fields[1], ns: fields[3]})
		prefix, _, _ := xmodule.SplitPathVersion(fields[0])
		for _, w := range strings.Split(prefix[strings.LastIndex(prefix, "/")+1:], "-") {
			if len(w) >= 3 {
				words[w] = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if len(l.targets) < 2 {
		return fmt.Errorf("%s lists %d modules; run seed first", l.cfg.Modules, len(l.targets))
	}
	for w := range words {
		l.queries = append(l.queries, w)
	}
	sort.Strings(l.queries)
	// Shuffle once so hot modules are spread over the file, not just the
	// real ones at its top.
	r := rand.New(rand.NewSource(l.cfg.Seed))
	r.Shuffle(len(l.targets), func(i, j int) { l.targets[i], l.targets[j] = l.targets[j], l.targets[i] })
	return nil
}

func (l *loader) query(r *rand.Rand, t target) string {
	switch k := r.Intn(10); {
	case k < 5:
		return l.queries[r.Intn(len(l.queries))]
	case k < 8:
		return l.queries[r.Intn(len(l.queries))] + " " + l.queries[r.Intn(len(l.queries))]
	case k < 9:
		return t.ns + "/" + t.rel[strings.LastIndex(t.rel, "/")+1:]
	default:
		return "zq" + strconv.Itoa(r.Intn(1e6)) // no results
	}
}

func proxyURL(t target, suffix string) string {
	p, err := xmodule.EscapePath(t.path)
	if err != nil {
		p = t.path
	}
	return "/api/proxy/" + p + suffix
}

func escVersion(v string) string {
	if e, err := xmodule.EscapeVersion(v); err == nil {
		return e
	}
	return v
}

var scenarios = map[string]func(l *loader, r *rand.Rand, t target) string{
	"home": func(*loader, *rand.Rand, target) string { return "/" },
	"search": func(l *loader, r *rand.Rand, t target) string {
		return "/search?scope=hosted&q=" + url.QueryEscape(l.query(r, t))
	},
	"search-sorted": func(l *loader, r *rand.Rand, t target) string {
		return "/search?scope=hosted&sort=" + []string{"downloads", "updated", "new"}[r.Intn(3)] + "&q=" + url.QueryEscape(l.query(r, t))
	},
	"project":         func(_ *loader, _ *rand.Rand, t target) string { return "/" + t.rel },
	"project-version": func(_ *loader, _ *rand.Rand, t target) string { return "/" + t.rel + "@" + t.latest },
	"docs":            func(_ *loader, _ *rand.Rand, t target) string { return "/" + t.rel + "?tab=docs" },
	"versions":        func(_ *loader, _ *rand.Rand, t target) string { return "/" + t.rel + "?tab=versions" },
	"deps":            func(_ *loader, _ *rand.Rand, t target) string { return "/" + t.rel + "?tab=deps" },
	"source":          func(_ *loader, _ *rand.Rand, t target) string { return "/" + t.rel + "?tab=source" },
	"owner":           func(_ *loader, _ *rand.Rand, t target) string { return "/" + t.ns },
	"api-module":      func(_ *loader, _ *rand.Rand, t target) string { return "/api/v1/modules/" + t.path },
	"api-search": func(l *loader, r *rand.Rand, t target) string {
		return "/api/v1/search?q=" + url.QueryEscape(l.query(r, t))
	},
	"badge":        func(_ *loader, _ *rand.Rand, t target) string { return "/badge/" + t.rel + ".svg" },
	"feed":         func(_ *loader, _ *rand.Rand, t target) string { return "/feeds/" + t.ns + ".atom" },
	"go-get":       func(_ *loader, _ *rand.Rand, t target) string { return "/" + t.rel + "?go-get=1" },
	"proxy-list":   func(_ *loader, _ *rand.Rand, t target) string { return proxyURL(t, "/@v/list") },
	"proxy-latest": func(_ *loader, _ *rand.Rand, t target) string { return proxyURL(t, "/@latest") },
	"proxy-info": func(_ *loader, _ *rand.Rand, t target) string {
		return proxyURL(t, "/@v/"+escVersion(t.latest)+".info")
	},
	"proxy-mod": func(_ *loader, _ *rand.Rand, t target) string { return proxyURL(t, "/@v/"+escVersion(t.latest)+".mod") },
	"proxy-zip": func(_ *loader, _ *rand.Rand, t target) string { return proxyURL(t, "/@v/"+escVersion(t.latest)+".zip") },
}

// WriteTable prints a report as an aligned table.
func (rep *LoadReport) WriteTable(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "scenario\trequests\treq/s\terrors\tp50\tp90\tp99\tmax\tavg size\t")
	row := func(name string, l Latency, rps float64, errs int, size int64, codes map[string]int) {
		avg := int64(0)
		if l.Count > 0 {
			avg = size / int64(l.Count)
		}
		fmt.Fprintf(tw, "%s\t%d\t%.1f\t%s\t%s\t%s\t%s\t%s\t%s\t\n", name, l.Count, rps, errText(errs, codes),
			ms(l.P50), ms(l.P90), ms(l.P99), ms(l.Max), kb(avg))
	}
	var total int64
	for _, s := range rep.Scenarios {
		row(s.Name, s.Latency, s.RPS, s.Errors, s.Bytes, s.Statuses)
		total += s.Bytes
	}
	row("ALL", rep.Overall, rep.RPS, rep.Errors, total, nil)
	tw.Flush()
}

func errText(n int, codes map[string]int) string {
	if n == 0 {
		return "0"
	}
	var parts []string
	for code, c := range codes {
		if code >= "400" || code[0] > '9' {
			parts = append(parts, fmt.Sprintf("%s×%d", code, c))
		}
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%d (%s)", n, strings.Join(parts, " "))
}

func ms(d time.Duration) string {
	switch {
	case d == 0:
		return "-"
	case d < 10*time.Millisecond:
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	case d < time.Second:
		return fmt.Sprintf("%.0fms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

func kb(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	return fmt.Sprintf("%.1fKB", float64(n)/1024)
}
