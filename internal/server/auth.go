package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
)

const (
	sessionCookie = "gopherdex_session"
	maxFormBytes  = 16 << 10
)

type userCacheKey struct{}

// userCache holds the signed-in user for one request, so the session is
// looked up at most once.
type userCache struct {
	loaded bool
	user   *accounts.User
}

func withUserCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCacheKey{}, &userCache{})))
	})
}

// currentUser returns the signed-in user, or nil.
func (s *server) currentUser(r *http.Request) *accounts.User {
	cache, _ := r.Context().Value(userCacheKey{}).(*userCache)
	if cache != nil && cache.loaded {
		return cache.user
	}
	var u *accounts.User
	if c, err := r.Cookie(sessionCookie); err == nil {
		u, err = s.accounts.SessionUser(r.Context(), c.Value)
		if err != nil && !errors.Is(err, accounts.ErrInvalidToken) {
			s.log.Error("load session", "err", err)
		}
	}
	if cache != nil {
		cache.loaded, cache.user = true, u
	}
	return u
}

// requireUser redirects to the sign-in page when nobody is signed in.
func (s *server) requireUser(w http.ResponseWriter, r *http.Request) *accounts.User {
	u := s.currentUser(r)
	if u == nil {
		http.Redirect(w, r, "/login?"+url.Values{"next": {r.URL.RequestURI()}}.Encode(), http.StatusSeeOther)
	}
	return u
}

func (s *server) setSession(w http.ResponseWriter, secret string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    secret,
		Path:     "/",
		MaxAge:   int(accounts.SessionTTL / time.Second),
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode})
}

// clientOf identifies the caller for rate limits and the audit log. Behind
// a trusted reverse proxy the real client is the last X-Forwarded-For
// entry, the one the proxy itself appended; earlier entries are
// client-controlled.
func (s *server) clientOf(r *http.Request) accounts.Client {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if s.trustProxy {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			parts := strings.Split(xff[len(xff)-1], ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(last) != nil {
				ip = last
			}
		}
	}
	return accounts.Client{IP: ip, UserAgent: r.UserAgent()}
}

// safeNext allows only same-site relative redirect targets.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/account"
	}
	return next
}

func parseForm(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "The form couldn't be read.", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *server) tooManyRequests(w http.ResponseWriter, r *http.Request, what string) {
	w.Header().Set("Retry-After", "300")
	s.render(w, r, http.StatusTooManyRequests, "message", "Slow down", messageData{
		Kicker:      "Too many attempts",
		Heading:     "Please wait a few minutes.",
		Body:        "There have been too many " + what + " attempts from your network. Try again later.",
		ActionURL:   "/",
		ActionLabel: "Back to Gopherdex",
	})
}

// ---- Sign up ----

type signupData struct {
	Host            string
	Username, Email string
	Errors          map[string]string
}

func (s *server) handleSignupForm(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) != nil {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "signup", "Create an account", signupData{Host: s.moduleHost})
}

