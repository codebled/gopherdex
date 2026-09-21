// Package oidc verifies the OpenID Connect ID tokens that CI systems issue
// to their jobs, so a registry can trust "this upload comes from workflow W
// in repository R" without anyone storing a long-lived API token in CI.
//
// Only what trusted publishing needs is implemented: RS256 JWTs, keys from
// the issuer's discovery document, and the standard time and audience
// checks. It uses the standard library alone.
package oidc

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// GitHubIssuer is the issuer of GitHub Actions ID tokens.
const GitHubIssuer = "https://token.actions.githubusercontent.com"

const (
	maxTokenBytes = 16 << 10
	leeway        = time.Minute
	keysTTL       = time.Hour
	// refetchEvery limits refreshes forced by unknown key IDs, so garbage
	// tokens can't make us hammer the issuer.
	refetchEvery = 5 * time.Minute
)

// ErrInvalid reports a token that is malformed, badly signed, expired, or
// meant for someone else. Its message is safe to show the caller.
var ErrInvalid = errors.New("invalid ID token")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Verifier checks ID tokens from one issuer for one audience.
type Verifier struct {
	Issuer   string
	Audience string
	HTTP     *http.Client
	Now      func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *Verifier) client() *http.Client {
	if v.HTTP != nil {
		return v.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Verify checks raw's signature, issuer, audience and lifetime, then
// decodes its claims into dst.
func (v *Verifier) Verify(ctx context.Context, raw string, dst any) error {
	if len(raw) > maxTokenBytes {
		return invalid("token is too large")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return invalid("not a JWT")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return invalid("bad header")
	}
	if header.Alg != "RS256" {
		return invalid("unsupported algorithm %q", header.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return invalid("bad signature encoding")
	}
	key, err := v.key(ctx, header.Kid)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
		return invalid("signature doesn't verify")
	}

	var std struct {
		Iss string   `json:"iss"`
		Aud audience `json:"aud"`
		Exp *int64   `json:"exp"`
		Nbf *int64   `json:"nbf"`
		Iat *int64   `json:"iat"`
	}
	if err := decodeSegment(parts[1], &std); err != nil {
		return invalid("bad claims")
	}
	now := v.now()
	switch {
	case std.Iss != v.Issuer:
		return invalid("issued by %q, not %q", std.Iss, v.Issuer)
	case !slices.Contains(std.Aud, v.Audience):
		return invalid("audience is %q; request the token with audience %q", strings.Join(std.Aud, ", "), v.Audience)
	case std.Exp == nil || now.After(time.Unix(*std.Exp, 0).Add(leeway)):
		return invalid("token has expired")
	case std.Nbf != nil && now.Add(leeway).Before(time.Unix(*std.Nbf, 0)):
		return invalid("token isn't valid yet")
	case std.Iat != nil && now.Add(leeway).Before(time.Unix(*std.Iat, 0)):
		return invalid("token was issued in the future")
	}
	if err := decodeSegment(parts[1], dst); err != nil {
		return invalid("bad claims")
	}
	return nil
}

// audience accepts the aud claim as a string or a list of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

func decodeSegment(seg string, dst any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// key returns the signing key kid, fetching the issuer's key set when it
// is stale or doesn't have kid yet.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if k, ok := v.keys[kid]; ok && now.Sub(v.fetchedAt) < keysTTL {
		return k, nil
	}
	if v.keys == nil || now.Sub(v.fetchedAt) >= refetchEvery {
		keys, err := v.fetchKeys(ctx)
		if err != nil {
			if k, ok := v.keys[kid]; ok { // the issuer is down; keep using what we had
				return k, nil
			}
			return nil, err
		}
		v.keys, v.fetchedAt = keys, now
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, invalid("signed with unknown key %q", kid)
}

func (v *Verifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	var disco struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := v.getJSON(ctx, strings.TrimSuffix(v.Issuer, "/")+"/.well-known/openid-configuration", &disco); err != nil {
		return nil, err
	}
	if disco.Issuer != v.Issuer || !strings.HasPrefix(disco.JWKSURI, "https://") && !strings.HasPrefix(v.Issuer, "http://") {
		return nil, fmt.Errorf("oidc: discovery document for %s is inconsistent", v.Issuer)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := v.getJSON(ctx, disco.JWKSURI, &set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("oidc: %s publishes no usable RSA keys", v.Issuer)
	}
	return keys, nil
}

func (v *Verifier) getJSON(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.client().Do(req)
	if err != nil {
		return fmt.Errorf("oidc: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc: fetch %s: %s", url, resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst); err != nil {
		return fmt.Errorf("oidc: decode %s: %w", url, err)
	}
	return nil
}

// GitHubClaims are the GitHub Actions claims trusted publishing uses. See
// https://docs.github.com/actions/security-for-github-actions/security-hardening-your-deployments/about-security-hardening-with-openid-connect
type GitHubClaims struct {
	JTI               string `json:"jti"`
	Exp               int64  `json:"exp"`
	Repository        string `json:"repository"`          // "octo-org/octo-repo"
	RepositoryID      string `json:"repository_id"`       // stable across renames
	RepositoryOwner   string `json:"repository_owner"`    // "octo-org"
	RepositoryOwnerID string `json:"repository_owner_id"` // stable across renames
	WorkflowRef       string `json:"workflow_ref"`        // "octo-org/octo-repo/.github/workflows/release.yml@refs/tags/v1.0.0"
	Environment       string `json:"environment"`
	Ref               string `json:"ref"`
	SHA               string `json:"sha"`
	RunID             string `json:"run_id"`
	RunAttempt        string `json:"run_attempt"`
	EventName         string `json:"event_name"`
}

// Workflow returns the workflow file name from workflow_ref, e.g.
// "release.yml", or "" if the claim isn't a workflow in the repository.
func (c *GitHubClaims) Workflow() string {
	path, _, ok := strings.Cut(c.WorkflowRef, "@")
	if !ok {
		return ""
	}
	name, ok := strings.CutPrefix(path, c.Repository+"/.github/workflows/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return ""
	}
	return name
}
