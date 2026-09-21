// Command gopherdexd runs the Gopherdex Go module registry server: accounts and API
// tokens, a GOPROXY server for hosted modules, and a site to search libraries,
// browse their release history and read their docs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	xmodule "golang.org/x/mod/module"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/blob"
	"github.com/parthiban-sivakumar/gopherdex/internal/database"
	"github.com/parthiban-sivakumar/gopherdex/internal/discovery"
	"github.com/parthiban-sivakumar/gopherdex/internal/goproxy"
	"github.com/parthiban-sivakumar/gopherdex/internal/mail"
	"github.com/parthiban-sivakumar/gopherdex/internal/mirror"
	"github.com/parthiban-sivakumar/gopherdex/internal/oidc"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
	"github.com/parthiban-sivakumar/gopherdex/internal/server"
	"github.com/parthiban-sivakumar/gopherdex/internal/store"
	"github.com/parthiban-sivakumar/gopherdex/internal/version"
	"github.com/parthiban-sivakumar/gopherdex/internal/vulndb"
)

func main() {
	run := run
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "backup":
			run = backupCommand
		case "healthcheck":
			run = healthcheckCommand
		case "blobs":
			run = blobsCommand
		case "version", "-version", "--version":
			fmt.Println("gopherdexd", version.String())
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gopherdexd:", err)
		os.Exit(1)
	}
}

// healthcheckCommand is "gopherdexd healthcheck [-url URL]": it exits 0
// when the server answers /healthz, for container health checks in images
// without curl.
func healthcheckCommand() error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	target := fs.String("url", "http://localhost:8080/healthz", "health endpoint to check")
	fs.Parse(os.Args[2:])
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(*target)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", *target, resp.Status)
	}
	return nil
}

// blobsCommand is "gopherdexd blobs copy -from A -to B": it copies every
// published zip between stores, e.g. from the local directory to S3 before
// switching -blobs. Blobs already at the destination are skipped, so it's
// safe to run again.
func blobsCommand() error {
	if len(os.Args) < 3 || os.Args[2] != "copy" {
		return errors.New("usage: gopherdexd blobs copy -from DIR|s3://… -to DIR|s3://…")
	}
	fs := flag.NewFlagSet("blobs copy", flag.ExitOnError)
	from := fs.String("from", "data/blobs", "store to copy from")
	to := fs.String("to", "", "store to copy to")
	maxSize := fs.Int64("max-upload", registry.DefaultMaxZipSize*2, "largest blob to copy, in bytes")
	fs.Parse(os.Args[3:])
	if *to == "" || *to == *from {
		return errors.New("give a different -to store")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	src, closeSrc, err := blob.Open(*from, os.Getenv)
	if err != nil {
		return err
	}
	defer closeSrc()
	dst, closeDst, err := blob.Open(*to, os.Getenv)
	if err != nil {
		return err
	}
	defer closeDst()
	copied, skipped, err := blob.Copy(ctx, dst, src, *maxSize)
	fmt.Printf("Copied %d blobs, skipped %d already at %s.\n", copied, skipped, *to)
	return err
}

// backupCommand is "gopherdexd backup -db FILE -out FILE": a consistent
// copy of the database, safe to take while the server runs.
func backupCommand() error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	dbPath := fs.String("db", "data/gopherdex.db", "database to back up")
	out := fs.String("out", "", "file to write (must not exist); default gopherdex-<time>.db in the current directory")
	fs.Parse(os.Args[2:])
	if *out == "" {
		*out = "gopherdex-" + time.Now().UTC().Format("20060102T150405Z") + ".db"
	}
	ctx := context.Background()
	db, err := database.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := database.Backup(ctx, db, *out); err != nil {
		return err
	}
	fmt.Println("Backed up", *dbPath, "to", *out)
	fmt.Println("Also copy the blob directory (published zips); its files never change once written.")
	return nil
}

