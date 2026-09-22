package bench

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	xmodule "golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/blob"
	"github.com/parthiban-sivakumar/gopherdex/internal/database"
	"github.com/parthiban-sivakumar/gopherdex/internal/mail"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
)

// SeedConfig describes a dataset to build.
type SeedConfig struct {
	DB, Blobs, Out string // new database file, blob directory, and where to write modules.tsv and the report
	ModuleHost     string
	Upstream       *Upstream

	Real     int // real modules sampled from the index
	Zips     int // of those, how many get their real source (the rest get generated source)
	Modules  int // total modules, padded with synthetic ones
	Versions int // total versions, roughly
	Users    int // publisher accounts
	Orgs     int // organizations, among the namespaces

	Since   time.Time // oldest point sampled from the index
	Windows int       // index windows spread between Since and now

	Concurrency     int // upstream fetches and zip building
	DownloadsPerDay int // total downloads per day, spread over 30 days by popularity
	SkipChecks      bool
	Seed            uint64

	Log      *slog.Logger
	Progress io.Writer
}

// SeedReport is what seeding did and how long it took.
type SeedReport struct {
	Modules, RealModules, RealZips, Versions, Users, Orgs int
	Rejected                                              map[string]int // publish errors by code
	Warned                                                int            // versions published with publish-check warnings
	Phases                                                []Phase
	Publish                                               Latency // every publish
	FirstPublish                                          Latency // publishes that created a module (runs name checks)
	DBBytes, BlobBytes                                    int64
}

// Phase is how long one step took.
type Phase struct {
	Name     string
	Duration time.Duration
}

type nopMailer struct{}

func (nopMailer) Send(context.Context, mail.Message) error { return nil }

// Seed builds the dataset described by cfg.
func Seed(ctx context.Context, cfg SeedConfig) (*SeedReport, error) {
	if _, err := os.Stat(cfg.DB); err == nil {
		return nil, fmt.Errorf("%s already exists; seeding needs a new database (delete it or choose another -db)", cfg.DB)
	}
	for _, dir := range []string{cfg.Out, filepath.Dir(cfg.DB), cfg.Upstream.Cache} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	if err := fenceOffFromGo(filepath.Dir(cfg.Upstream.Cache)); err != nil {
		return nil, err
	}
	s := &seeder{cfg: cfg, rnd: mrand.New(mrand.NewPCG(cfg.Seed, 1)), rep: &SeedReport{Rejected: map[string]int{}}}
	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{"sample the module index", s.sample},
		{"fetch go.mod files and zips", s.fetch},
		{"plan namespaces and modules", s.plan},
		{"create accounts and organizations", s.accounts},
		{"publish", s.publish},
		{"record download history", s.downloads},
		{"index dependencies and search (server start-up work)", s.backfill},
		{"write modules.tsv", s.write},
	}
	defer func() {
		if s.db != nil {
			s.db.Close()
		}
	}()
	for _, st := range steps {
		s.logf("== %s", st.name)
		start := time.Now()
		if err := st.run(ctx); err != nil {
			return s.rep, fmt.Errorf("%s: %w", st.name, err)
		}
		s.rep.Phases = append(s.rep.Phases, Phase{st.name, time.Since(start).Round(time.Millisecond)})
	}
	s.rep.DBBytes = fileSize(cfg.DB) + fileSize(cfg.DB+"-wal")
	s.rep.BlobBytes = dirSize(cfg.Blobs)
	return s.rep, nil
}

type seeder struct {
	cfg SeedConfig
	rnd *mrand.Rand
	rep *SeedReport

	sampled   map[string][]IndexEntry // real module → versions seen in the index
	real      []string                // sampled paths, in a stable order
	upMods    map[string][]byte       // real module → go.mod of its latest version
	upZips    map[string]string       // real module → cached zip of its latest version
	modules   []*Module
	remap     map[string]string // real path → registry path
	nsOrder   []string          // namespaces, users first
	orgs      map[string]string // org → owning user
	publisher map[string]string // namespace → user who publishes there

	db      *sql.DB
	reg     *registry.Registry
	clock   time.Time
	users   map[string]*accounts.User
	tokens  map[string]*accounts.Token
	lastLog time.Time
}

