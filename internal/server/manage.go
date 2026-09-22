package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/module"
	"github.com/parthiban-sivakumar/gopherdex/internal/registry"
)

// manageNotices are the fixed messages shown after a maintainer action.
var manageNotices = map[string]string{
	"yanked":              "Version yanked. It's hidden from new installs; builds that already use it keep working.",
	"unyanked":            "Version restored. It's installable again.",
	"deprecated":          "Module marked deprecated. The notice shows on its page and in search.",
	"undeprecated":        "Deprecation notice removed.",
	"role-set":            "Access updated.",
	"role-removed":        "Access removed.",
	"org-created":         "Organization created. Publish modules under its namespace, and add members below.",
	"member-set":          "Member updated.",
	"member-removed":      "Member removed.",
	"publisher-added":     "Trusted publisher added. Its workflow can now publish with gopherdex publish, no API token needed.",
	"publisher-removed":   "Trusted publisher removed. Tokens it had already received are revoked.",
	"access-set":          "Member access updated.",
	"team-created":        "Team created. Add members and give it access to modules below.",
	"team-deleted":        "Team deleted. Its members lost the access it gave them, and stay in the organization.",
	"team-member-added":   "Added to the team.",
	"team-member-removed": "Removed from the team. They stay in the organization.",
	"team-module-set":     "Team access updated.",
	"team-module-removed": "Team access removed.",
}

// maintainerAction handles a manage-tab form: it checks the module, runs
// act, then redirects back to the manage tab with a notice, or shows the
// tab again with the error.
func (s *server) maintainerAction(w http.ResponseWriter, r *http.Request, done string, act func(u *accounts.User, modPath string) error) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	modPath := r.PostFormValue("module")
	if module.CheckPath(modPath) != nil {
		s.notFound(w, r)
		return
	}
	err := act(u, modPath)
	var re *registry.Error
	switch {
	case err == nil:
		http.Redirect(w, r, s.project.URL(modPath, "")+"?"+url.Values{"tab": {"manage"}, "done": {done}}.Encode(), http.StatusSeeOther)
	case errors.Is(err, module.ErrNotFound):
		s.notFound(w, r)
	case errors.As(err, &re):
		s.renderProject(w, r, modPath, "", "manage", re.Status, re.Message)
	default:
		s.serverError(w, r, err)
	}
}

func (s *server) handleYank(w http.ResponseWriter, r *http.Request) {
	s.maintainerAction(w, r, "yanked", func(u *accounts.User, modPath string) error {
		return s.registry.Yank(r.Context(), u, modPath, r.PostFormValue("version"), r.PostFormValue("reason"), s.clientOf(r))
	})
}

func (s *server) handleUnyank(w http.ResponseWriter, r *http.Request) {
	s.maintainerAction(w, r, "unyanked", func(u *accounts.User, modPath string) error {
		return s.registry.Unyank(r.Context(), u, modPath, r.PostFormValue("version"), s.clientOf(r))
	})
}

func (s *server) handleDeprecate(w http.ResponseWriter, r *http.Request) {
	s.maintainerAction(w, r, "deprecated", func(u *accounts.User, modPath string) error {
		return s.registry.Deprecate(r.Context(), u, modPath, r.PostFormValue("message"), r.PostFormValue("successor"), s.clientOf(r))
	})
}

func (s *server) handleUndeprecate(w http.ResponseWriter, r *http.Request) {
	s.maintainerAction(w, r, "undeprecated", func(u *accounts.User, modPath string) error {
		return s.registry.Undeprecate(r.Context(), u, modPath, s.clientOf(r))
	})
}

func (s *server) handleSetCollaborator(w http.ResponseWriter, r *http.Request) {
	s.maintainerAction(w, r, "role-set", func(u *accounts.User, modPath string) error {
		target, role := accounts.NormalizeUsername(strings.TrimPrefix(strings.TrimSpace(r.PostFormValue("username")), "@")), r.PostFormValue("role")
		if err := s.registry.SetCollaborator(r.Context(), u, modPath, target, role, s.clientOf(r)); err != nil {
			return err
		}
		if target != u.Username {
			s.notifyRole(r, target, registry.InviteModule, modPath, role, u.Username, s.project.URL(modPath, ""))
		}
		return nil
	})
}

func (s *server) handleRemoveCollaborator(w http.ResponseWriter, r *http.Request) {
	s.maintainerAction(w, r, "role-removed", func(u *accounts.User, modPath string) error {
		return s.registry.RemoveCollaborator(r.Context(), u, modPath, r.PostFormValue("username"), s.clientOf(r))
	})
}

// ---- Organizations ----

func (s *server) orgAction(w http.ResponseWriter, r *http.Request, done string, act func(u *accounts.User, org string) error) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	org := accounts.NormalizeUsername(r.PostFormValue("org"))
	err := act(u, org)
	var re *registry.Error
	switch {
	case err == nil:
		http.Redirect(w, r, "/"+org+"?done="+done, http.StatusSeeOther)
	case errors.As(err, &re):
		s.renderOwner(w, r, org, re.Status, re.Message)
	default:
		s.serverError(w, r, err)
	}
}

func (s *server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	name := accounts.NormalizeUsername(r.PostFormValue("org"))
	err := s.registry.CreateOrg(r.Context(), u, name, r.PostFormValue("display_name"), s.clientOf(r))
	var re *registry.Error
	switch {
	case err == nil:
		http.Redirect(w, r, "/"+name+"?done=org-created", http.StatusSeeOther)
	case errors.As(err, &re):
		s.renderAccount(w, r, re.Status, accountData{OrgName: name, Errors: map[string]string{"org": re.Message}})
	default:
		s.serverError(w, r, err)
	}
}

