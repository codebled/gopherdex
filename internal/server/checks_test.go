package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	xmodule "golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

// uploadFiles uploads a version with the given files through the API.
func uploadFiles(t *testing.T, env *testEnv, token, modPath, version string, files map[string]string) (int, string) {
	t.Helper()
	src := t.TempDir()
	for name, body := range files {
		p := filepath.Join(src, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	var zipBuf bytes.Buffer
	if err := modzip.CreateFromDir(&zipBuf, xmodule.Version{Path: modPath, Version: version}, src); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("module", modPath)
	mw.WriteField("version", version)
	part, _ := mw.CreateFormFile("zip", "m.zip")
	part.Write(zipBuf.Bytes())
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, env.srv.URL+"/api/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := env.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPublishChecksOverHTTP(t *testing.T) {
	env := newTestEnv(t) // alice is an admin
	mod := "gopherdex.test/alice/retry"
	env.publish(t, mod, "v1.0.0")
	alice := newBrowser(t, env.srv.URL)
	alice.post("/login", url.Values{"login": {"alice"}, "password": {"correct horse battery"}})
	_, body := alice.post("/account/tokens", url.Values{"name": {"ci"}, "expires": {"30"}, "password": {"correct horse battery"}})
	token := strings.Split(strings.Split(body, `data-copy="gdx_`)[1], `"`)[0]
	token = "gdx_" + token

	status, body := uploadFiles(t, env, token, mod, "v1.1.0", map[string]string{
		"go.mod": "module " + mod + "\n", "lib.go": "package lib\n", "tool.exe": "MZ\x90\x00\x03",
	})
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "blocked_by_checks") || !strings.Contains(body, "tool.exe") {
		t.Fatalf("executable upload: %d %s", status, body)
	}

	status, body = uploadFiles(t, env, token, mod, "v1.1.0", map[string]string{
		"go.mod": "module " + mod + "\n",
		"lib.go": "package lib\n\nimport \"net/http\"\n\nvar _, _ = http.Get(\"https://example.com/\")\n",
	})
	if status != http.StatusCreated {
		t.Fatalf("flagged upload: %d %s", status, body)
	}
	var pub struct {
		Warnings []struct{ Rule, File, Message string }
	}
	json.Unmarshal([]byte(body), &pub)
	if len(pub.Warnings) != 1 || pub.Warnings[0].Rule != "runs-on-import" || pub.Warnings[0].File != "lib.go" {
		t.Errorf("warnings = %+v", pub.Warnings)
	}
	if msg := env.mails.find(t, "alice@example.com", "Publish checks flagged "+mod+" v1.1.0"); !strings.Contains(msg, "net/http.Get") {
		t.Errorf("maintainer email:\n%s", msg)
	}
	env.mails.find(t, "alice@example.com", "Review queue: "+mod+" v1.1.0")

	resp, body := alice.get("/alice/retry?tab=manage")
	expect(t, resp, body, http.StatusOK, "Publish checks")
	expect(t, resp, body, http.StatusOK, `href="/alice/retry@v1.1.0?tab=source&amp;file=lib.go#L5"`)
}