func (s *seeder) logf(format string, args ...any) {
	if s.cfg.Progress != nil {
		fmt.Fprintf(s.cfg.Progress, format+"\n", args...)
	}
}

// ---- Sampling ----

// sample reads the module index in windows spread over time, so the set
// mixes old and new modules, and keeps tagged releases only.
func (s *seeder) sample(ctx context.Context) error {
	s.sampled = map[string][]IndexEntry{}
	if s.cfg.Real <= 0 {
		return nil
	}
	windows := max(1, s.cfg.Windows)
	end := time.Now().Add(-time.Hour)
	step := end.Sub(s.cfg.Since) / time.Duration(windows)
	quota := s.cfg.Real/windows + 1
	results := make([]map[string][]IndexEntry, windows)
	sem := make(chan struct{}, max(1, s.cfg.Concurrency))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for w := range windows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			got, err := s.window(ctx, s.cfg.Since.Add(time.Duration(w)*step), s.cfg.Since.Add(time.Duration(w+1)*step), quota)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			results[w] = got
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	for _, got := range results {
		paths := make([]string, 0, len(got))
		for p := range got {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		s.rnd.Shuffle(len(paths), func(i, j int) { paths[i], paths[j] = paths[j], paths[i] })
		taken := 0
		for _, p := range paths {
			if len(s.real) >= s.cfg.Real || taken >= quota {
				break
			}
			if _, dup := s.sampled[p]; dup {
				s.sampled[p] = append(s.sampled[p], got[p]...)
				continue
			}
			s.sampled[p] = got[p]
			s.real = append(s.real, p)
			taken++
		}
	}
	s.logf("   %d real modules sampled", len(s.real))
	return nil
}

func (s *seeder) window(ctx context.Context, from, until time.Time, quota int) (map[string][]IndexEntry, error) {
	got := map[string][]IndexEntry{}
	since := from
	for len(got) < quota && since.Before(until) {
		page, err := s.cfg.Upstream.IndexPage(ctx, since)
		if err != nil {
			return nil, err
		}
		for _, e := range page {
			if releaseVersion(e.Path, e.Version) {
				got[e.Path] = append(got[e.Path], e)
			}
		}
		if len(page) < 2000 || !page[len(page)-1].Timestamp.After(since) {
			break
		}
		since = page[len(page)-1].Timestamp
	}
	return got, nil
}

func releaseVersion(p, v string) bool {
	return xmodule.CheckPath(p) == nil && semver.IsValid(v) && semver.Canonical(v) == v &&
		!strings.Contains(v, "+") && !xmodule.IsPseudoVersion(v)
}

// ---- Fetching ----

func (s *seeder) fetch(ctx context.Context) error {
	s.upMods, s.upZips = map[string][]byte{}, map[string]string{}
	withZip := map[string]bool{}
	for _, i := range s.rnd.Perm(len(s.real))[:min(s.cfg.Zips, len(s.real))] {
		withZip[s.real[i]] = true
	}
	var mu sync.Mutex
	var done, failed, zips int
	err := parallel(ctx, s.cfg.Concurrency, s.real, func(p string) error {
		latest := latestOf(s.sampled[p])
		mod, err := s.cfg.Upstream.GoMod(ctx, p, latest)
		zip := ""
		if err == nil && withZip[p] {
			zip, _ = s.cfg.Upstream.Zip(ctx, p, latest) // too large or gone: generated source instead
		}
		mu.Lock()
		defer mu.Unlock()
		done++
		if err != nil {
			failed++ // removed from the mirror, or never had a go.mod: skip it
		} else {
			s.upMods[p] = mod
			if zip != "" {
				s.upZips[p] = zip
				zips++
			}
		}
		if done%1000 == 0 {
			s.logf("   %d/%d fetched (%d zips, %d unavailable)", done, len(s.real), zips, failed)
		}
		return ctx.Err()
	})
	s.logf("   %d go.mod files, %d zips, %d modules unavailable upstream", len(s.upMods), zips, failed)
	return err
}

func latestOf(es []IndexEntry) string {
	best := ""
	for _, e := range es {
		if best == "" || semver.Compare(e.Version, best) > 0 {
			best = e.Version
		}
	}
	return best
}

func parallel[T any](ctx context.Context, n int, items []T, fn func(T) error) error {
	ch := make(chan T)
	errc := make(chan error, 1)
	var wg sync.WaitGroup
	for range max(1, n) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range ch {
				if err := fn(it); err != nil {
					select {
					case errc <- err:
					default:
					}
				}
			}
		}()
	}
	var err error