func (s *server) handleSetOrgMember(w http.ResponseWriter, r *http.Request) {
	s.orgAction(w, r, "member-set", func(u *accounts.User, org string) error {
		target, role := accounts.NormalizeUsername(strings.TrimPrefix(strings.TrimSpace(r.PostFormValue("username")), "@")), r.PostFormValue("role")
		if err := s.registry.SetOrgMember(r.Context(), u, org, target, role, s.clientOf(r)); err != nil {
			return err
		}
		if target != u.Username {
			s.notifyRole(r, target, registry.InviteOrg, org, role, u.Username, "/"+org)
		}
		return nil
	})
}

func (s *server) handleRemoveOrgMember(w http.ResponseWriter, r *http.Request) {
	s.orgAction(w, r, "member-removed", func(u *accounts.User, org string) error {
		return s.registry.RemoveOrgMember(r.Context(), u, org, r.PostFormValue("username"), s.clientOf(r))
	})
}

// ---- API ----

// handleAPIYank yanks or restores a version for the CLI:
// POST /api/yank {"module": …, "version": …, "reason": …, "yank": true}
func (s *server) handleAPIYank(w http.ResponseWriter, r *http.Request) {
	u, tok, ok := s.bearerUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Module  string `json:"module"`
		Version string `json:"version"`
		Reason  string `json:"reason"`
		Yank    bool   `json:"yank"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{"Send JSON: {\"module\", \"version\", \"reason\", \"yank\"}."})
		return
	}
	if module.CheckPath(req.Module) != nil || module.CheckVersion(req.Version) != nil {
		writeJSON(w, http.StatusBadRequest, apiError{"Give a module path and a version like v1.2.3."})
		return
	}
	err := registry.TokenAllows(tok, u, req.Module)
	if err == nil && tok.PublisherID != 0 {
		writeJSON(w, http.StatusForbidden, apiError{"Tokens from trusted publishing can only upload releases. Yank from the website or with a personal API token."})
		return
	}
	if err == nil {
		if req.Yank {
			err = s.registry.Yank(r.Context(), u, req.Module, req.Version, req.Reason, s.clientOf(r))
		} else {
			err = s.registry.Unyank(r.Context(), u, req.Module, req.Version, s.clientOf(r))
		}
	}
	var re *registry.Error
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, struct {
			Module  string `json:"module"`
			Version string `json:"version"`
			Yanked  bool   `json:"yanked"`
			URL     string `json:"url"`
		}{req.Module, req.Version, req.Yank, s.siteURL + s.project.URL(req.Module, req.Version)})
	case errors.As(err, &re):
		writeJSON(w, re.Status, struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}{re.Message, re.Code})
	default:
		s.apiError(w, r, err)
	}
}

// notifyRole emails someone given a role: an invitation to accept, or, for
// someone who already accepted, the change.
func (s *server) notifyRole(r *http.Request, target, kind, name, role, by, path string) {
	what := name
	if kind == registry.InviteOrg {
		what = "the organization @" + name
	}
	pending, err := s.registry.InvitationPending(r.Context(), kind, name, target)
	if err != nil {
		s.log.Error("check invitation", "err", err)
		return
	}
	if pending {
		s.emailUser(target, accounts.EmailAccess, "@"+by+" invited you to "+name+" on Gopherdex",
			fmt.Sprintf("@%s invited you to be %s %s of %s on Gopherdex. Nothing changes until you accept: accept or decline on your account page.\n\n%s/account",
				by, article(role), role, what, s.siteURL))
		return
	}
	s.emailUser(target, accounts.EmailAccess, "You're now "+article(role)+" "+role+" of "+name,
		fmt.Sprintf("@%s made you %s %s of %s on Gopherdex.\n\n%s%s", by, article(role), role, what, s.siteURL, path))
}

// ---- Invitations and leaving ----

func (s *server) handleInvitation(accept bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.requireUser(w, r)
		if u == nil || !parseForm(w, r) {
			return
		}
		kind, name := r.PostFormValue("kind"), r.PostFormValue("name")
		if kind != registry.InviteOrg && kind != registry.InviteModule {
			http.Error(w, "Unknown invitation.", http.StatusBadRequest)
			return
		}
		act, done := s.registry.DeclineInvitation, "invite-declined"
		if accept {
			act, done = s.registry.AcceptInvitation, "invite-accepted"
		}
		err := act(r.Context(), u, kind, name, s.clientOf(r))
		var re *registry.Error
		switch {
		case err == nil:
			http.Redirect(w, r, "/account?done="+done, http.StatusSeeOther)
		case errors.As(err, &re):
			s.renderAccount(w, r, re.Status, accountData{Error: re.Message})
		default:
			s.serverError(w, r, err)
		}
	}
}

func (s *server) handleLeaveOrg(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	err := s.registry.LeaveOrg(r.Context(), u, accounts.NormalizeUsername(r.PostFormValue("org")), s.clientOf(r))
	var re *registry.Error
	switch {
	case err == nil:
		http.Redirect(w, r, "/account?done=left", http.StatusSeeOther)
	case errors.As(err, &re):
		s.renderAccount(w, r, re.Status, accountData{Error: re.Message})
	default:
		s.serverError(w, r, err)
	}
}

func (s *server) handleLeaveModule(w http.ResponseWriter, r *http.Request) {
	s.maintainerAction(w, r, "role-removed", func(u *accounts.User, modPath string) error {
		return s.registry.LeaveModule(r.Context(), u, modPath, s.clientOf(r))
	})
}