func run() error {
	addr := flag.String("addr", "localhost:8080", "address to listen on")
	dataDir := flag.String("data", "", "directory of fixture modules to serve alongside published ones (for demos and tests)")
	blobDir := flag.String("blobs", "data/blobs", "where published module zips live: a directory, or an S3-compatible bucket like s3://bucket/prefix?region=…&endpoint=… (credentials from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY)")
	moduleHost := flag.String("module-host", "", "domain modules are published under (default: the -base-url host, or gopherdex.localhost for localhost)")
	maxUpload := flag.Int64("max-upload", registry.DefaultMaxZipSize, "largest module zip accepted, in bytes")
	upstream := flag.String("upstream", "https://proxy.golang.org", "public GOPROXY used for modules not hosted here")
	pkgsite := flag.String("pkgsite", "https://pkg.go.dev", "pkg.go.dev base URL for public search and docs links")
	offline := flag.Bool("offline", false, "serve hosted modules only and make no outbound requests")
	dbPath := flag.String("db", "data/gopherdex.db", "SQLite database file")
	baseURL := flag.String("base-url", "", "public site URL used in emails and cookies (default http://<addr>)")
	smtpAddr := flag.String("smtp-addr", "", "SMTP server host:port; empty logs emails instead of sending them")
	smtpFrom := flag.String("smtp-from", "Gopherdex <no-reply@localhost>", "sender address for emails")
	smtpUser := flag.String("smtp-user", "", "SMTP username (password comes from $GOPHERDEX_SMTP_PASSWORD)")
	trustProxy := flag.Bool("trust-proxy", false, "take client IPs from X-Forwarded-For (only behind a reverse proxy that sets it)")
	notifyMirror := flag.Bool("notify-mirror", true, "ask the public module mirror and checksum database to fetch each new version (skipped for local module hosts)")
	sumdb := flag.String("sumdb", "https://sum.golang.org", "checksum database to notify")
	admins := flag.String("admins", "", "comma-separated usernames who may review reports at /admin (they need two-factor authentication)")
	require2FA := flag.Bool("require-2fa", false, "refuse uploads from accounts without two-factor authentication")
	backupDir := flag.String("backup-dir", "", "directory for automatic database backups; empty disables them")
	backupEvery := flag.Duration("backup-every", 24*time.Hour, "time between automatic backups")
	backupKeep := flag.Int("backup-keep", 7, "automatic backups to keep")
	trusted := flag.Bool("trusted-publishing", true, "let GitHub Actions workflows publish with OIDC ID tokens instead of API tokens (off with -offline)")
	oidcAudience := flag.String("oidc-audience", "", "audience GitHub ID tokens must be requested for (default: the module host)")
	vulnDB := flag.String("vulndb", vulndb.DefaultUpstream, "public Go vulnerability database merged into /vulndb and used to flag vulnerable dependencies; empty turns it off (off with -offline)")
	playground := flag.String("playground", "https://play.golang.org", "Go Playground that documentation examples open in; empty hides the Run buttons (off with -offline)")
	publishChecks := flag.Bool("publish-checks", true, "scan uploads before publishing: refuse compiled programs, and send code that runs on import, encoded blobs and look-alike names for review")
	listingCache := flag.Duration("listing-cache", 30*time.Second, "how long the home page's listings and registry totals are cached; 0 turns caching off")
	debugAddr := flag.String("debug-addr", "", "serve Go profiling (net/http/pprof) on this address, e.g. localhost:6060; keep it off the public network")
	verbose := flag.Bool("v", false, "log debug messages")
	flag.Parse()
	if err := flagsFromEnv(flag.CommandLine, os.Getenv); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *baseURL == "" {
		*baseURL = "http://" + *addr
	}
	site, err := url.Parse(*baseURL)
	if err != nil || (site.Scheme != "http" && site.Scheme != "https") || site.Host == "" {
		return fmt.Errorf("-base-url %q must be an absolute http(s) URL", *baseURL)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var mailer mail.Mailer = mail.LogMailer{Log: log}
	if *smtpAddr != "" {
		mailer = mail.SMTPMailer{Addr: *smtpAddr, From: *smtpFrom, Username: *smtpUser, Password: os.Getenv("GOPHERDEX_SMTP_PASSWORD")}
	}
	accts := &accounts.Service{DB: db, Mailer: mailer, BaseURL: site.String(), Log: log}

	if *moduleHost == "" {
		*moduleHost = defaultModuleHost(site)
	}
	if err := xmodule.CheckPath(*moduleHost + "/owner/name"); err != nil {
		return fmt.Errorf("-module-host %q can't start a Go module path (it needs a dot and no port): %w", *moduleHost, err)
	}
	blobs, closeBlobs, err := blob.Open(*blobDir, os.Getenv)
	if err != nil {
		return err
	}
	defer closeBlobs()
	if s3, ok := blobs.(*blob.S3); ok {
		checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := s3.Check(checkCtx)
		cancel()
		if err != nil {
			return err
		}
	}
	reg := &registry.Registry{DB: db, Blobs: blobs, ModuleHost: *moduleHost, MaxZipSize: *maxUpload, Log: log, Require2FA: *require2FA, SkipChecks: !*publishChecks}

	if *backupDir != "" {
		go func() {
			t := time.NewTicker(*backupEvery)
			defer t.Stop()
			for {
				path, err := database.BackupTo(ctx, db, *backupDir, *backupKeep, time.Now())
				if err != nil {
					log.Error("database backup failed", "err", err)
				} else {
					log.Info("database backed up", "file", path)
				}
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
			}
		}()
	}

	go func() {
		if err := reg.BackfillRequires(ctx); err != nil && ctx.Err() == nil {
			log.Error("backfill requirements", "err", err)
		}
		if err := reg.Backfill(ctx); err != nil && ctx.Err() == nil {
			log.Error("backfill search index", "err", err)
		}
	}()
	downloads := &registry.Downloads{Registry: reg}
	downloads.Start()
	defer downloads.Stop()

	hosted := registry.Multi{reg}
	if *dataDir != "" {
		st, err := store.Open(*dataDir)
		if err != nil {
			return err
		}
		defer st.Close()
		hosted = append(hosted, st)
	}

	disc := &discovery.Service{Local: hosted, ProxyPrefix: "/api/proxy", DocsBase: *pkgsite, Log: log}
	if !*offline {
		hc := &http.Client{Timeout: 15 * time.Second}
		disc.Public = goproxy.NewClient(*upstream, hc)
		disc.Index = &discovery.PkgsiteIndex{Base: *pkgsite, HTTP: hc}
	}

	var notifier *mirror.Notifier
	if *notifyMirror && !*offline && !registry.IsLocalHost(*moduleHost) && site.Scheme == "https" {
		notifier = &mirror.Notifier{ProxyURL: *upstream, SumDBURL: *sumdb, Log: log}
	}

	proxy := goproxy.NewHandler(hosted, log)
	proxy.OnZip = downloads.Count

	var githubOIDC *oidc.Verifier
	if *trusted && !*offline {
		aud := *oidcAudience
		if aud == "" {
			aud = *moduleHost
		}
		githubOIDC = &oidc.Verifier{Issuer: oidc.GitHubIssuer, Audience: aud}
	}

	var vulnUpstream *vulndb.Upstream
	if *vulnDB != "" && !*offline {
		vulnUpstream = &vulndb.Upstream{URL: *vulnDB, Log: log}
		go vulnUpstream.Run(ctx, time.Hour)
	}

	handler, err := server.New(server.Config{
		Discovery:     disc,
		Accounts:      accts,
		Registry:      reg,
		Proxy:         proxy,
		Log:           log,
		SiteURL:       site.String(),
		ModuleHost:    *moduleHost,
		SecureCookies: site.Scheme == "https",
		TrustProxy:    *trustProxy,
		Mirror:        mirrorOrNil(notifier),
		Admins:        splitList(*admins),
		GitHubOIDC:    githubOIDC,
		ListingCache:  *listingCache,
		VulnDB:        vulnUpstream,
		PlaygroundURL: map[bool]string{true: "", false: *playground}[*offline],
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	for _, w := range launchWarnings(site, *smtpAddr, *admins, *backupDir, *trustProxy, *addr) {
		log.Warn(w)
	}

	if *debugAddr != "" {
		if host, _, err := net.SplitHostPort(*debugAddr); err != nil || !isLoopback(host) {
			return fmt.Errorf("-debug-addr %q must be a loopback address like localhost:6060: profiles expose internals", *debugAddr)
		}
		go func() {
			log.Info("profiling on", "addr", "http://"+*debugAddr+"/debug/pprof/")
			if err := http.ListenAndServe(*debugAddr, debugMux()); err != nil {
				log.Error("profiling server", "err", err)
			}
		}()
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("gopherdex listening", "version", version.String(),
			"site", site.String(), "goproxy", site.String()+"/api/proxy",
			"modules", *moduleHost+"/<user>/<module>", "db", *dbPath, "blobs", *blobDir,
			"zero_config_go_get", !registry.IsLocalHost(*moduleHost) && site.Scheme == "https",
			"notify_mirror", notifier != nil, "offline", *offline, "smtp", *smtpAddr != "")
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("listen on %s: %w", *addr, err)
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if notifier != nil {
		done := make(chan struct{})
		go func() { notifier.Wait(); close(done) }()
		select {
		case <-done:
		case <-shutdownCtx.Done():
			log.Warn("exiting with mirror notifications still pending")
		}
	}
	return nil
}

// mirrorOrNil avoids handing the server a typed nil interface.
func mirrorOrNil(n *mirror.Notifier) interface{ Notify(string, string) } {
	if n == nil {
		return nil
	}
	return n
}

// defaultModuleHost derives the module domain from the site URL. Module
// paths can't carry a port or a dot-less host, so local development uses
// gopherdex.localhost.
func defaultModuleHost(site *url.URL) string {
	host := site.Hostname()
	if !strings.Contains(host, ".") || net.ParseIP(host) != nil {
		return "gopherdex.localhost"
	}
	return strings.ToLower(host)
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// flagsFromEnv lets every flag be set from the environment, which suits
// containers: -base-url becomes GOPHERDEX_BASE_URL. Flags given on the
// command line win.
func flagsFromEnv(fs *flag.FlagSet, getenv func(string) string) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	var err error
	fs.VisitAll(func(f *flag.Flag) {
		if set[f.Name] || err != nil {
			return
		}
		key := envName(f.Name)
		if v := getenv(key); v != "" {
			if e := f.Value.Set(v); e != nil {
				err = fmt.Errorf("%s=%q: %w", key, v, e)
			}
		}
	})
	return err
}

func envName(flagName string) string {
	return "GOPHERDEX_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// launchWarnings points out settings that are fine for development but
// wrong for a public registry.
func launchWarnings(site *url.URL, smtpAddr, admins, backupDir string, trustProxy bool, addr string) []string {
	if site.Scheme != "https" {
		return nil // development
	}
	var w []string
	if smtpAddr == "" {
		w = append(w, "no -smtp-addr: emails are only logged, so people can't verify their address or reset their password")
	}
	if admins == "" {
		w = append(w, "no -admins: nobody can review reports or quarantine modules")
	}
	if backupDir == "" {
		w = append(w, "no -backup-dir: the database isn't backed up automatically")
	}
	if host, _, err := net.SplitHostPort(addr); err == nil && !trustProxy && (host == "127.0.0.1" || host == "localhost" || host == "::1") {
		w = append(w, "listening on loopback behind what looks like a reverse proxy, without -trust-proxy: rate limits will treat every visitor as the proxy")
	}
	return w
}

// debugMux serves net/http/pprof's handlers, only on -debug-addr.
func debugMux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/debug/pprof/", pprof.Index)
	m.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	m.HandleFunc("/debug/pprof/profile", pprof.Profile)
	m.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	m.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return m
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
