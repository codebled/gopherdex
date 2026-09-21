package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Successful uploads and publish validation are covered end to end by the
// CLI tests (internal/cli) and the registry tests.
func TestUploadRequestErrors(t *testing.T) {
	srv := newTestServer(t)

	post := func(header http.Header) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/upload", strings.NewReader(""))
		for k, v := range header {
			req.Header[k] = v
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}

	resp, body := post(http.Header{"Content-Type": {"multipart/form-data; boundary=x"}})
	expect(t, resp, body, http.StatusUnauthorized, "Authorization header")

	resp, body = post(http.Header{"Content-Type": {"application/zip"}, "Authorization": {"Bearer gdx_invalid"}})
	expect(t, resp, body, http.StatusUnauthorized, "invalid, expired or revoked")

	resp, body = post(http.Header{"Sec-Fetch-Site": {"cross-site"}, "Authorization": {"Bearer gdx_invalid"}})
	expect(t, resp, body, http.StatusForbidden, "Cross-site")
}
