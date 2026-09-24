// Command gdxbench builds large datasets for Gopherdex and load-tests a
// running server.
//
//	gdxbench seed [-profile small|full] [flags]   build a dataset in bench/
//	gdxbench accounts [flags]                     credentials for write load (tokens, sign-ins)
//	gdxbench load [flags]                         drive traffic at a server
//
// Seed samples real modules from index.golang.org and proxy.golang.org
// (cached under bench/cache, so reruns don't download again), remaps them
// under the registry's module host, pads the set with synthetic modules,
// and publishes everything through the registry's own publish code.
//
// Then start a server on the dataset, offline so it makes no outbound
// requests, and trusting X-Forwarded-For so the load comes from many
// clients instead of tripping one client's rate limits:
//
//	gopherdexd -db bench/data/gopherdex.db -blobs bench/data/blobs -offline -trust-proxy
//	gdxbench load -url http://localhost:8080 -c 64 -d 60s
//
// Mixed read and write load, e.g. for 30 minutes with two uploads and five
// sign-ins a second:
//
//	gdxbench accounts
//	gdxbench load -d 30m -publish-rate 2 -login-rate 5
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/codebled/gopherdex/internal/bench"
)

type profile struct {
	real, zips, modules, versions, users, orgs, downloads int
}

var profiles = map[string]profile{
	"small": {real: 2000, zips: 150, modules: 5000, versions: 25000, users: 150, orgs: 15, downloads: 50000},
	"full":  {real: 50000, zips: 3000, modules: 100000, versions: 1000000, users: 800, orgs: 80, downloads: 2000000},
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	switch os.Args[1] {
	case "seed":
		err = seed(ctx, os.Args[2:])
	case "load":
		err = load(ctx, os.Args[2:])
	case "accounts":
		err = prepareAccounts(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gdxbench:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gdxbench seed|accounts|load [flags]   (-h for flags)")
	os.Exit(2)
}

func seed(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	prof := fs.String("profile", "small", "dataset size: small (a few minutes) or full (100k modules, 1M versions; hours)")
	dir := fs.String("dir", "bench", "where the cache, dataset and reports go")
	host := fs.String("module-host", "gopherdex.localhost", "module host of the server that will serve the dataset")
	index := fs.String("index", "https://index.golang.org", "Go module index")
	proxy := fs.String("proxy", "https://proxy.golang.org", "Go module mirror")
	since := fs.String("since", "2020-01-01", "oldest date to sample the index from")
	windows := fs.Int("windows", 24, "index windows spread between -since and now")
	conc := fs.Int("concurrency", 8, "parallel upstream fetches and zip builds (keep it modest: the mirror is shared)")
	maxZip := fs.Int64("max-zip", 8<<20, "skip real zips larger than this many bytes")
	skipChecks := fs.Bool("skip-checks", false, "publish without the publish-time safety checks")
	rnd := fs.Uint64("seed", 1, "random seed; the same seed and cache give the same dataset")
	realMods := fs.Int("real", -1, "override: real modules to sample")
	zips := fs.Int("zips", -1, "override: real modules published with their real source")
	modules := fs.Int("modules", -1, "override: total modules")
	versions := fs.Int("versions", -1, "override: total versions (roughly)")
	users := fs.Int("users", -1, "override: publisher accounts")
	orgs := fs.Int("orgs", -1, "override: organizations")
	downloads := fs.Int("downloads", -1, "override: downloads per day, spread by popularity")
	if err := fs.Parse(args); err != nil {
		return err
	}

	p, ok := profiles[*prof]
	if !ok {
		return fmt.Errorf("unknown profile %q (small or full)", *prof)
	}
	pick := func(v, def int) int {
		if v >= 0 {
			return v
		}
		return def
	}
	from, err := time.Parse(time.DateOnly, *since)
	if err != nil {
		return fmt.Errorf("-since: %w", err)
	}
	data := filepath.Join(*dir, "data")
	cfg := bench.SeedConfig{
		DB: filepath.Join(data, "gopherdex.db"), Blobs: filepath.Join(data, "blobs"), Out: data, ModuleHost: *host,
		Upstream: &bench.Upstream{Index: strings.TrimSuffix(*index, "/"), Proxy: strings.TrimSuffix(*proxy, "/"),
			Cache: filepath.Join(*dir, "cache"), HTTP: &http.Client{Timeout: 2 * time.Minute}, MaxZip: *maxZip},
		Real: pick(*realMods, p.real), Zips: pick(*zips, p.zips), Modules: pick(*modules, p.modules), Versions: pick(*versions, p.versions),
		Users: pick(*users, p.users), Orgs: pick(*orgs, p.orgs), DownloadsPerDay: pick(*downloads, p.downloads),
		Since: from, Windows: *windows, Concurrency: *conc, SkipChecks: *skipChecks, Seed: *rnd,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Progress: os.Stderr,
	}
	start := time.Now()
	rep, err := bench.Seed(ctx, cfg)
	if rep != nil {
		writeSeedReport(os.Stdout, rep, time.Since(start))
		if err := saveJSON(filepath.Join(data, "seed-report.json"), rep); err != nil {
			fmt.Fprintln(os.Stderr, "gdxbench: save seed report:", err)
		}
	}
	if err == nil {
		fmt.Printf("\nDataset ready. Serve it with:\n  gopherdexd -db %s -blobs %s -module-host %s -offline -trust-proxy\n",
			cfg.DB, cfg.Blobs, *host)
	}
	return err
}

func writeSeedReport(w io.Writer, r *bench.SeedReport, total time.Duration) {
	fmt.Fprintf(w, "\nDataset: %d modules (%d real, %d with real source), %d versions, %d users, %d organizations\n",
		r.Modules, r.RealModules, r.RealZips, r.Versions, r.Users, r.Orgs)
	fmt.Fprintf(w, "Size: database %s, blobs %s\n\n", human(r.DBBytes), human(r.BlobBytes))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, p := range r.Phases {
		fmt.Fprintf(tw, "  %s\t%s\n", p.Name, p.Duration)
	}
	fmt.Fprintf(tw, "  total\t%s\n", total.Round(time.Second))
	tw.Flush()
	lat := func(name string, l bench.Latency) {
		fmt.Fprintf(w, "%s: %d, p50 %s, p90 %s, p99 %s, max %s\n", name, l.Count, l.P50.Round(time.Microsecond),
			l.P90.Round(time.Microsecond), l.P99.Round(time.Microsecond), l.Max.Round(time.Microsecond))
	}
	fmt.Fprintln(w)
	lat("Publish (every version)", r.Publish)
	lat("First publish of a module (includes name checks)", r.FirstPublish)
	fmt.Fprintf(w, "Published with warnings (sent to the review queue): %d\n", r.Warned)
	if len(r.Rejected) > 0 {
		fmt.Fprintf(w, "Rejected: %v\n", r.Rejected)
	}
}

func load(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	base := fs.String("url", "http://localhost:8080", "server to load")
	modules := fs.String("modules", "bench/data/modules.tsv", "modules.tsv written by seed")
	dur := fs.Duration("d", 60*time.Second, "how long to measure")
	warm := fs.Duration("warmup", 5*time.Second, "requests sent first and not measured")
	conc := fs.Int("c", 32, "concurrent clients")
	rps := fs.Int("rps", 0, "total requests per second; 0 is as fast as the clients go")
	ips := fs.Int("ips", 5000, "distinct client addresses sent in X-Forwarded-For; 0 sends one per client")
	mix := fs.String("mix", "", "scenario weights like search=5,proxy-zip=2 (default: a registry-like mix); only=NAME runs one")
	timeout := fs.Duration("timeout", 30*time.Second, "per-request timeout")
	out := fs.String("out", "", "also write the report as JSON to this file")
	remote := fs.Bool("i-own-this-server", false, "allow a non-local -url (only load servers you run)")
	accts := fs.String("accounts", "bench/data/accounts.tsv", "credentials written by gdxbench accounts, for write load")
	publishRate := fs.Float64("publish-rate", 0, "uploads per second through /api/upload, alongside the reads")
	newShare := fs.Float64("new-share", 0.3, "fraction of uploads that are brand-new modules; the rest are new versions")
	loginRate := fs.Float64("login-rate", 0, "password sign-ins per second")
	window := fs.Duration("window", 30*time.Second, "timeline resolution in the report")
	if err := fs.Parse(args); err != nil {
		return err
	}

	u, err := url.Parse(*base)
	if err != nil || u.Host == "" {
		return fmt.Errorf("-url %q isn't a URL", *base)
	}
	if h := u.Hostname(); h != "localhost" && h != "127.0.0.1" && h != "::1" && !*remote {
		return fmt.Errorf("%s isn't local; pass -i-own-this-server to load a server you run", h)
	}
	m, err := parseMix(*mix)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "== loading %s: %d clients for %s (after %s warm-up)\n", *base, *conc, *dur, *warm)
	rep, err := bench.Load(ctx, bench.LoadConfig{BaseURL: strings.TrimSuffix(*base, "/"), Modules: *modules, Duration: *dur, Warmup: *warm,
		Concurrency: *conc, RPS: *rps, ClientIPs: *ips, Mix: m, Timeout: *timeout, Seed: time.Now().UnixNano(), Progress: os.Stderr,
		Accounts: *accts, PublishRate: *publishRate, NewModuleShare: *newShare, LoginRate: *loginRate, Window: *window})
	if err != nil {
		return err
	}
	fmt.Println()
	rep.WriteTable(os.Stdout)
	if len(rep.Timeline) > 1 {
		fmt.Println()
		rep.WriteTimeline(os.Stdout, *window)
	}
	if *out != "" {
		return saveJSON(*out, rep)
	}
	return nil
}

func prepareAccounts(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("accounts", flag.ExitOnError)
	dir := fs.String("dir", "bench", "the benchmark directory seed wrote")
	publishers := fs.Int("publishers", 200, "existing publishers to give API tokens (each may upload 60 releases an hour)")
	users := fs.Int("users", 50, "fresh accounts with known passwords, for sign-ins and brand-new modules")
	if err := fs.Parse(args); err != nil {
		return err
	}
	out := filepath.Join(*dir, "data", "accounts.tsv")
	n, err := bench.PrepareAccounts(ctx, bench.AccountsConfig{DB: filepath.Join(*dir, "data", "gopherdex.db"), Out: out,
		Publishers: *publishers, NewUsers: *users})
	if err != nil {
		return err
	}
	fmt.Printf("%d accounts written to %s (test credentials for this local database; keep the file private)\n", n, out)
	return nil
}

func parseMix(s string) (map[string]int, error) {
	if s == "" {
		return bench.DefaultMix, nil
	}
	if name, ok := strings.CutPrefix(s, "only="); ok {
		return map[string]int{name: 1}, nil
	}
	m := map[string]int{}
	for _, part := range strings.Split(s, ",") {
		name, w, ok := strings.Cut(strings.TrimSpace(part), "=")
		n, err := strconv.Atoi(w)
		if !ok || err != nil || n < 0 {
			return nil, fmt.Errorf("-mix: %q should be name=weight", part)
		}
		m[name] = n
	}
	return m, nil
}

func saveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
}
