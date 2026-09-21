package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Trusted publishing: inside GitHub Actions, with no API token configured,
// publish asks GitHub for an OIDC ID token naming this workflow run and
// trades it at the registry for a short-lived token for one module.

// inGitHubActions reports whether this job can request an ID token.
func inGitHubActions(env Env) bool {
	return env.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL") != "" && env.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN") != ""
}

// errNoIDTokenPermission explains the usual reason a GitHub Actions job
// can't request an ID token.
var errNoIDTokenPermission = errors.New("no API token, and this GitHub Actions job can't request an ID token for trusted publishing. " +
	"Add this to the workflow:\n\n  permissions:\n    id-token: write\n\nor set GOPHERDEX_TOKEN from a repository secret")

type mintResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	Publisher string    `json:"publisher"`
}

// trustedPublishToken returns a registry token for modPath minted from this
// GitHub Actions run's identity.
func trustedPublishToken(ctx context.Context, env Env, registry, modPath string) (*mintResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	var aud struct {
		Audience string `json:"audience"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registry+"/api/oidc/audience", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent())
	if err := doJSON(env, req, &aud); err != nil {
		return nil, fmt.Errorf("trusted publishing: %w", err)
	}

	idToken, err := githubIDToken(ctx, env, aud.Audience)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(map[string]string{"token": idToken, "module": modPath})
	if err != nil {
		return nil, err
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, registry+"/api/oidc/mint-token", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent())
	var minted mintResponse
	if err := doJSON(env, req, &minted); err != nil {
		return nil, fmt.Errorf("trusted publishing: %w", err)
	}
	return &minted, nil
}

// githubIDToken asks the Actions runtime for an ID token with audience.
func githubIDToken(ctx context.Context, env Env, audience string) (string, error) {
	u, err := url.Parse(env.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"))
	if err != nil || u.Scheme != "https" && !isLoopback(u.Hostname()) {
		return "", errors.New("trusted publishing: ACTIONS_ID_TOKEN_REQUEST_URL isn't a valid https URL")
	}
	q := u.Query()
	q.Set("audience", audience)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "bearer "+env.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent())
	client := env.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("trusted publishing: request an ID token from GitHub: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("trusted publishing: GitHub refused an ID token (%s). Check the workflow has permissions: id-token: write", resp.Status)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil || out.Value == "" {
		return "", errors.New("trusted publishing: GitHub sent an unexpected ID token response")
	}
	return out.Value, nil
}
