package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/codebled/gopherdex/internal/accounts"
	"github.com/codebled/gopherdex/internal/registry"
)

// Team pages live at /orgs/<org>/teams/<team> ("orgs" is a reserved
// namespace, so it can't clash with a module path) and are only shown to
// the organization's members. Organization owners manage teams there.

type teamData struct {
	Org        *registry.Org
	Team       *registry.Team
	Members    []registry.TeamMember
	Modules    []registry.TeamModule
	Candidates []string // org members not yet in the team, for the add form
	OrgModules []string // the org's module paths, for the add form
	IsOrgOwner bool
	Notice     string
	Error      string
}

func teamURL(org, team string) string {
	return "/orgs/" + url.PathEscape(org) + "/teams/" + url.PathEscape(team)
}

func (s *server) handleTeam(w http.ResponseWriter, r *http.Request) {
	s.renderTeam(w, r, r.PathValue("org"), r.PathValue("team"), http.StatusOK, "")
}

func (s *server) renderTeam(w http.ResponseWriter, r *http.Request, orgName, teamName string, status int, errMsg string) {
	ctx := r.Context()
	u := s.currentUser(r)
	org, ok, err := s.registry.OrgByName(ctx, orgName)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	member := false
	if ok {
		if member, err = s.registry.IsOrgMember(ctx, u, orgName); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	if !member {
		s.notFound(w, r)
		return
	}
	team, err := s.registry.TeamByName(ctx, orgName, teamName)
	var re *registry.Error
	if errors.As(err, &re) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data := teamData{Org: org, Team: team, Notice: manageNotices[r.URL.Query().Get("done")], Error: errMsg}
	if data.Members, err = s.registry.TeamMembers(ctx, team.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if data.Modules, err = s.registry.TeamModules(ctx, team.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if data.IsOrgOwner, err = s.registry.IsOrgOwner(ctx, u, orgName); err != nil {
		s.serverError(w, r, err)
		return
	}
	if data.IsOrgOwner {
		inTeam := map[string]bool{}
		for _, m := range data.Members {
			inTeam[m.Username] = true
		}
		all, err := s.registry.OrgMembers(ctx, orgName)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for _, m := range all {
			if !inTeam[m.Username] {
				data.Candidates = append(data.Candidates, m.Username)
			}
		}
		mods, err := s.registry.NamespaceModules(ctx, orgName)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for _, m := range mods {
			data.OrgModules = append(data.OrgModules, m.Path)
		}
	}
	s.render(w, r, status, "team", "@"+orgName+"/"+team.Name, data)
}

// teamAction handles a form on a team page: it runs act, then redirects
// back to the team page with a notice, or shows it again with the error.
func (s *server) teamAction(w http.ResponseWriter, r *http.Request, done string, act func(u *accounts.User, org, team string) error) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	org, team := accounts.NormalizeUsername(r.PostFormValue("org")), strings.ToLower(strings.TrimSpace(r.PostFormValue("team")))
	err := act(u, org, team)
	var re *registry.Error
	switch {
	case err == nil:
		http.Redirect(w, r, teamURL(org, team)+"?done="+done, http.StatusSeeOther)
	case errors.As(err, &re) && re.Code == "unknown_org":
		s.notFound(w, r)
	case errors.As(err, &re):
		s.renderTeam(w, r, org, team, re.Status, re.Message)
	default:
		s.serverError(w, r, err)
	}
}

func (s *server) handleCreateTeam(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil || !parseForm(w, r) {
		return
	}
	org, team := accounts.NormalizeUsername(r.PostFormValue("org")), strings.ToLower(strings.TrimSpace(r.PostFormValue("team")))
	err := s.registry.CreateTeam(r.Context(), u, org, team, r.PostFormValue("description"), s.clientOf(r))
	var re *registry.Error
	switch {
	case err == nil:
		http.Redirect(w, r, teamURL(org, team)+"?done=team-created", http.StatusSeeOther)
	case errors.As(err, &re):
		s.renderOwner(w, r, org, re.Status, re.Message)
	default:
		s.serverError(w, r, err)
	}
}

func (s *server) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	s.orgAction(w, r, "team-deleted", func(u *accounts.User, org string) error {
		return s.registry.DeleteTeam(r.Context(), u, org, r.PostFormValue("team"), s.clientOf(r))
	})
}

func (s *server) handleMemberAccess(w http.ResponseWriter, r *http.Request) {
	s.orgAction(w, r, "access-set", func(u *accounts.User, org string) error {
		return s.registry.SetMemberAccess(r.Context(), u, org, r.PostFormValue("access"), s.clientOf(r))
	})
}

func (s *server) handleAddTeamMember(w http.ResponseWriter, r *http.Request) {
	s.teamAction(w, r, "team-member-added", func(u *accounts.User, org, team string) error {
		target := accounts.NormalizeUsername(strings.TrimPrefix(strings.TrimSpace(r.PostFormValue("username")), "@"))
		if err := s.registry.AddTeamMember(r.Context(), u, org, team, target, s.clientOf(r)); err != nil {
			return err
		}
		if target != u.Username {
			s.emailUser(target, accounts.EmailAccess, "You're now in the team @"+org+"/"+team,
				fmt.Sprintf("@%s added you to the team @%s/%s on Gopherdex. You have the access the team has to @%s's modules.\n\n%s%s",
					u.Username, org, team, org, s.siteURL, teamURL(org, team)))
		}
		return nil
	})
}

func (s *server) handleRemoveTeamMember(w http.ResponseWriter, r *http.Request) {
	s.teamAction(w, r, "team-member-removed", func(u *accounts.User, org, team string) error {
		return s.registry.RemoveTeamMember(r.Context(), u, org, team, r.PostFormValue("username"), s.clientOf(r))
	})
}

func (s *server) handleSetTeamModule(w http.ResponseWriter, r *http.Request) {
	s.teamAction(w, r, "team-module-set", func(u *accounts.User, org, team string) error {
		return s.registry.SetTeamModule(r.Context(), u, org, team, r.PostFormValue("module"), r.PostFormValue("role"), s.clientOf(r))
	})
}

func (s *server) handleRemoveTeamModule(w http.ResponseWriter, r *http.Request) {
	s.teamAction(w, r, "team-module-removed", func(u *accounts.User, org, team string) error {
		return s.registry.RemoveTeamModule(r.Context(), u, org, team, r.PostFormValue("module"), s.clientOf(r))
	})
}
