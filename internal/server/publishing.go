package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/codebled/gopherdex/internal/accounts"
	"github.com/codebled/gopherdex/internal/module"
	"github.com/codebled/gopherdex/internal/oidc"
	"github.com/codebled/gopherdex/internal/registry"
)

// publisherTokenTTL is how long a token minted for a trusted publisher
// lasts: long enough to build and upload a zip, short enough that a leaked
// CI log is harmless soon after.
const publisherTokenTTL = 15 * time.Minute

// handleOIDCAudience tells CI which audience to request its ID token for.
func (s *server) handleOIDCAudience(w http.ResponseWriter, r *http.Request) {
	if s.githubOIDC == nil {
		writeJSON(w, http.StatusNotFound, apiError{"Trusted publishing is turned off on this registry."})
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Audience string `json:"audience"`
	}{s.githubOIDC.Audience})
}

// handleMintToken trades a GitHub Actions ID token for a short-lived API
// token that can publish one module:
// POST /api/oidc/mint-token {"token": "<JWT>", "module": "<path>"}
func (s *server) handleMintToken(w http.ResponseWriter, r *http.Request) {
	if s.githubOIDC == nil {
		writeJSON(w, http.StatusNotFound, apiError{"Trusted publishing is turned off on this registry."})
		return
	}
	var req struct {
		Token  string `json:"token"`
		Module string `json:"module"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<10)).Decode(&req); err != nil || req.Token == "" || req.Module == "" {
		writeJSON(w, http.StatusBadRequest, apiError{`Send JSON: {"token": "<GitHub Actions ID token>", "module": "<module path>"}.`})
		return
	}
	if module.CheckPath(req.Module) != nil {
		writeJSON(w, http.StatusBadRequest, apiError{"Give a valid module path."})
		return
	}
	var claims oidc.GitHubClaims
	if err := s.githubOIDC.Verify(r.Context(), req.Token, &claims); err != nil {
		if errors.Is(err, oidc.ErrInvalid) {
			writeJSON(w, http.StatusUnauthorized, apiError{"GitHub's ID token was refused: " + err.Error() + "."})
			return
		}
		s.log.Error("verify GitHub ID token", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, apiError{"The registry couldn't fetch GitHub's signing keys. Try again in a minute."})
		return
	}
	if !s.mintLimit.Allow("repo:" + claims.Repository) {
		w.Header().Set("Retry-After", "600")
		writeJSON(w, http.StatusTooManyRequests, apiError{"This repository has requested a lot of tokens in the last hour. Try again in a few minutes."})
		return
	}

	pub, provenance, err := s.registry.ExchangeGitHubToken(r.Context(), req.Module, &claims)
	if err != nil {
		s.writeRegistryError(w, r, err)
		return
	}
	// The token acts for whoever set the publisher up, so it can do no
	// more than they can today.
	u, err := s.accounts.UserByID(r.Context(), pub.CreatedByID)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	if err := s.registry.CanPublish(r.Context(), u, req.Module); err != nil {
		var re *registry.Error
		if errors.As(err, &re) {
			writeJSON(w, http.StatusForbidden, struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}{fmt.Sprintf("This trusted publisher was added by @%s, who can no longer publish %s: %s An owner should remove it and add it again.",
				u.Username, req.Module, re.Message), "publisher_owner_lost_access"})
			return
		}
		s.apiError(w, r, err)
		return
	}
	if s.registry.Require2FA && !u.TwoFactor {
		writeJSON(w, http.StatusForbidden, apiError{fmt.Sprintf(
			"This registry requires two-factor authentication to publish, and @%s, who added this trusted publisher, hasn't turned it on.", u.Username)})
		return
	}

	name := fmt.Sprintf("GitHub Actions: %s (%s)", pub.Repository, pub.Workflow)
	secret, tok, err := s.accounts.MintPublisherToken(r.Context(), u, name, accounts.ScopeModulePrefix+req.Module,
		publisherTokenTTL, pub.ID, provenance, s.clientOf(r))
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	s.log.Info("trusted publisher token minted", "module", req.Module, "repository", claims.Repository,
		"workflow", pub.Workflow, "run", claims.RunID, "token", tok.ID)
	writeJSON(w, http.StatusOK, struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
		Module    string    `json:"module"`
		Publisher string    `json:"publisher"`
	}{secret, tok.ExpiresAt.UTC(), req.Module, name})
}

func (s *server) writeRegistryError(w http.ResponseWriter, r *http.Request, err error) {
	var re *registry.Error
	if errors.As(err, &re) {
		writeJSON(w, re.Status, struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}{re.Message, re.Code})
		return
	}
	s.apiError(w, r, err)
}

// ---- Managing trusted publishers ----

// publisherAction runs a form action from a module's Manage tab, or, for a
// module that isn't published yet, from the account page.
func (s *server) publisherAction(w http.ResponseWriter, r *http.Request, done string, act func(u *accounts.User) (*registry.Publisher, error)) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	fromAccount := r.PostFormValue("from") == "account"
	modPath := r.PostFormValue("module")
	p, err := act(u)
	var re *registry.Error
	switch {
	case err == nil:
		s.notifyPublisherChange(u, p, done)
		if fromAccount || p.Pending {
			http.Redirect(w, r, "/account?done="+done+"#publishing-title", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, s.project.URL(p.ModulePath, "")+"?tab=manage&done="+done+"#publishers-title", http.StatusSeeOther)
	case errors.Is(err, module.ErrNotFound):
		s.notFound(w, r)
	case errors.As(err, &re) && fromAccount:
		s.renderAccount(w, r, re.Status, accountData{Error: re.Message, Publisher: publisherForm{
			Module: modPath, Repository: r.PostFormValue("repository"), Workflow: r.PostFormValue("workflow"), Environment: r.PostFormValue("environment"),
		}})
	case errors.As(err, &re) && module.CheckPath(modPath) == nil:
		s.renderProject(w, r, modPath, "", "manage", re.Status, re.Message)
	case errors.As(err, &re):
		http.Error(w, re.Message, re.Status)
	default:
		s.serverError(w, r, err)
	}
}

func (s *server) handleAddPublisher(w http.ResponseWriter, r *http.Request) {
	s.publisherAction(w, r, "publisher-added", func(u *accounts.User) (*registry.Publisher, error) {
		modPath := r.PostFormValue("module")
		if _, err := s.registry.CheckModulePath(modPath); err != nil {
			return nil, err
		}
		return s.registry.AddPublisher(r.Context(), u, modPath, r.PostFormValue("repository"), r.PostFormValue("workflow"),
			r.PostFormValue("environment"), s.clientOf(r))
	})
}

func (s *server) handleRemovePublisher(w http.ResponseWriter, r *http.Request) {
	s.publisherAction(w, r, "publisher-removed", func(u *accounts.User) (*registry.Publisher, error) {
		id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
		if err != nil {
			return nil, module.ErrNotFound
		}
		return s.registry.RemovePublisher(r.Context(), u, id, s.clientOf(r))
	})
}

// notifyPublisherChange emails everyone responsible for the module, since
// a new publisher can release code without any of their credentials.
func (s *server) notifyPublisherChange(u *accounts.User, p *registry.Publisher, done string) {
	what := "added"
	detail := "It can now publish new versions without an API token."
	if done == "publisher-removed" {
		what, detail = "removed", "It can no longer publish."
	}
	env := ""
	if p.Environment != "" {
		env = fmt.Sprintf(", environment %q", p.Environment)
	}
	subject := fmt.Sprintf("Trusted publisher %s for %s", what, p.ModulePath)
	body := fmt.Sprintf("@%s %s a trusted publisher for %s: the GitHub Actions workflow %s in %s%s. %s",
		u.Username, what, p.ModulePath, p.Workflow, p.RepositoryURL(), env, detail)
	if p.Pending {
		s.accounts.Notify(u, subject, body)
		return
	}
	s.emailMaintainers(p.ModulePath, subject, body+"\n\nReview it on the module's Manage tab: "+s.siteURL+s.project.URL(p.ModulePath, "")+"?tab=manage")
}
