package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// PkgsiteIndex searches public modules through pkg.go.dev's JSON search
// endpoint (GET /v1/search?q=…). If that endpoint changes or is unreachable,
// run the server with -offline to search hosted modules only.
type PkgsiteIndex struct {
	Base string // e.g. "https://pkg.go.dev"
	HTTP *http.Client
}

// Search returns up to limit modules matching query, one result per module.
func (p *PkgsiteIndex) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	u := strings.TrimSuffix(p.Base, "/") + "/v1/search?" + url.Values{"q": {query}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build search request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search pkg.go.dev: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search pkg.go.dev: unexpected status %s", resp.Status)
	}
	var body struct {
		Items []struct {
			PackagePath string `json:"packagePath"`
			ModulePath  string `json:"modulePath"`
			Version     string `json:"version"`
			Synopsis    string `json:"synopsis"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode pkg.go.dev search results: %w", err)
	}

	results := []Result{}
	index := map[string]int{}
	for _, it := range body.Items {
		if it.ModulePath == "" {
			continue
		}
		if i, ok := index[it.ModulePath]; ok {
			// Prefer the synopsis of the module's root package.
			if it.PackagePath == it.ModulePath {
				results[i].Synopsis = it.Synopsis
			}
			continue
		}
		if len(results) == limit {
			continue
		}
		index[it.ModulePath] = len(results)
		results = append(results, Result{Path: it.ModulePath, Version: it.Version, Synopsis: it.Synopsis, Origin: Public})
	}
	return results, nil
}