feed:
	for _, it := range items {
		select {
		case ch <- it:
		case err = <-errc:
			break feed
		case <-ctx.Done():
			err = ctx.Err()
			break feed
		}
	}
	close(ch)
	wg.Wait()
	if err == nil {
		select {
		case err = <-errc:
		default:
		}
	}
	return err
}

// ---- Planning ----

func (s *seeder) plan(ctx context.Context) error {
	// Namespaces: the most prolific real owners get their own; the rest
	// share, as smaller publishers would.
	count, vanity := map[string]int{}, map[string]bool{}
	for p := range s.upMods {
		o := owner(p)
		count[o]++
		vanity[o] = vanity[o] || !codeHosts[strings.Split(p, "/")[0]]
	}
	owners := make([]string, 0, len(count))
	for o := range count {
		owners = append(owners, o)
	}
	slices.SortFunc(owners, func(a, b string) int { return cmp.Or(count[b]-count[a], strings.Compare(a, b)) })
	pool := max(2, s.cfg.Users+s.cfg.Orgs)
	nsOf, seen := map[string]string{}, map[string]bool{}
	var names []string
	for _, o := range owners {
		if len(names) == pool {
			break
		}
		if n := namespaceName(o); n != "" && !seen[n] {
			seen[n], nsOf[o] = true, n
			names = append(names, n)
		}
	}
	for i := 0; len(names) < pool; i++ {
		n := fmt.Sprintf("%s-%s-%d", nameWords[i%len(nameWords)], nameWords[(i/len(nameWords)+7)%len(nameWords)], i)
		if !seen[n] && accounts.ValidateNamespace(n) == nil {
			seen[n] = true
			names = append(names, n)
		}
	}
	// Organizations: vanity-domain owners first (go.uber.org, k8s.io …).
	isOrg := map[string]bool{}
	for _, o := range owners {
		if n, ok := nsOf[o]; ok && vanity[o] && len(isOrg) < s.cfg.Orgs {
			isOrg[n] = true
		}
	}
	for _, n := range names {
		if len(isOrg) >= s.cfg.Orgs {
			break
		}
		isOrg[n] = true
	}
	var users, orgs []string
	for _, n := range names {
		if isOrg[n] {
			orgs = append(orgs, n)
		} else {
			users = append(users, n)
		}
	}
	if len(users) == 0 {
		return errors.New("no user namespaces; raise -users")
	}
	s.nsOrder = append(users, orgs...)
	s.orgs, s.publisher = map[string]string{}, map[string]string{}
	for _, u := range users {
		s.publisher[u] = u
	}
	for i, o := range orgs {
		s.orgs[o] = users[i%len(users)]
		s.publisher[o] = users[i%len(users)]
	}
	nsFor := func(o string) string {
		if n, ok := nsOf[o]; ok {
			return n
		}
		h := fnv.New32a()
		h.Write([]byte(o))
		return names[int(h.Sum32())%len(names)]
	}

	taken := map[string]bool{}
	unique := func(ns, name, major string) string {
		for i := 1; ; i++ {
			n := name
			if i > 1 {
				n = fmt.Sprintf("%s-%d", name, i)
			}
			p := s.cfg.ModuleHost + "/" + ns + "/" + n + major
			if !taken[p] {
				taken[p] = true
				return p
			}
		}
	}

	// Real modules.
	s.remap = map[string]string{}
	for _, src := range s.real {
		mod, ok := s.upMods[src]
		if !ok {
			continue
		}
		ns := nsFor(owner(src))
		name, major := baseName(src)
		m := &Module{Source: src, Namespace: ns, upstreamMod: mod, zip: s.upZips[src]}
		m.Path = unique(ns, name, major)
		_, pathMajor, _ := xmodule.SplitPathVersion(m.Path)
		seenV := map[string]bool{}
		for _, e := range s.sampled[src] {
			if !seenV[e.Version] && xmodule.CheckPathMajor(e.Version, pathMajor) == nil {
				seenV[e.Version] = true
				m.Versions = append(m.Versions, Version{e.Version, e.Timestamp})
			}
		}
		slices.SortFunc(m.Versions, func(a, b Version) int { return semver.Compare(a.V, b.V) })
		if len(m.Versions) > 20 {
			m.Versions = m.Versions[len(m.Versions)-20:]
		}
		if len(m.Versions) == 0 || m.Latest() != latestOf(s.sampled[src]) {
			m.zip = "" // its latest version was dropped, so the real zip doesn't match
		}
		if len(m.Versions) == 0 {
			m.Versions = []Version{{"v1.0.0", s.sampled[src][0].Timestamp}}
		}
		m.Synopsis = synopsisFor(s.rnd, name)
		s.remap[src] = m.Path
		s.modules = append(s.modules, m)
	}
	realCount, realVersions := len(s.modules), 0
	for _, m := range s.modules {
		realVersions += len(m.Versions)
		if m.zip != "" {
			s.rep.RealZips++
		}
	}

	// Synthetic modules fill the rest, with some publishers far busier than
	// others and roughly geometric version counts.
	synth := max(0, s.cfg.Modules-realCount)
	mean := 1.0
	if synth > 0 {
		mean = max(1, float64(s.cfg.Versions-realVersions)/float64(synth))
	}
	now := time.Now()
	for range synth {
		ns := names[int(math.Pow(s.rnd.Float64(), 2.5)*float64(len(names)))]
		var parts []string
		for range 2 + s.rnd.IntN(2) {
			parts = append(parts, nameWords[s.rnd.IntN(len(nameWords))])
		}
		name := slug(strings.Join(parts, "-"))
		m := &Module{Namespace: ns, Path: unique(ns, name, "")}
		start := now.Add(-time.Duration(30+s.rnd.IntN(5*365)) * 24 * time.Hour)
		end := start.Add(time.Duration(s.rnd.Int64N(int64(now.Sub(start))) + 1))
		n := max(1, int(math.Round(s.rnd.ExpFloat64()*mean)))
		m.Versions = syntheticVersions(s.rnd, min(n, 200), start, end)
		m.Synopsis = synopsisFor(s.rnd, name)
		// Depend on a few modules published before this one.
		var req strings.Builder
		fmt.Fprintf(&req, "module %s\n\ngo 1.2%d\n", m.Path, s.rnd.IntN(4))
		if k := s.rnd.IntN(6); k > 0 && len(s.modules) > 0 {
			req.WriteString("\nrequire (\n")
			dup := map[string]bool{}
			for range k {
				d := s.modules[s.rnd.IntN(len(s.modules))]
				if !dup[d.Path] {
					dup[d.Path] = true
					fmt.Fprintf(&req, "\t%s %s\n", d.Path, d.Versions[0].V)
				}
			}
			req.WriteString(")\n")
		}
		m.upstreamMod = []byte(req.String())
		s.modules = append(s.modules, m)
	}
	for _, m := range s.modules {
		s.rep.Versions += len(m.Versions)
	}
	s.rep.Modules, s.rep.RealModules = len(s.modules), realCount
	s.rep.Users, s.rep.Orgs = len(users), len(orgs)
	s.logf("   %d modules (%d real, %d with real source), %d versions, %d users, %d organizations",
		s.rep.Modules, realCount, s.rep.RealZips, s.rep.Versions, len(users), len(orgs))
	return nil
}

