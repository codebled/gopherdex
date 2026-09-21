package oidc_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/oidc"
	"github.com/parthiban-sivakumar/gopherdex/internal/oidc/oidctest"
)

func TestVerify(t *testing.T) {
	iss := oidctest.New(t)
	now := time.Unix(1_800_000_000, 0)
	v := &oidc.Verifier{Issuer: iss.URL, Audience: "gopherdex.test", Now: func() time.Time { return now }}
	ctx := context.Background()

	good := iss.GitHubClaims("octo-org/retry", "release.yml", "gopherdex.test", now.Unix()-10)
	var c oidc.GitHubClaims
	if err := v.Verify(ctx, iss.Sign(t, good), &c); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if c.Repository != "octo-org/retry" || c.Workflow() != "release.yml" || c.RepositoryOwnerID != "2002" || c.JTI == "" {
		t.Errorf("claims = %+v, workflow %q", c, c.Workflow())
	}

	// A list audience works too.
	listAud := iss.GitHubClaims("octo-org/retry", "release.yml", "", now.Unix())
	listAud["aud"] = []string{"other", "gopherdex.test"}
	if err := v.Verify(ctx, iss.Sign(t, listAud), &c); err != nil {
		t.Errorf("list audience: %v", err)
	}

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	with := func(k string, val any) map[string]any {
		m := iss.GitHubClaims("octo-org/retry", "release.yml", "gopherdex.test", now.Unix())
		m[k] = val
		return m
	}
	bad := map[string]string{
		"wrong audience":   iss.Sign(t, with("aud", "pypi")),
		"wrong issuer":     iss.Sign(t, with("iss", "https://evil.example")),
		"expired":          iss.Sign(t, with("exp", now.Unix()-3600)),
		"no expiry":        iss.Sign(t, with("exp", nil)),
		"not yet valid":    iss.Sign(t, with("nbf", now.Unix()+3600)),
		"forged signature": iss.SignWith(t, otherKey, iss.Kid, good),
		"unknown key":      iss.SignWith(t, otherKey, "other-kid", good),
		"tampered claims":  tamper(iss.Sign(t, good)),
		"alg none":         "eyJhbGciOiJub25lIn0." + strings.Split(iss.Sign(t, good), ".")[1] + ".",
		"garbage":          "not-a-jwt",
	}
	for name, tok := range bad {
		if err := v.Verify(ctx, tok, &c); !errors.Is(err, oidc.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

// tamper swaps the claims of a signed token for different ones.
func tamper(tok string) string {
	parts := strings.Split(tok, ".")
	// {"repository":"evil/repo"} base64url
	parts[1] = "eyJyZXBvc2l0b3J5IjoiZXZpbC9yZXBvIn0"
	return strings.Join(parts, ".")
}

func TestWorkflow(t *testing.T) {
	for ref, want := range map[string]string{
		"o/r/.github/workflows/release.yml@refs/tags/v1":     "release.yml",
		"o/r/.github/workflows/release.yml":                  "", // no ref
		"x/y/.github/workflows/release.yml@refs/heads/main":  "", // another repository's workflow
		"o/r/.github/workflows/sub/release.yml@refs/heads/m": "",
		"o/r/.github/workflows/@refs/heads/main":             "",
	} {
		c := oidc.GitHubClaims{Repository: "o/r", WorkflowRef: ref}
		if got := c.Workflow(); got != want {
			t.Errorf("Workflow(%q) = %q, want %q", ref, got, want)
		}
	}
}
