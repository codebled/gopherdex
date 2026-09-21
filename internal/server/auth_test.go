package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// browser is an HTTP client that keeps cookies and does not follow
// redirects, so tests can assert on them.
type browser struct {
	t      *testing.T
	base   string
	client *http.Client
}

func newBrowser(t *testing.T, base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: base, client: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (b *browser) do(method, path string, form url.Values, header http.Header) (*http.Response, string) {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, b.base+path, body)
	if err != nil {
		b.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	// Same-origin form posts from a browser carry this header.
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func (b *browser) get(path string) (*http.Response, string) {
	return b.do(http.MethodGet, path, nil, nil)
}
func (b *browser) post(path string, form url.Values) (*http.Response, string) {
	return b.do(http.MethodPost, path, form, nil)
}

func expect(t *testing.T, resp *http.Response, body string, status int, contains string) {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d\n%s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, status, body)
	}
	if contains != "" && !strings.Contains(body, contains) {
		t.Fatalf("%s %s: body does not contain %q\n%s", resp.Request.Method, resp.Request.URL.Path, contains, body)
	}
}

func TestAccountFlow(t *testing.T) {
	srv, mails := newTestServerWithMail(t)
	b := newBrowser(t, srv.URL)

	resp, body := b.get("/account")
	expect(t, resp, body, http.StatusSeeOther, "")
	if loc := resp.Header.Get("Location"); loc != "/login?next=%2Faccount" {
		t.Fatalf("signed-out /account redirects to %q", loc)
	}

	resp, body = b.get("/signup")
	expect(t, resp, body, http.StatusOK, "gopherdex.test/")

	resp, body = b.post("/signup", url.Values{"username": {"API"}, "email": {"a@example.com"}, "password": {"correct horse battery"}})
	expect(t, resp, body, http.StatusUnprocessableEntity, "That name is reserved")

	resp, body = b.post("/signup", url.Values{"username": {"alice"}, "email": {"alice@example.com"}, "password": {"correct horse battery"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	if resp.Header.Get("Location") != "/account?done=signed-up" {
		t.Fatalf("sign-up redirect = %q", resp.Header.Get("Location"))
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && (!c.HttpOnly || c.SameSite != http.SameSiteLaxMode) {
			t.Fatalf("session cookie attributes: %+v", c)
		}
	}

	resp, body = b.get("/account?done=signed-up")
	expect(t, resp, body, http.StatusOK, "Not verified")
	if !strings.Contains(body, "@alice") || !strings.Contains(body, "gopherdex.test/alice/") || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("account page is missing user details or no-store")
	}

	// Tokens are refused until the email is verified.
	resp, body = b.post("/account/tokens", url.Values{"name": {"laptop"}, "expires": {"90"}})
	expect(t, resp, body, http.StatusForbidden, "Verify your email")

	link := regexp.MustCompile(`http://gopherdex\.test(/verify-email\?token=\S+)`).FindStringSubmatch(mails.last(t).Body)
	if link == nil {
		t.Fatalf("verification email has no link:\n%s", mails.last(t).Body)
	}
	resp, body = b.get(link[1])
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = b.get("/account?done=email-verified")
	expect(t, resp, body, http.StatusOK, "Your email is verified")

	resp, body = b.post("/account/tokens", url.Values{"name": {"laptop"}, "expires": {"90"}})
	expect(t, resp, body, http.StatusCreated, "Copy it now")
	secret := regexp.MustCompile(`gdx_[A-Za-z0-9_-]{43}`).FindString(body)
	if secret == "" {
		t.Fatal("new token not shown")
	}

	// The CLI's view of the token.
	api := newBrowser(t, srv.URL)
	resp, body = api.do(http.MethodGet, "/api/whoami", nil, http.Header{"Authorization": {"Bearer " + secret}})
	expect(t, resp, body, http.StatusOK, `"username":"alice"`)
	var who struct {
		Namespace string
		Token     struct{ Scope string }
	}
	json.Unmarshal([]byte(body), &who)
	if who.Namespace != "gopherdex.test/alice" || who.Token.Scope != "namespace:alice" {
		t.Fatalf("whoami = %s", body)
	}
	resp, body = api.do(http.MethodGet, "/api/whoami", nil, http.Header{"Authorization": {"Bearer gdx_wrong"}})
	expect(t, resp, body, http.StatusUnauthorized, "invalid")

	// Revoke it from the account page.
	resp, body = b.get("/account")
	id := regexp.MustCompile(`/account/tokens/(\d+)/revoke`).FindStringSubmatch(body)
	if id == nil {
		t.Fatal("token row not listed")
	}
	if strings.Contains(body, secret) {
		t.Fatal("the full token secret must only be shown once")
	}
	resp, body = b.post("/account/tokens/"+id[1]+"/revoke", url.Values{})
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = api.do(http.MethodGet, "/api/whoami", nil, http.Header{"Authorization": {"Bearer " + secret}})
	expect(t, resp, body, http.StatusUnauthorized, "")

	// Sign out, then back in with a safe redirect.
	resp, body = b.post("/logout", url.Values{})
	expect(t, resp, body, http.StatusSeeOther, "")
	resp, body = b.get("/")
	expect(t, resp, body, http.StatusOK, "Register")

	resp, body = b.post("/login", url.Values{"login": {"alice"}, "password": {"wrong password"}, "next": {"/account"}})
	expect(t, resp, body, http.StatusUnauthorized, "That username, email or password")
	resp, body = b.post("/login", url.Values{"login": {"alice@example.com"}, "password": {"correct horse battery"}, "next": {"//evil.example"}})
	expect(t, resp, body, http.StatusSeeOther, "")
	if loc := resp.Header.Get("Location"); loc != "/account" {
		t.Fatalf("open redirect: Location %q", loc)
	}
}

func TestCrossSiteFormPostRejected(t *testing.T) {
	srv := newTestServer(t)
	b := newBrowser(t, srv.URL)
	resp, body := b.do(http.MethodPost, "/signup",
		url.Values{"username": {"mallory"}, "email": {"m@example.com"}, "password": {"correct horse battery"}},
		http.Header{"Sec-Fetch-Site": {"cross-site"}})
	expect(t, resp, body, http.StatusForbidden, "Cross-site")
}

func TestLoginRateLimit(t *testing.T) {
	srv := newTestServer(t)
	b := newBrowser(t, srv.URL)
	var resp *http.Response
	var body string
	for range 11 {
		resp, body = b.post("/login", url.Values{"login": {"nobody"}, "password": {"guess guess guess"}})
	}
	expect(t, resp, body, http.StatusTooManyRequests, "too many sign-in attempts")
}