// ---- Accounts ----

func (s *seeder) accounts(ctx context.Context) error {
	db, err := database.Open(ctx, s.cfg.DB)
	if err != nil {
		return err
	}
	s.db = db
	blobs, err := blob.OpenFS(s.cfg.Blobs)
	if err != nil {
		return err
	}
	s.reg = &registry.Registry{DB: db, Blobs: blobs, ModuleHost: s.cfg.ModuleHost, MaxZipSize: registry.DefaultMaxZipSize,
		Log: s.cfg.Log, SkipChecks: s.cfg.SkipChecks, Now: func() time.Time { return s.clock }}
	acc := &accounts.Service{DB: db, Mailer: nopMailer{}, BaseURL: "http://" + s.cfg.ModuleHost, Log: s.cfg.Log}
	s.users, s.tokens = map[string]*accounts.User{}, map[string]*accounts.Token{}
	c := accounts.Client{IP: "192.0.2.1"}
	for _, name := range s.nsOrder {
		if _, isOrg := s.orgs[name]; isOrg {
			continue
		}
		pw := make([]byte, 18)
		rand.Read(pw)
		u, err := acc.Register(ctx, name, name+"@bench.invalid", hex.EncodeToString(pw), c)
		if err != nil {
			return fmt.Errorf("register %s: %w", name, err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE users SET email_verified_at = ? WHERE id = ?`, time.Now().Unix(), u.ID); err != nil {
			return err
		}
		u.EmailVerified = true
		_, tok, err := acc.CreateToken(ctx, u, "bench", 0, "", c)
		if err != nil {
			return err
		}
		s.users[name], s.tokens[name] = u, tok
	}
	s.clock = time.Now()
	for _, org := range s.nsOrder {
		ownerName, ok := s.orgs[org]
		if !ok {
			continue
		}
		if err := s.reg.CreateOrg(ctx, s.users[ownerName], org, strings.ToUpper(org[:1])+org[1:], c); err != nil {
			return fmt.Errorf("create org %s: %w", org, err)
		}
		// A couple of other members, so teams and member lists have people.
		for i := range 2 {
			m := s.nsOrder[(len(org)*7+i*13)%len(s.users)]
			if m != ownerName {
				s.reg.SetOrgMember(ctx, s.users[ownerName], org, m, "member", c)
			}
		}
	}
	return nil
}

// ---- Publishing ----

type built struct {
	m    *Module
	zips []string
	err  error
}

func (s *seeder) publish(ctx context.Context) error {
	tmp, err := os.MkdirTemp(s.cfg.Out, "zips-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // stops the zip builders if publishing fails
	jobs := make(chan *Module)
	out := make(chan built, 2*max(1, s.cfg.Concurrency))
	var wg sync.WaitGroup
	for w := range max(1, s.cfg.Concurrency) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := mrand.New(mrand.NewPCG(s.cfg.Seed, uint64(w)+100))
			for m := range jobs {
				b := built{m: m}
				for _, v := range m.Versions {
					z, err := writeZip(r, tmp, m, v.V, s.remap)
					if err != nil {
						b.err = err
						break
					}
					b.zips = append(b.zips, z)
				}
				select {
				case out <- b:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, m := range s.modules {
			select {
			case jobs <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(out) }()

	var all, first []time.Duration
	var generic int
	start, published := time.Now(), 0
	c := accounts.Client{IP: "192.0.2.1"}
	for b := range out {
		if b.err != nil {
			return fmt.Errorf("build %s: %w", b.m.Path, b.err)
		}
		user := s.publisher[b.m.Namespace]
		for i, z := range b.zips {
			v := b.m.Versions[i]
			s.clock = v.Time
			t0 := time.Now()
			res, err := s.reg.Publish(ctx, registry.Upload{User: s.users[user], Token: s.tokens[user], Client: c,
				Module: b.m.Path, Version: v.V, ZipFile: z})
			d := time.Since(t0)
			os.Remove(z)
			all = append(all, d)
			if i == 0 {
				first = append(first, d)
			}
			var re *registry.Error
			switch {
			case err == nil:
				published++
				b.m.published = append(b.m.published, v.V)
				if len(res.Warnings) > 0 {
					s.rep.Warned++
				}
			case errors.As(err, &re):
				s.rep.Rejected[re.Code]++
			default:
				s.rep.Rejected["error"]++
				if generic++; generic <= 5 {
					s.logf("   publish %s@%s: %v", b.m.Path, v.V, err)
				}
				if generic > 200 {
					return fmt.Errorf("too many publish errors; last: %w", err)
				}
			}
		}
		if time.Since(s.lastLog) > 3*time.Second {
			s.lastLog = time.Now()
			rate := float64(published) / time.Since(start).Seconds()
			s.logf("   %d/%d versions published (%.0f/s, last first-publish %s), rejected %d",
				published, s.rep.Versions, rate, first[len(first)-1].Round(time.Microsecond), sum(s.rep.Rejected))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	s.rep.Publish, s.rep.FirstPublish = summarize(all), summarize(first)
	s.logf("   %d versions published in %s", published, time.Since(start).Round(time.Second))
	return nil
}

func sum(m map[string]int) (n int) {
	for _, v := range m {
		n += v
	}
	return n
}

// ---- Downloads ----

// downloads records 30 days of download history, Zipf-distributed over the
// modules (real ones tend to rank higher), mostly for latest versions.
func (s *seeder) downloads(ctx context.Context) error {
	if s.cfg.DownloadsPerDay <= 0 {
		return nil
	}
	type ranked struct {
		m   *Module
		key float64
	}
	order := make([]ranked, len(s.modules))
	for i, m := range s.modules {
		k := s.rnd.Float64()
		if m.Source != "" {
			k *= 0.3
		}
		order[i] = ranked{m, k}
	}
	slices.SortFunc(order, func(a, b ranked) int { return cmp.Compare(a.key, b.key) })
	weights, total := make([]float64, len(order)), 0.0
	for i := range order {
		weights[i] = 1 / math.Pow(float64(i+1), 1.1)
		total += weights[i]
	}
	dl := &registry.Downloads{Registry: s.reg}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	for day := 29; day >= 0; day-- {
		s.clock = today.Add(-time.Duration(day) * 24 * time.Hour).Add(12 * time.Hour)
		for i, o := range order {
			n := float64(s.cfg.DownloadsPerDay) * weights[i] / total * (0.6 + 0.8*s.rnd.Float64())
			if n < 1 {
				if s.rnd.Float64() >= n {
					continue
				}
				n = 1
			}
			latest := int(n * 0.8)
			if latest > 0 {
				dl.CountN(o.m.Path, o.m.Latest(), latest)
			}
			if rest := int(n) - latest; rest > 0 {
				dl.CountN(o.m.Path, o.m.Versions[s.rnd.IntN(len(o.m.Versions))].V, rest)
			}
		}
		if err := dl.Flush(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (s *seeder) backfill(ctx context.Context) error {
	if err := s.reg.BackfillRequires(ctx); err != nil {
		return err
	}
	return s.reg.Backfill(ctx)
}

// ---- Output ----

// write saves modules.tsv for the load generator: path, latest published
// version, published version count, namespace and the real module it came
// from. Modules whose versions were all rejected are left out.
func (s *seeder) write(ctx context.Context) error {
	f, err := os.Create(filepath.Join(s.cfg.Out, "modules.tsv"))
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, m := range s.modules {
		if len(m.published) == 0 {
			continue // every version was rejected
		}
		latest := m.published[0]
		for _, v := range m.published {
			if semver.Compare(v, latest) > 0 {
				latest = v
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", m.Path, latest, len(m.published), m.Namespace, m.Source)
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

func dirSize(dir string) (n int64) {
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// fenceOffFromGo puts a go.mod in the benchmark directory. The go command
// treats it as a separate module and leaves it out of ./... patterns, which
// would otherwise walk every cached file and published blob.
func fenceOffFromGo(dir string) error {
	p := filepath.Join(dir, "go.mod")
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	return os.WriteFile(p, []byte("// Benchmark data from gdxbench; this file keeps go ./... out of it.\nmodule gdxbench.invalid/data\n"), 0o644)
}
