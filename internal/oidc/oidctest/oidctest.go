// Package oidctest runs a fake OIDC issuer that signs tokens like GitHub
// Actions does, for tests.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Issuer is a running fake issuer.
type Issuer struct {
	URL string
	Kid string
	key *rsa.PrivateKey
}

// New starts an issuer that serves its discovery document and key set.
func New(t testing.TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss := &Issuer{Kid: "test-key", key: key}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	iss.URL = srv.URL
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": iss.URL, "jwks_uri": iss.URL + "/.well-known/jwks"})
	})
	mux.HandleFunc("GET /.well-known/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": iss.Kid,
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	return iss
}

// Sign returns an RS256 JWT with claims.
func (iss *Issuer) Sign(t testing.TB, claims map[string]any) string {
	t.Helper()
	return iss.SignWith(t, iss.key, iss.Kid, claims)
}

// SignWith signs with any key, to test forged tokens.
func (iss *Issuer) SignWith(t testing.TB, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signed := enc(map[string]string{"alg": "RS256", "typ": "JWT", "kid": kid}) + "." + enc(claims)
	digest := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// GitHubClaims returns the claims GitHub Actions would issue for a run of
// workflow in repo ("owner/name"), with the given audience and times.
func (iss *Issuer) GitHubClaims(repo, workflow, audience string, iat int64) map[string]any {
	owner := repo
	for i := range repo {
		if repo[i] == '/' {
			owner = repo[:i]
			break
		}
	}
	return map[string]any{
		"iss": iss.URL, "aud": audience, "iat": iat, "nbf": iat, "exp": iat + 300,
		"jti":                 rand.Text(),
		"repository":          repo,
		"repository_id":       "1001",
		"repository_owner":    owner,
		"repository_owner_id": "2002",
		"workflow_ref":        repo + "/.github/workflows/" + workflow + "@refs/tags/v1.0.0",
		"ref":                 "refs/tags/v1.0.0",
		"sha":                 "0123456789abcdef0123456789abcdef01234567",
		"run_id":              "42",
		"run_attempt":         "1",
		"event_name":          "push",
	}
}
