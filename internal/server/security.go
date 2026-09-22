package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"rsc.io/qr"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/mail"
	"github.com/parthiban-sivakumar/gopherdex/internal/module"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
)

const challengeCookie = "gopherdex_2fa"

func (s *server) setChallenge(w http.ResponseWriter, secret string) {
	http.SetCookie(w, &http.Cookie{Name: challengeCookie, Value: secret, Path: "/login/2fa", MaxAge: 300, //nolint:gosec // Secure is off only for plain-HTTP local development
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode})
}

func (s *server) clearChallenge(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: challengeCookie, Value: "", Path: "/login/2fa", MaxAge: -1, //nolint:gosec // Secure is off only for plain-HTTP local development
		HttpOnly: true, Secure: s.secureCookies, SameSite: http.SameSiteLaxMode})
}

func sessionSecret(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}

// ---- Forgot and reset password ----

type forgotData struct {
	Email string
	Sent  bool
}

func (s *server) handleForgotForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "forgot", "Reset your password", forgotData{})
}

func (s *server) handleForgot(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	c := s.clientOf(r)
	if !s.resetLimit.Allow(ipKey(c.IP)) {
		s.tooManyRequests(w, r, "password reset")
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	if err := s.accounts.RequestPasswordReset(r.Context(), email, c); err != nil {
		s.log.Error("password reset request", "err", err)
	}
	// Same answer whether or not the account exists.
	s.render(w, r, http.StatusOK, "forgot", "Reset your password", forgotData{Email: email, Sent: true})
}

type resetData struct {
	Token string
	Error string
}

func (s *server) handleResetForm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if err := s.accounts.CheckResetToken(r.Context(), token); err != nil {
		s.render(w, r, http.StatusBadRequest, "message", "Link expired", messageData{
			Kicker: "Password reset", Heading: "This reset link is invalid or has expired.",
			Body:      "Reset links work once and expire after an hour. Ask for a new one.",
			ActionURL: "/forgot-password", ActionLabel: "Send a new link",
		})
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer") // keep the token out of Referer headers
	s.render(w, r, http.StatusOK, "reset", "Choose a new password", resetData{Token: token})
}

func (s *server) handleReset(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	token := r.PostFormValue("token")
	_, err := s.accounts.ResetPassword(r.Context(), token, r.PostFormValue("password"), s.clientOf(r))
	var fe *accounts.FieldError
	switch {
	case errors.As(err, &fe):
		s.render(w, r, http.StatusUnprocessableEntity, "reset", "Choose a new password", resetData{Token: token, Error: fe.Message})
		return
	case errors.Is(err, accounts.ErrInvalidToken):
		http.Redirect(w, r, "/reset-password?token=", http.StatusSeeOther)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.clearSession(w)
	s.render(w, r, http.StatusOK, "message", "Password changed", messageData{
		Kicker: "Password reset", Heading: "Your password is changed.",
		Body:      "You've been signed out everywhere, and any pending email change was canceled. Sign in with your new password.",
		ActionURL: "/login", ActionLabel: "Sign in",
	})
}

// ---- Second sign-in step ----

type twoFactorData struct {
	Next, Error string
	App         bool // the account has an authenticator app
	Passkeys    bool // the account has passkeys
}

// twoFactorMethods fills in which second factors the pending sign-in has.
func (s *server) twoFactorMethods(r *http.Request, d *twoFactorData) {
	if c, err := r.Cookie(challengeCookie); err == nil {
		d.App, d.Passkeys, _ = s.accounts.ChallengeMethods(r.Context(), c.Value)
	}
}

func (s *server) handleTwoFactorForm(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie(challengeCookie); err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	data := twoFactorData{Next: safeNext(r.URL.Query().Get("next"))}
	s.twoFactorMethods(r, &data)
	s.render(w, r, http.StatusOK, "login_2fa", "Two-factor authentication", data)
}

func (s *server) handleTwoFactor(w http.ResponseWriter, r *http.Request) {
	if !parseForm(w, r) {
		return
	}
	c := s.clientOf(r)
	if !s.loginLimit.Allow(ipKey(c.IP)) {
		s.tooManyRequests(w, r, "sign-in")
		return
	}
	data := twoFactorData{Next: safeNext(r.PostFormValue("next"))}
	s.twoFactorMethods(r, &data)
	cookie, err := r.Cookie(challengeCookie)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	session, _, err := s.accounts.CompleteLogin(r.Context(), cookie.Value, r.PostFormValue("code"), c)
	switch {
	case errors.Is(err, accounts.ErrBadSecondFactor):
		data.Error = "That code isn't right. Use the current code from your authenticator app, or one of your recovery codes."
		s.render(w, r, http.StatusUnauthorized, "login_2fa", "Two-factor authentication", data)
		return
	case errors.Is(err, accounts.ErrInvalidToken), errors.Is(err, accounts.ErrTooManyAttempts):
		s.clearChallenge(w)
		http.Redirect(w, r, "/login?"+url.Values{"next": {data.Next}}.Encode(), http.StatusSeeOther)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.clearChallenge(w)
	s.setSession(w, session)
	http.Redirect(w, r, data.Next, http.StatusSeeOther)
}

// ---- Account security page ----

type securityData struct {
	User          *accounts.User
	Setup         *accounts.TOTPSetup
	QR            template.URL
	RecoveryCodes []string
	CodesLeft     int
	Sessions      int
	Events        []accounts.Event
	Notice, Error string
	Errors        map[string]string
	Require2FA    bool
	Passkeys      []accounts.Passkey
	AppEnabled    bool // an authenticator app is set up
}

var securityNotices = map[string]string{ //nolint:gosec // notice texts that mention passwords, not credentials
	"password":        "Password changed. Other sessions were signed out.",
	"sessions":        "Signed out of every other session.",
	"2fa-off":         "Two-factor authentication is off.",
	"passkey-added":   "Passkey added. You can sign in with it now, and it counts as two-factor authentication.",
	"passkey-removed": "Passkey removed.",
}

func (s *server) handleSecurity(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	s.renderSecurity(w, r, http.StatusOK, securityData{Notice: securityNotices[r.URL.Query().Get("done")]})
}

func (s *server) renderSecurity(w http.ResponseWriter, r *http.Request, status int, data securityData) {
	ctx := r.Context()
	u := s.currentUser(r)
	data.User, data.Require2FA = u, s.registry.Require2FA
	var err error
	if data.AppEnabled, err = s.accounts.AppEnabled(ctx, u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if data.Passkeys, err = s.accounts.Passkeys(ctx, u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if u.TwoFactor {
		if data.CodesLeft, err = s.accounts.RecoveryCodesLeft(ctx, u.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if !data.AppEnabled && data.RecoveryCodes == nil {
		if data.Setup, err = s.accounts.BeginTOTP(ctx, u); err != nil {
			s.serverError(w, r, err)
			return
		}
		code, err := qr.Encode(data.Setup.URI, qr.M)
		if err != nil {
			s.serverError(w, r, fmt.Errorf("render 2FA QR code: %w", err))
			return
		}
		code.Scale = 6
		//nolint:gosec // a base64 PNG data URL we encode ourselves, not user input
		data.QR = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG()))
	}
	if data.Sessions, err = s.accounts.SessionCount(ctx, u.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if data.Events, err = s.accounts.SecurityEvents(ctx, u.ID, 30); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, status, "security", "Account security", data)
}

func (s *server) handleConfirmTOTP(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	if msg := s.confirmPassword(r, u); msg != "" {
		s.renderSecurity(w, r, http.StatusUnprocessableEntity, securityData{Errors: map[string]string{"code": msg}})
		return
	}
	codes, err := s.accounts.ConfirmTOTP(r.Context(), u, r.PostFormValue("code"), s.clientOf(r))
	switch {
	case errors.Is(err, accounts.ErrBadSecondFactor):
		s.renderSecurity(w, r, http.StatusUnprocessableEntity, securityData{Errors: map[string]string{"code": "That code doesn't match. Check your phone's clock and use the newest code."}})
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	u.TwoFactor = true
	s.renderSecurity(w, r, http.StatusOK, securityData{RecoveryCodes: codes})
}

func (s *server) handleDisableTOTP(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	if !s.loginLimit.Allow(sensitiveKey(u)) {
		s.renderSecurity(w, r, http.StatusTooManyRequests, securityData{Errors: map[string]string{"disable": tooManyTries}})
		return
	}
	switch err := s.accounts.DisableTOTP(r.Context(), u, r.PostFormValue("code"), s.clientOf(r)); {
	case errors.Is(err, accounts.ErrBadSecondFactor):
		s.renderSecurity(w, r, http.StatusUnprocessableEntity, securityData{Errors: map[string]string{"disable": "That code isn't right."}})
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, "/account/security?done=2fa-off", http.StatusSeeOther)
	}
}

func (s *server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	if !s.loginLimit.Allow(sensitiveKey(u)) {
		s.renderSecurity(w, r, http.StatusTooManyRequests, securityData{Errors: map[string]string{"current_password": tooManyTries}})
		return
	}
	err := s.accounts.ChangePassword(r.Context(), u, r.PostFormValue("current_password"), r.PostFormValue("new_password"), sessionSecret(r), s.clientOf(r))
	var fe *accounts.FieldError
	switch {
	case errors.As(err, &fe):
		field := fe.Field
		if field == "password" {
			field = "new_password"
		}
		s.renderSecurity(w, r, http.StatusUnprocessableEntity, securityData{Errors: map[string]string{field: fe.Message}})
	case err != nil:
		s.serverError(w, r, err)
	default:
		http.Redirect(w, r, "/account/security?done=password", http.StatusSeeOther)
	}
}

func (s *server) handleSignOutOthers(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	if _, err := s.accounts.SignOutOthers(r.Context(), u, sessionSecret(r), s.clientOf(r)); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account/security?done=sessions", http.StatusSeeOther)
}

// ---- Reports ----

type reportData struct {
	Module     string
	URL        string
	Categories []string
	Category   string
	Details    string
	Error      string
	Sent       bool
}

func (s *server) handleReportForm(w http.ResponseWriter, r *http.Request) {
	if s.requireUser(w, r) == nil {
		return
	}
	modPath := r.URL.Query().Get("module")
	if module.CheckPath(modPath) != nil {
		s.notFound(w, r)
		return
	}
	s.render(w, r, http.StatusOK, "report", "Report "+modPath, reportData{Module: modPath, URL: s.project.URL(modPath, ""), Categories: registry.ReportCategories})
}

func (s *server) handleReport(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	data := reportData{
		Module: r.PostFormValue("module"), Category: r.PostFormValue("category"), Details: r.PostFormValue("details"),
		Categories: registry.ReportCategories,
	}
	data.URL = s.project.URL(data.Module, "")
	if !s.reportLimit.Allow("user:" + strconv.FormatInt(u.ID, 10)) {
		s.tooManyRequests(w, r, "report")
		return
	}
	err := s.registry.ReportModule(r.Context(), u, data.Module, data.Category, data.Details, s.clientOf(r))
	var re *registry.Error
	switch {
	case errors.As(err, &re):
		data.Error = re.Message
		s.render(w, r, re.Status, "report", "Report "+data.Module, data)
		return
	case errors.Is(err, module.ErrNotFound):
		s.notFound(w, r)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	data.Sent = true
	s.render(w, r, http.StatusOK, "report", "Report sent", data)
}

// ---- Admin review ----

func (s *server) isAdmin(u *accounts.User) bool {
	return u != nil && s.admins[u.Username]
}

// requireAdmin allows configured admins who have two-factor authentication
// on. Everyone else gets a 404, so the page's existence isn't advertised.
func (s *server) requireAdmin(w http.ResponseWriter, r *http.Request) *accounts.User {
	u := s.requireUser(w, r)
	if u == nil {
		return nil
	}
	if !s.isAdmin(u) {
		s.notFound(w, r)
		return nil
	}
	if !u.TwoFactor {
		s.render(w, r, http.StatusForbidden, "message", "Two-factor required", messageData{
			Kicker: "Admin", Heading: "Turn on two-factor authentication first.",
			Body:      "Admin tools can hide anyone's modules, so they need two-factor authentication on your account.",
			ActionURL: "/account/security", ActionLabel: "Set it up",
		})
		return nil
	}
	return u
}

type adminData struct {
	Reports     []registry.Report
	Quarantined []registry.QuarantinedModule
	Notice      string
	Error       string
}

var adminNotices = map[string]string{
	"dismissed":   "Report dismissed.",
	"quarantined": "Module quarantined. Its owners have been emailed.",
	"released":    "Module released. It's available again.",
}

func (s *server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if s.requireAdmin(w, r) == nil {
		return
	}
	s.renderAdmin(w, r, http.StatusOK, adminNotices[r.URL.Query().Get("done")], "")
}

func (s *server) renderAdmin(w http.ResponseWriter, r *http.Request, status int, notice, errMsg string) {
	reports, err := s.registry.OpenReports(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	quarantined, err := s.registry.QuarantinedModules(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, status, "admin", "Review queue", adminData{Reports: reports, Quarantined: quarantined, Notice: notice, Error: errMsg})
}

func (s *server) adminAction(w http.ResponseWriter, r *http.Request, done string, act func(admin *accounts.User) error) {
	admin := s.requireAdmin(w, r)
	if admin == nil || !parseForm(w, r) {
		return
	}
	err := act(admin)
	var re *registry.Error
	switch {
	case err == nil:
		http.Redirect(w, r, "/admin?done="+done, http.StatusSeeOther)
	case errors.As(err, &re):
		s.renderAdmin(w, r, re.Status, "", re.Message)
	case errors.Is(err, module.ErrNotFound):
		s.renderAdmin(w, r, http.StatusNotFound, "", "There's no such module.")
	default:
		s.serverError(w, r, err)
	}
}

func (s *server) handleDismissReport(w http.ResponseWriter, r *http.Request) {
	s.adminAction(w, r, "dismissed", func(admin *accounts.User) error {
		id, _ := strconv.ParseInt(r.PostFormValue("report"), 10, 64)
		return s.registry.DismissReport(r.Context(), admin, id, r.PostFormValue("note"), s.clientOf(r))
	})
}

func (s *server) handleQuarantine(w http.ResponseWriter, r *http.Request) {
	s.adminAction(w, r, "quarantined", func(admin *accounts.User) error {
		modPath, reason := r.PostFormValue("module"), strings.TrimSpace(r.PostFormValue("reason"))
		if err := s.registry.Quarantine(r.Context(), admin, modPath, reason, s.clientOf(r)); err != nil {
			return err
		}
		s.emailMaintainers(modPath, "Your module "+modPath+" is under review",
			fmt.Sprintf("The administrators of %s have temporarily hidden %s while they review a report about it.\n\nReason: %s\n\nThe module's files are kept. Reply to this email if you think this is a mistake.", s.siteURL, modPath, reason))
		return nil
	})
}

func (s *server) handleRelease(w http.ResponseWriter, r *http.Request) {
	s.adminAction(w, r, "released", func(admin *accounts.User) error {
		return s.registry.Release(r.Context(), admin, r.PostFormValue("module"), s.clientOf(r))
	})
}

// ---- Notifications ----

// emailMaintainers sends a notice to everyone who maintains modPath, in the
// background.
func (s *server) emailMaintainers(modPath, subject, body string) {
	s.emailMaintainersWho(modPath, "", subject, body)
}

// emailMaintainersWho emails the maintainers who want emails of kind
// (accounts.EmailPublish…); an empty kind means everyone, for security
// notices nobody can turn off.
func (s *server) emailMaintainersWho(modPath, kind, subject, body string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		names, err := s.registry.Maintainers(ctx, modPath)
		if err != nil {
			s.log.Error("notify maintainers", "module", modPath, "err", err)
			return
		}
		var emails map[string]string
		if kind == "" {
			emails, err = s.accounts.UserEmails(ctx, names)
		} else {
			emails, err = s.accounts.EmailsFor(ctx, names, kind)
		}
		if err != nil {
			s.log.Error("notify maintainers", "module", modPath, "err", err)
			return
		}
		for name, email := range emails {
			msg := mail.Message{To: email, Subject: subject, Body: "Hi " + name + ",\n\n" + body + "\n"}
			if err := s.accounts.Mailer.Send(ctx, msg); err != nil {
				s.log.Error("notify maintainer", "module", modPath, "user", name, "err", err)
			}
		}
	}()
}

// emailUser emails one user, if they want emails of kind.
func (s *server) emailUser(username, kind, subject, body string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		emails, err := s.accounts.EmailsFor(ctx, []string{username}, kind)
		if err != nil {
			s.log.Error("email user", "user", username, "err", err)
			return
		}
		for name, email := range emails {
			msg := mail.Message{To: email, Subject: subject, Body: "Hi " + name + ",\n\n" + body + "\n\nChoose which emails you get at " + s.siteURL + "/account.\n"}
			if err := s.accounts.Mailer.Send(ctx, msg); err != nil {
				s.log.Error("email user", "user", name, "err", err)
			}
		}
	}()
}

// article is "an" before owner and "a" before maintainer and member.
func article(role string) string {
	if role == "owner" {
		return "an"
	}
	return "a"
}

// notifyPublished tells every maintainer about a new version, so a leaked
// token is noticed quickly.
func (s *server) notifyPublished(modPath, version string, u *accounts.User, tok *accounts.Token, ip string) {
	if p := registry.ParseProvenance(tok.Claims); tok.PublisherID != 0 && p != nil {
		s.emailMaintainersWho(modPath, accounts.EmailPublish, fmt.Sprintf("%s %s was published", modPath, version),
			fmt.Sprintf("The GitHub Actions workflow %s in %s published %s@%s through a trusted publisher added by @%s.\n\nRun: %s\n\n%s%s\n\nIf you didn't expect this, remove the trusted publisher and yank the version on the module's Manage tab.",
				p.Workflow, p.RepositoryURL(), modPath, version, u.Username, p.RunURL(), s.siteURL, s.project.URL(modPath, version)))
		return
	}
	s.emailMaintainersWho(modPath, accounts.EmailPublish, fmt.Sprintf("%s %s was published", modPath, version),
		fmt.Sprintf("@%s published %s@%s using the API token %q (from %s).\n\n%s%s\n\nIf you didn't expect this, revoke the token at %s/account and yank the version from the module's Manage tab.",
			u.Username, modPath, version, tok.Name, ip, s.siteURL, s.project.URL(modPath, version), s.siteURL))
}

// Sensitive changes need the current password, so a stolen session cookie
// alone can't plant lasting access (an API token, an authenticator app, a
// passkey). Guesses are limited per account, whichever address they're from.

const tooManyTries = "Too many attempts. Wait a few minutes and try again."

func sensitiveKey(u *accounts.User) string { return "sensitive:" + strconv.FormatInt(u.ID, 10) }

// confirmPassword checks the form's "password" field, returning a message
// for the form when it isn't right.
func (s *server) confirmPassword(r *http.Request, u *accounts.User) string {
	return s.checkPassword(r, u, r.PostFormValue("password"))
}

func (s *server) checkPassword(r *http.Request, u *accounts.User, password string) string {
	if !s.loginLimit.Allow(sensitiveKey(u)) {
		return tooManyTries
	}
	switch err := s.accounts.ConfirmPassword(r.Context(), u, password); {
	case errors.Is(err, accounts.ErrWrongPassword):
		return "Enter your current password to confirm."
	case err != nil:
		s.log.Error("confirm password", "err", err)
		return "Something went wrong. Try again."
	}
	return ""
}

// handleCancelEmailChange drops a pending email change.
func (s *server) handleCancelEmailChange(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	if _, err := s.accounts.CancelEmailChange(r.Context(), u, s.clientOf(r)); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account?done=email-canceled", http.StatusSeeOther)
}
