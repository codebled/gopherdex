package bench

import (
	"archive/zip"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	xmodule "golang.org/x/mod/module"
	"golang.org/x/mod/semver"
	modzip "golang.org/x/mod/zip"

	"github.com/codebled/gopherdex/internal/accounts"
)

// Module is one module to publish.
type Module struct {
	Source    string // original path on the public mirror; "" for synthetic modules
	Path      string // path on this registry
	Namespace string
	Versions  []Version // oldest first
	Synopsis  string
	Words     []string // name words, for search queries and README text

	published   []string // versions the registry accepted
	upstreamMod []byte   // real go.mod of the latest version, if known
	zip         string   // cached real zip of the latest version, if any
}

// Version is one release of a Module.
type Version struct {
	V    string
	Time time.Time
}

// Latest returns the newest version.
func (m *Module) Latest() string { return m.Versions[len(m.Versions)-1].V }

// name is the module's last path element, without a major version suffix.
func (m *Module) name() string {
	prefix, _, _ := xmodule.SplitPathVersion(m.Path)
	return path.Base(prefix)
}

// ---- Names ----

var notNameChar = regexp.MustCompile(`[^a-z0-9]+`)

// slug lower-cases s and turns anything other than letters and digits into
// single hyphens.
func slug(s string) string {
	return strings.Trim(notNameChar.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

var codeHosts = map[string]bool{"github.com": true, "gitlab.com": true, "bitbucket.org": true, "codeberg.org": true, "gitee.com": true, "git.sr.ht": true}

// owner guesses who publishes a public module: the account on a code host
// (github.com/spf13/cobra → spf13) or the organization behind a vanity
// domain (go.uber.org/zap → uber).
func owner(p string) string {
	elems := strings.Split(p, "/")
	if codeHosts[elems[0]] && len(elems) > 1 {
		return strings.TrimPrefix(elems[1], "~")
	}
	labels := strings.Split(elems[0], ".")
	if len(labels) >= 2 {
		return labels[len(labels)-2]
	}
	return labels[0]
}

// namespaceName turns an owner into a valid, unreserved namespace.
func namespaceName(o string) string {
	n := slug(o)
	if len(n) > 30 {
		n = strings.Trim(n[:30], "-")
	}
	for len(n) < 2 {
		n += "x"
	}
	if accounts.ValidateNamespace(n) != nil {
		n += "-dev"
	}
	if accounts.ValidateNamespace(n) != nil {
		return ""
	}
	return n
}

// baseName is a public module's name without its major version suffix:
// github.com/spf13/cobra → cobra, gopkg.in/yaml.v3 → yaml.
func baseName(p string) (name, pathMajor string) {
	prefix, major, _ := xmodule.SplitPathVersion(p)
	if strings.HasPrefix(major, ".v") { // gopkg.in
		major = ""
	}
	n := slug(path.Base(prefix))
	if len(n) > 60 {
		n = strings.Trim(n[:60], "-")
	}
	if n == "" {
		n = "module"
	}
	return n, major
}

// ---- Synthetic text ----

var topics = []string{
	"HTTP routing and middleware", "structured logging", "retries with exponential backoff", "a key-value cache",
	"JSON encoding and decoding", "YAML configuration files", "command-line interfaces", "database migrations",
	"a PostgreSQL driver", "rate limiting", "consistent hashing", "a worker pool", "feature flags", "OAuth 2.0 clients",
	"JWT tokens", "gRPC interceptors", "Prometheus metrics", "OpenTelemetry tracing", "S3-compatible object storage",
	"file watching", "cron schedules", "UUID generation", "semantic version parsing", "terminal colors", "progress bars",
	"table-driven test helpers", "mock generation", "dependency injection", "an event bus", "WebSocket connections",
	"TLS certificates", "SSH clients", "Markdown rendering", "HTML templates", "image resizing", "PDF generation",
	"CSV parsing", "time zone handling", "money and currency arithmetic", "geospatial queries", "bloom filters",
	"a B-tree", "a Raft consensus implementation", "service discovery", "circuit breakers", "Kubernetes controllers",
	"Docker API clients", "environment variables", "secrets management", "email sending", "Slack webhooks",
	"validation of structs", "an in-memory SQL engine", "compression", "checksums", "password hashing", "Redis clients",
	"message queues", "a static site generator", "i18n and translations",
}

var nameWords = []string{
	"fast", "go", "tiny", "simple", "kit", "lite", "json", "yaml", "log", "cache", "retry", "http", "router", "mux",
	"sql", "orm", "cli", "flag", "config", "env", "queue", "worker", "pool", "stream", "sync", "atomic", "graph",
	"tree", "hash", "crypto", "jwt", "auth", "oauth", "grpc", "proto", "rpc", "rest", "api", "client", "server",
	"proxy", "gateway", "limit", "rate", "breaker", "trace", "metric", "prom", "otel", "bus", "event", "pubsub",
	"kafka", "nats", "redis", "mongo", "pg", "mysql", "sqlite", "s3", "blob", "fs", "watch", "cron", "uuid", "semver",
	"color", "term", "table", "test", "mock", "assert", "di", "wire", "ws", "tls", "ssh", "md", "html", "tmpl",
	"image", "pdf", "csv", "time", "money", "geo", "bloom", "btree", "raft", "consul", "k8s", "docker", "secret",
	"mail", "slack", "valid", "compress", "zstd", "gzip", "crc", "argon", "bcrypt", "i18n",
}

func synopsisFor(r *rand.Rand, name string) string {
	return fmt.Sprintf("Package %s provides %s for Go programs.", pkgName(name), topics[r.IntN(len(topics))])
}

func pkgName(name string) string {
	p := strings.ReplaceAll(slug(name), "-", "")
	if p == "" || (p[0] >= '0' && p[0] <= '9') {
		p = "pkg" + p
	}
	return p
}

func readme(r *rand.Rand, m *Module) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n%s\n\n## Install\n\n```\ngo get %s@%s\n```\n\n## Features\n\n", m.name(), m.Synopsis, m.Path, m.Latest())
	for range 3 + r.IntN(5) {
		fmt.Fprintf(&b, "- Support for %s\n", topics[r.IntN(len(topics))])
	}
	b.WriteString("\n## Usage\n\n")
	for range 2 + r.IntN(4) {
		for range 3 + r.IntN(4) {
			fmt.Fprintf(&b, "It handles %s and works well with %s. ", topics[r.IntN(len(topics))], topics[r.IntN(len(topics))])
		}
		b.WriteString("\n\n")
	}
	return b.String()
}

const mitLicense = `MIT License

Copyright (c) %d %s

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`

const bsdLicense = `Copyright (c) %d %s. All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

3. Neither the name of the copyright holder nor the names of its
   contributors may be used to endorse or promote products derived from
   this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
`

func goSource(r *rand.Rand, m *Module, version string) string {
	pkg := pkgName(m.name())
	var b strings.Builder
	fmt.Fprintf(&b, "// %s\npackage %s\n\nimport \"errors\"\n\n// Version is this release.\nconst Version = %q\n\n", m.Synopsis, pkg, version)
	b.WriteString("// ErrClosed is returned after Close.\nvar ErrClosed = errors.New(\"closed\")\n\n")
	b.WriteString("// Options configure a Client.\ntype Options struct {\n\t// Retries is how many times to retry.\n\tRetries int\n\t// Name identifies the client in logs.\n\tName string\n}\n\n")
	b.WriteString("// Client is safe for concurrent use.\ntype Client struct {\n\topts   Options\n\tclosed bool\n}\n\n")
	b.WriteString("// New returns a Client.\nfunc New(opts Options) *Client { return &Client{opts: opts} }\n\n")
	for i := range 2 + r.IntN(6) {
		w := nameWords[r.IntN(len(nameWords))]
		fmt.Fprintf(&b, "// %s%d handles %s.\nfunc (c *Client) %s%d(input string) (string, error) {\n\tif c.closed {\n\t\treturn \"\", ErrClosed\n\t}\n\treturn input, nil\n}\n\n",
			strings.ToUpper(w[:1])+w[1:], i, topics[r.IntN(len(topics))], strings.ToUpper(w[:1])+w[1:], i)
	}
	b.WriteString("// Close releases the client.\nfunc (c *Client) Close() error {\n\tc.closed = true\n\treturn nil\n}\n")
	return b.String()
}

// ---- go.mod ----

// rewriteGoMod builds a go.mod for the remapped module from the upstream one:
// the new module path, the real go version, and the real requirements, with
// modules that are also in the dataset pointed at their new paths so the
// dependency graph ("used by") is realistic.
func rewriteGoMod(upstream []byte, newPath string, remap map[string]string) []byte {
	goVersion, reqs := "1.21", []string(nil)
	if f, err := modfile.ParseLax("go.mod", upstream, nil); err == nil {
		if f.Go != nil {
			goVersion = f.Go.Version
		}
		for _, r := range f.Require {
			p := r.Mod.Path
			if np, ok := remap[p]; ok {
				p = np
			}
			if xmodule.Check(p, r.Mod.Version) == nil {
				line := p + " " + r.Mod.Version
				if r.Indirect {
					line += " // indirect"
				}
				reqs = append(reqs, line)
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "module %s\n\ngo %s\n", newPath, goVersion)
	if len(reqs) > 0 {
		b.WriteString("\nrequire (\n")
		for _, r := range reqs {
			b.WriteString("\t" + r + "\n")
		}
		b.WriteString(")\n")
	}
	return []byte(b.String())
}

// ---- Zips ----

// writeZip writes a module zip for m@version into dir and returns its path.
// Real modules with a cached upstream zip keep all of that zip's files, with
// the go.mod rewritten; the rest get generated source.
func writeZip(r *rand.Rand, dir string, m *Module, version string, remap map[string]string) (string, error) {
	f, err := os.CreateTemp(dir, "*.zip")
	if err != nil {
		return "", err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	prefix := m.Path + "@" + version + "/"
	add := func(name, body string) error {
		w, err := zw.Create(prefix + name)
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, body)
		return err
	}
	goMod := rewriteGoMod(m.upstreamMod, m.Path, remap)
	if m.zip != "" && version == m.Latest() {
		if err := repack(zw, m, prefix, goMod); err != nil {
			return "", err
		}
	} else {
		if err := add("go.mod", string(goMod)); err != nil {
			return "", err
		}
		if err := add(pkgName(m.name())+".go", goSource(r, m, version)); err != nil {
			return "", err
		}
		if err := add("README.md", readme(r, m)); err != nil {
			return "", err
		}
		switch r.IntN(10) {
		case 0: // no license
		case 1, 2, 3:
			err = add("LICENSE", fmt.Sprintf(bsdLicense, 2015+r.IntN(10), "The "+m.Namespace+" Authors"))
		default:
			err = add("LICENSE", fmt.Sprintf(mitLicense, 2015+r.IntN(10), m.Namespace))
		}
		if err != nil {
			return "", err
		}
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// repack copies a real module zip under a new path prefix.
func repack(zw *zip.Writer, m *Module, prefix string, goMod []byte) error {
	zr, err := zip.OpenReader(m.zip)
	if err != nil {
		return err
	}
	defer zr.Close()
	wrote := false
	want := m.Source + "@" + m.Latest() + "/"
	for _, zf := range zr.File {
		rel, ok := strings.CutPrefix(zf.Name, want)
		if !ok || rel == "" || strings.HasSuffix(rel, "/") {
			continue
		}
		w, err := zw.Create(prefix + rel)
		if err != nil {
			return err
		}
		if rel == "go.mod" {
			_, err = w.Write(goMod)
			wrote = true
		} else {
			var rc io.ReadCloser
			if rc, err = zf.Open(); err == nil {
				// No file in a valid module zip is larger than the whole zip
				// may be, so this only stops a corrupt or hostile cache entry.
				var n int64
				n, err = io.Copy(w, io.LimitReader(rc, modzip.MaxZipFile+1))
				rc.Close()
				if err == nil && n > modzip.MaxZipFile {
					err = fmt.Errorf("%s: %s is larger than a module zip may be", m.zip, zf.Name)
				}
			}
		}
		if err != nil {
			return err
		}
	}
	if !wrote {
		w, err := zw.Create(prefix + "go.mod")
		if err != nil {
			return err
		}
		_, err = w.Write(goMod)
		return err
	}
	return nil
}

// ---- Versions ----

// syntheticVersions makes n releases between start and end, oldest first:
// mostly v0/v1 minor and patch releases, with an occasional pre-release.
func syntheticVersions(r *rand.Rand, n int, start, end time.Time) []Version {
	major, minor, patch := 0, 1, 0
	if r.IntN(3) == 0 {
		major = 1
	}
	out := make([]Version, 0, n)
	span := end.Sub(start)
	for i := range n {
		v := fmt.Sprintf("v%d.%d.%d", major, minor, patch)
		if i < n-1 && r.IntN(12) == 0 {
			v += fmt.Sprintf("-rc.%d", 1+r.IntN(3))
		}
		t := start.Add(time.Duration(float64(span) * float64(i+1) / float64(n+1)))
		out = append(out, Version{v, t})
		switch k := r.IntN(10); {
		case k < 6:
			patch++
		case k < 9:
			minor, patch = minor+1, 0
		default:
			if major == 0 {
				major, minor, patch = 1, 0, 0
			} else {
				minor, patch = minor+1, 0
			}
		}
	}
	// Pre-releases sort before their release; keep release times in
	// version order.
	times := make([]time.Time, len(out))
	for i, v := range out {
		times[i] = v.Time
	}
	slices.SortStableFunc(out, func(a, b Version) int { return semver.Compare(a.V, b.V) })
	out = slices.CompactFunc(out, func(a, b Version) bool { return a.V == b.V })
	for i := range out {
		out[i].Time = times[i]
	}
	return out
}