func (s *server) handleSignup(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	c := s.clientOf(r)
	if !s.signupLimit.Allow(c.IP) {
		s.tooManyRequests(w, r, "sign-up")
		return
	}
	data := signupData{
		Host:     s.moduleHost,
		Username: accounts.NormalizeUsername(r.PostFormValue("username")),
		Email:    strings.TrimSpace(r.PostFormValue("email")),
	}
	u, err := s.accounts.Register(r.Context(), data.Username, data.Email, r.PostFormValue("password"), c)
	var fe *accounts.FieldError
	switch {
	case errors.As(err, &fe):
		data.Errors = map[string]string{fe.Field: fe.Message}
		s.render(w, r, http.StatusUnprocessableEntity, "signup", "Create an account", data)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	secret, err := s.accounts.StartSession(r.Context(), u, c)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.setSession(w, secret)
	http.Redirect(w, r, "/account?done=signed-up", http.StatusSeeOther)
}

// ---- Sign in / out ----

type loginData struct {
	Login, Next, Error string
}

func (s *server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if s.currentUser(r) != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", "Sign in", loginData{Next: next})
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	c := s.clientOf(r)
	if !s.loginLimit.Allow(c.IP) {
		s.tooManyRequests(w, r, "sign-in")
		return
	}
	data := loginData{Login: strings.TrimSpace(r.PostFormValue("login")), Next: safeNext(r.PostFormValue("next"))}
	res, err := s.accounts.Login(r.Context(), data.Login, r.PostFormValue("password"), c)
	switch {
	case errors.Is(err, accounts.ErrInvalidCredentials):
		data.Error = "That username, email or password isn't right. Check them and try again."
		s.render(w, r, http.StatusUnauthorized, "login", "Sign in", data)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	if res.Challenge != "" {
		s.setChallenge(w, res.Challenge)
		http.Redirect(w, r, "/login/2fa?"+url.Values{"next": {data.Next}}.Encode(), http.StatusSeeOther)
		return
	}
	s.setSession(w, res.Session)
	http.Redirect(w, r, data.Next, http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.accounts.Logout(r.Context(), c.Value); err != nil {
			s.log.Error("logout", "err", err)
		}
	}
	s.clearSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---- Email verification ----

type messageData struct {
	Kicker, Heading, Body, ActionURL, ActionLabel string
}

func (s *server) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	u, err := s.accounts.VerifyEmail(r.Context(), r.URL.Query().Get("token"), s.clientOf(r))
	switch {
	case errors.Is(err, accounts.ErrInvalidToken):
		s.render(w, r, http.StatusBadRequest, "message", "Link expired", messageData{
			Kicker:      "Email verification",
			Heading:     "This link is invalid or has expired.",
			Body:        "Verification links work once and expire after 24 hours. Sign in and send yourself a new one from your account page.",
			ActionURL:   "/account",
			ActionLabel: "Go to your account",
		})
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	if cur := s.currentUser(r); cur != nil && cur.ID == u.ID {
		http.Redirect(w, r, "/account?done=email-verified", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "message", "Email verified", messageData{
		Kicker:      "Email verification",
		Heading:     "Your email is verified.",
		Body:        "Sign in as @" + u.Username + " to create API tokens and publish modules.",
		ActionURL:   "/login",
		ActionLabel: "Sign in",
	})
}

func (s *server) handleResendVerification(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if !s.emailLimit.Allow("user:" + strconv.FormatInt(u.ID, 10)) {
		s.tooManyRequests(w, r, "email")
		return
	}
	if err := s.accounts.SendVerification(r.Context(), u); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account?done=verification-sent", http.StatusSeeOther)
}

// ---- Account and API tokens ----

type accountData struct {
	Host        string
	SiteURL     string
	User        *accounts.User
	Tokens      []accounts.Token
	Modules     []registry.ModuleSummary
	Memberships []registry.Membership
	OrgName     string
	NewToken    *newToken
	TokenName   string
	Notice      string
	Error       string
	Errors      map[string]string

	// Trusted publishers for modules not published yet.
	Pending           []registry.Publisher
	Publisher         publisherForm
	TrustedPublishing bool
}

// publisherForm keeps what was typed into the add-publisher form after an
// error.
type publisherForm struct {
	Module, Repository, Workflow, Environment string
}

type newToken struct {
	Name, Secret string
}

// notices maps ?done= keys to fixed messages, so the query string can't
// inject arbitrary text into the page.
var notices = map[string]string{
	"signed-up":         "Welcome to Gopherdex! We've emailed you a link to confirm your address.",
	"email-verified":    "Your email is verified. You can create API tokens now.",
	"verification-sent": "We've sent a new verification link. It expires in 24 hours.",
	"token-revoked":     "Token revoked. Anything using it can no longer publish.",
	"publisher-added":   "Trusted publisher added. Its workflow can publish the module's first release with gopherdex publish, no API token needed.",
	"publisher-removed": "Trusted publisher removed.",
}

func (s *server) handleAccount(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	s.renderAccount(w, r, http.StatusOK, accountData{Notice: notices[r.URL.Query().Get("done")]})
}

func (s *server) renderAccount(w http.ResponseWriter, r *http.Request, status int, data accountData) {
	u := s.currentUser(r)
	tokens, err := s.accounts.Tokens(r.Context(), u.ID)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	modules, err := s.registry.Managed(r.Context(), u)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if data.Memberships, err = s.registry.Memberships(r.Context(), u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if data.Pending, err = s.registry.PendingPublishers(r.Context(), u); err != nil {
		s.serverError(w, r, err)
		return
	}
	data.Host, data.SiteURL, data.User, data.Tokens, data.Modules = s.moduleHost, s.siteURL, u, tokens, modules
	data.TrustedPublishing = s.githubOIDC != nil
	s.render(w, r, status, "account", "Your account", data)
}

var tokenExpiry = map[string]time.Duration{
	"30":  30 * 24 * time.Hour,
	"90":  90 * 24 * time.Hour,
	"365": 365 * 24 * time.Hour,
	"0":   0,
}

func (s *server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	ttl, ok := tokenExpiry[r.PostFormValue("expires")]
	if !ok {
		http.Error(w, "Choose an expiry from the list.", http.StatusBadRequest)
		return
	}
	name := r.PostFormValue("name")
	scope := ""
	if m := r.PostFormValue("module"); m != "" {
		// A token for one module; the user must maintain it.
		if role, err := s.registry.Role(r.Context(), u, m); err != nil || role < registry.RoleMaintainer {
			s.renderAccount(w, r, http.StatusUnprocessableEntity, accountData{TokenName: name, Errors: map[string]string{"name": "Choose one of your modules."}})
			return
		}
		scope = accounts.ScopeModulePrefix + m
	}
	secret, tok, err := s.accounts.CreateToken(r.Context(), u, name, ttl, scope, s.clientOf(r))
	var fe *accounts.FieldError
	switch {
	case errors.As(err, &fe):
		s.renderAccount(w, r, http.StatusUnprocessableEntity, accountData{TokenName: name, Errors: map[string]string{fe.Field: fe.Message}})
		return
	case errors.Is(err, accounts.ErrEmailNotVerified):
		s.renderAccount(w, r, http.StatusForbidden, accountData{Error: "Verify your email address before creating API tokens."})
		return
	case errors.Is(err, accounts.ErrTooManyTokens):
		s.renderAccount(w, r, http.StatusUnprocessableEntity, accountData{Error: "You have 20 active tokens, the maximum. Revoke one you no longer use first."})
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	// The secret is rendered once, never redirected through a URL.
	s.renderAccount(w, r, http.StatusCreated, accountData{NewToken: &newToken{Name: tok.Name, Secret: secret}})
}

func (s *server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch err := s.accounts.RevokeToken(r.Context(), u.ID, id, s.clientOf(r)); {
	case errors.Is(err, accounts.ErrNotFound):
		http.NotFound(w, r)
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, "/account?done=token-revoked", http.StatusSeeOther)
	}
}

// bearerUser authenticates an API request (Authorization: Bearer gdx_…) and
// writes a 401 when it fails.
func (s *server) bearerUser(w http.ResponseWriter, r *http.Request) (*accounts.User, *accounts.Token, bool) {
	secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="gopherdex"`)
		writeJSON(w, http.StatusUnauthorized, apiError{"Send an API token in the Authorization header: Bearer gdx_…"})
		return nil, nil, false
	}
	u, tok, err := s.accounts.AuthenticateToken(r.Context(), strings.TrimSpace(secret))
	switch {
	case errors.Is(err, accounts.ErrInvalidToken):
		w.Header().Set("WWW-Authenticate", `Bearer realm="gopherdex", error="invalid_token"`)
		writeJSON(w, http.StatusUnauthorized, apiError{"This API token is invalid, expired or revoked. Create a new one at " + s.siteURL + "/account."})
		return nil, nil, false
	case err != nil:
		s.apiError(w, r, err)
		return nil, nil, false
	}
	return u, tok, true
}

// handleWhoami lets the CLI check a token.
func (s *server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	u, tok, ok := s.bearerUser(w, r)
	if !ok {
		return
	}
	type tokenInfo struct {
		Name      string     `json:"name"`
		Scope     string     `json:"scope"`
		ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	}
	namespaces := []string{s.moduleHost + "/" + u.Username}
	memberships, err := s.registry.Memberships(r.Context(), u.ID)
	if err != nil {
		s.apiError(w, r, err)
		return
	}
	for _, m := range memberships {
		namespaces = append(namespaces, s.moduleHost+"/"+m.Org)
	}
	writeJSON(w, http.StatusOK, struct {
		Username      string    `json:"username"`
		Namespace     string    `json:"namespace"`
		Namespaces    []string  `json:"namespaces"` // yours and your organizations'
		EmailVerified bool      `json:"emailVerified"`
		Token         tokenInfo `json:"token"`
	}{u.Username, s.moduleHost + "/" + u.Username, namespaces, u.EmailVerified, tokenInfo{tok.Name, tok.Scope, tok.ExpiresAt}})
}
