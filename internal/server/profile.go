package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
)

// handleChangeEmail starts an email change; the address changes once the
// link sent to the new one is opened.
func (s *server) handleChangeEmail(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	if !s.emailLimit.Allow("user:" + u.Username) {
		s.renderAccount(w, r, http.StatusTooManyRequests, accountData{Errors: map[string]string{"new_email": "Too many emails sent. Try again in an hour."}})
		return
	}
	newEmail := r.PostFormValue("new_email")
	err := s.accounts.RequestEmailChange(r.Context(), u, newEmail, r.PostFormValue("current_password"), s.clientOf(r))
	var fe *accounts.FieldError
	switch {
	case errors.As(err, &fe):
		field := fe.Field
		if field == "email" {
			field = "new_email"
		}
		s.renderAccount(w, r, http.StatusUnprocessableEntity, accountData{NewEmail: newEmail, Errors: map[string]string{field: fe.Message}})
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, "/account?done=email-change-sent#email-title", http.StatusSeeOther)
	}
}

func (s *server) handlePreferences(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	p := accounts.Preferences{Publish: r.PostFormValue("publish") == "on", Access: r.PostFormValue("access") == "on"}
	if err := s.accounts.SetPreferences(r.Context(), u, p, s.clientOf(r)); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account?done=preferences#email-title", http.StatusSeeOther)
}

// handleDeleteAccount deletes the signed-in account after the password,
// a 2FA code when it's on, and the username typed as confirmation.
func (s *server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	if !s.loginLimit.Allow("delete:" + u.Username) {
		s.renderSecurity(w, r, http.StatusTooManyRequests, securityData{Errors: map[string]string{"delete": "Too many attempts. Try again in a few minutes."}})
		return
	}
	confirm := strings.TrimPrefix(strings.TrimSpace(r.PostFormValue("confirm")), "@")
	if !strings.EqualFold(confirm, u.Username) {
		s.renderSecurity(w, r, http.StatusUnprocessableEntity, securityData{Errors: map[string]string{"confirm": "Type your username, " + u.Username + ", to confirm."}})
		return
	}
	err := s.accounts.DeleteAccount(r.Context(), u, r.PostFormValue("delete_password"), r.PostFormValue("delete_code"), s.clientOf(r))
	var lastOwner *accounts.ErrLastOrgOwner
	switch {
	case errors.Is(err, accounts.ErrWrongPassword):
		s.renderSecurity(w, r, http.StatusUnprocessableEntity, securityData{Errors: map[string]string{"delete_password": "That password isn't right."}})
	case errors.Is(err, accounts.ErrBadSecondFactor):
		s.renderSecurity(w, r, http.StatusUnprocessableEntity, securityData{Errors: map[string]string{"delete_code": "That code isn't right."}})
	case errors.As(err, &lastOwner):
		s.renderSecurity(w, r, http.StatusConflict, securityData{Errors: map[string]string{"delete": "You're the only owner of " +
			strings.Join(lastOwner.Orgs, ", ") + ". Make another member an owner first, so the organization isn't left without one."}})
	case err != nil:
		s.serverError(w, r, err)
	default:
		s.clearSession(w)
		s.render(w, r, http.StatusOK, "message", "Account deleted", messageData{
			Kicker:      "Account deleted",
			Heading:     "Your account is gone.",
			Body:        "We've deleted @" + u.Username + ", its sessions, API tokens and trusted publishers. Modules you published stay available, because programs depend on them, and the name @" + u.Username + " can't be registered again.",
			ActionURL:   "/",
			ActionLabel: "Back to Gopherdex",
		})
	}
}
