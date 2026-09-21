package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/version"
)

type whoamiResponse struct {
	Username      string   `json:"username"`
	Namespace     string   `json:"namespace"`
	Namespaces    []string `json:"namespaces"`
	EmailVerified bool     `json:"emailVerified"`
	Token         struct {
		Name      string     `json:"name"`
		Scope     string     `json:"scope"`
		ExpiresAt *time.Time `json:"expiresAt"`
	} `json:"token"`
}

type publishResponse struct {
	Module           string    `json:"module"`
	Version          string    `json:"version"`
	H1               string    `json:"h1"`
	GoModH1          string    `json:"goModH1"`
	Size             int64     `json:"size"`
	PublishedAt      time.Time `json:"publishedAt"`
	AlreadyPublished bool      `json:"alreadyPublished"`
	URL              string    `json:"url"`
	ProxyURL         string    `json:"proxyURL"`
	Install          string    `json:"install"`
	Warnings         []struct {
		File    string `json:"file"`
		Line    int    `json:"line"`
		Message string `json:"message"`
	} `json:"warnings"`
}

// apiError is an error response from the registry.
type apiError struct {
	Status  int
	Message string `json:"error"`
	Code    string `json:"code"`
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the registry answered %d %s", e.Status, http.StatusText(e.Status))
	}
	return e.Message
}

func userAgent() string { return "gopherdex-cli/" + version.String() }

func callWhoami(ctx context.Context, env Env, registry, token string) (*whoamiResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registry+"/api/whoami", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgent())
	var who whoamiResponse
	if err := doJSON(env, req, &who); err != nil {
		return nil, err
	}
	return &who, nil
}

type uploadFields struct {
	Module, Version, Repository, Commit, Ref string
	ZipFile                                  string
}

// callUpload streams the zip to the registry as multipart/form-data without
// buffering it in memory.
func callUpload(ctx context.Context, env Env, registry, token string, f uploadFields) (*publishResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	zipFile, err := os.Open(f.ZipFile)
	if err != nil {
		return nil, err
	}
	defer zipFile.Close()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			for _, kv := range [][2]string{
				{"module", f.Module}, {"version", f.Version},
				{"repository", f.Repository}, {"commit", f.Commit}, {"ref", f.Ref},
			} {
				if kv[1] == "" {
					continue
				}
				if err := mw.WriteField(kv[0], kv[1]); err != nil {
					return err
				}
			}
			part, err := mw.CreateFormFile("zip", f.Module[strings.LastIndex(f.Module, "/")+1:]+"@"+f.Version+".zip")
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, zipFile); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registry+"/api/upload", pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgent())
	var resp publishResponse
	if err := doJSON(env, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func doJSON(env Env, req *http.Request, out any) error {
	client := env.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach the registry: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read the registry's response: %w", err)
	}
	if resp.StatusCode >= 300 {
		e := &apiError{Status: resp.StatusCode}
		if json.Unmarshal(body, e) != nil || e.Message == "" {
			e.Message = ""
		}
		return e
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("the registry sent an unexpected response (is %s a Gopherdex registry?)", req.URL.Host)
	}
	return nil
}

type yankResponse struct {
	Module  string `json:"module"`
	Version string `json:"version"`
	Yanked  bool   `json:"yanked"`
	URL     string `json:"url"`
}

func callYank(ctx context.Context, env Env, registry, token, modPath, version, reason string, yank bool) (*yankResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, err := json.Marshal(map[string]any{"module": modPath, "version": version, "reason": reason, "yank": yank})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registry+"/api/yank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", userAgent())
	var resp yankResponse
	if err := doJSON(env, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
