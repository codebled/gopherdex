package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
)

// Passkey ceremonies are two JSON requests from the page's script: one
// for the options the browser passes to navigator.credentials, and one
// with the browser's answer. The ceremony secret travels in a short-lived
// cookie between them.

const ceremonyCookie = "gopherdex_webauthn"

func (s *server) setCeremony(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{Name: ceremonyCookie, Value: token, Path: "/", MaxAge: 300,
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteStrictMode})
}

func (s *server) takeCeremony(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(ceremonyCookie)
	http.SetCookie(w, &http.Cookie{Name: ceremonyCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteStrictMode})
	if err != nil {
		return ""
	}
	return c.Value
}

// passkeyError answers a failed ceremony with a message for the page.
func (s *server) passkeyError(w http.ResponseWriter, r *http.Request, err error) {
	var fe *accounts.FieldError
	switch {
	case errors.Is(err, accounts.ErrPasskey), errors.Is(err, accounts.ErrInvalidToken):
		writeJSON(w, http.StatusBadRequest, apiError{"That passkey didn't work. Try again, or use another sign-in method."})
	case errors.Is(err, accounts.ErrEmailNotVerified):
		writeJSON(w, http.StatusForbidden, apiError{"Verify your email address before adding a passkey."})
	case errors.As(err, &fe):
		writeJSON(w, http.StatusUnprocessableEntity, apiError{fe.Message})
	default:
		s.apiError(w, r, err)
	}
}

// signedInJSON returns the signed-in user, or answers 401 for scripts.
func (s *server) signedInJSON(w http.ResponseWriter, r *http.Request) *accounts.User {
	u := s.currentUser(r)
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, apiError{"Sign in again to manage passkeys."})
	}
	return u
}

func limitedBody(r *http.Request) io.Reader { return io.LimitReader(r.Body, 64<<10) }

// ---- Adding a passkey (security page) ----

func (s *server) handlePasskeyRegisterOptions(w http.ResponseWriter, r *http.Request) {
	u := s.signedInJSON(w, r)
	if u == nil {
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	json.NewDecoder(limitedBody(r)).Decode(&body)
	if msg := s.checkPassword(r, u, body.Password); msg != "" {
		writeJSON(w, http.StatusUnprocessableEntity, apiError{msg})
		return
	}
	options, token, err := s.accounts.BeginPasskeyRegistration(r.Context(), u)
	if err != nil {
		s.passkeyError(w, r, err)
		return
	}
	s.setCeremony(w, token)
	writeJSON(w, http.StatusOK, options)
}

func (s *server) handlePasskeyRegister(w http.ResponseWriter, r *http.Request) {
	u := s.signedInJSON(w, r)
	if u == nil {
		return
	}
	pk, codes, err := s.accounts.FinishPasskeyRegistration(r.Context(), u, s.takeCeremony(w, r), r.URL.Query().Get("name"), limitedBody(r), s.clientOf(r))
	if err != nil {
		s.passkeyError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		Name          string   `json:"name"`
		RecoveryCodes []string `json:"recoveryCodes,omitempty"`
	}{pk.Name, codes})
}

func (s *server) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	err := s.accounts.DeletePasskey(r.Context(), u, id, r.PostFormValue("password"), s.clientOf(r))
	switch {
	case errors.Is(err, accounts.ErrWrongPassword):
		s.renderSecurity(w, r, http.StatusUnprocessableEntity, securityData{Errors: map[string]string{"passkey": "That password isn't right, so the passkey wasn't removed."}})
	case errors.Is(err, accounts.ErrNotFound):
		s.notFound(w, r)
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, "/account/security?done=passkey-removed", http.StatusSeeOther)
	}
}

// ---- Signing in with a passkey ----

func (s *server) handlePasskeyLoginOptions(w http.ResponseWriter, r *http.Request) {
	options, token, err := s.accounts.BeginPasskeyLogin(r.Context())
	if err != nil {
		s.passkeyError(w, r, err)
		return
	}
	s.setCeremony(w, token)
	writeJSON(w, http.StatusOK, options)
}

func (s *server) handlePasskeyLogin(w http.ResponseWriter, r *http.Request) {
	if !s.loginLimit.Allow(ipKey(s.clientOf(r).IP)) {
		writeJSON(w, http.StatusTooManyRequests, apiError{"Too many sign-in attempts. Wait a few minutes and try again."})
		return
	}
	secret, _, err := s.accounts.FinishPasskeyLogin(r.Context(), s.takeCeremony(w, r), limitedBody(r), s.clientOf(r))
	if err != nil {
		s.passkeyError(w, r, err)
		return
	}
	s.setSession(w, secret)
	writeJSON(w, http.StatusOK, struct {
		Redirect string `json:"redirect"`
	}{safeNext(r.URL.Query().Get("next"))})
}

// ---- A passkey as the second step after a password ----

func (s *server) handlePasskeySecondFactorOptions(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(challengeCookie)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{"Your sign-in expired. Start again."})
		return
	}
	options, token, err := s.accounts.BeginPasskeySecondFactor(r.Context(), c.Value)
	if err != nil {
		s.passkeyError(w, r, err)
		return
	}
	s.setCeremony(w, token)
	writeJSON(w, http.StatusOK, options)
}

func (s *server) handlePasskeySecondFactor(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(challengeCookie)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{"Your sign-in expired. Start again."})
		return
	}
	secret, _, err := s.accounts.FinishPasskeySecondFactor(r.Context(), c.Value, s.takeCeremony(w, r), limitedBody(r), s.clientOf(r))
	if err != nil {
		s.passkeyError(w, r, err)
		return
	}
	s.clearChallenge(w)
	s.setSession(w, secret)
	writeJSON(w, http.StatusOK, struct {
		Redirect string `json:"redirect"`
	}{safeNext(r.URL.Query().Get("next"))})
}
