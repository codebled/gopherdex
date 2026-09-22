package accounts

import (
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// FieldError is a problem with one form field, worded for the person filling
// in the form.
type FieldError struct {
	Field   string
	Message string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Message }

const (
	minPasswordLen = 10
	maxPasswordLen = 256
)

// Usernames become the owner segment of module paths
// (gopherdex.dev/<username>/<module>), so they use the strictest safe
// subset of module path characters.
var usernamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9]|-[a-z0-9]){1,38}$`)

// majorVersion matches names like v2 that the go command treats as major
// version suffixes.
var majorVersion = regexp.MustCompile(`^v[0-9]+$`)

// reserved names would collide with site routes or mislead users.
var reserved = []string{
	"about", "account", "accounts", "admin", "administrator", "advisories", "advisory", "api", "app", "assets", "auth",
	"badge", "badges", "feed", "feeds", "vulndb", "vuln",
	"blog", "docs", "download", "downloads", "favicon.ico", "gopherdex", "golang", "go",
	"healthz", "help", "join", "legal", "login", "logout", "mail", "modules", "new", "publish", "news", "org",
	"orgs", "organizations", "privacy", "project", "projects", "proxy", "register", "report", "reset-password", "forgot-password", "root",
	"search", "security", "settings", "signin", "signout", "signup", "sitemap", "static",
	"status", "std", "stdlib", "support", "terms", "tokens", "user", "users", "verify",
	"verify-email", "www",
}

// NormalizeUsername lower-cases and trims a username.
func NormalizeUsername(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// NormalizeEmail lower-cases and trims an email address.
func NormalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func validateUsername(username string) error {
	switch {
	case len(username) > 39 || !usernamePattern.MatchString(username):
		return &FieldError{"username", "Use 2 to 39 lower-case letters, digits or single hyphens, starting and ending with a letter or digit."}
	case majorVersion.MatchString(username):
		return &FieldError{"username", "Names like v2 are reserved because Go uses them for major versions."}
	case slices.Contains(reserved, username):
		return &FieldError{"username", "That name is reserved. Choose another."}
	}
	return nil
}

func validateEmail(email string) error {
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || len(email) > 254 || !strings.Contains(email, ".") {
		return &FieldError{"email", "Enter an email address like you@example.com."}
	}
	return nil
}

func validatePassword(password, username, email string) error {
	n := utf8.RuneCountInString(password)
	switch {
	case n < minPasswordLen:
		return &FieldError{"password", "Use at least 10 characters."}
	case len(password) > maxPasswordLen:
		return &FieldError{"password", "Use at most 256 characters."}
	case strings.EqualFold(password, username) || strings.EqualFold(password, email):
		return &FieldError{"password", "Your password can't be your username or email address."}
	}
	return nil
}

// ValidateNamespace checks a name for a user or organization namespace. The
// returned FieldError uses field "username".
func ValidateNamespace(name string) error { return validateUsername(name) }
