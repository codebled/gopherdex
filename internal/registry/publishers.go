package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/parthiban-sivakumar/gopherdex/internal/accounts"
	"github.com/parthiban-sivakumar/gopherdex/internal/module"
	"github.com/parthiban-sivakumar/gopherdex/internal/oidc"
)

// Trusted publishing, as on PyPI: an owner names the GitHub repository and
// workflow allowed to publish a module, and that workflow trades a GitHub
// OIDC ID token for a short-lived, single-module API token. Nothing
// long-lived is stored in CI, and every such release records which run
// built it.

const maxPublishersPerModule = 10

// Publisher is a trusted publisher.
type Publisher struct {
	ID          int64
	ModulePath  string
	Provider    string // "github"
	Repository  string // "owner/name"
	Workflow    string // "release.yml"
	Environment string // "" means any
	CreatedBy   string // username
	CreatedByID int64
	CreatedAt   time.Time
	LastUsedAt  *time.Time
	Pending     bool // the module hasn't been published yet
}

// RepositoryURL links to the repository.
func (p Publisher) RepositoryURL() string { return "https://github.com/" + p.Repository }

// WorkflowURL links to the workflow file on the default branch.
func (p Publisher) WorkflowURL() string {
	return p.RepositoryURL() + "/blob/HEAD/.github/workflows/" + p.Workflow
}

var (
	githubOwner  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38})$`)
	githubRepo   = regexp.MustCompile(`^[a-z0-9._-]{1,100}$`)
	workflowFile = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}\.ya?ml$`)
)

// normalizePublisher checks and tidies what someone typed into the form:
// "https://github.com/Octo/Retry.git" becomes "octo/retry", and
// ".github/workflows/release.yml" becomes "release.yml".
func normalizePublisher(repository, workflow, environment string) (string, string, string, error) {
	repo := strings.TrimSpace(repository)
	for _, p := range []string{"https://", "http://", "github.com/", "www.github.com/"} {
		repo = strings.TrimPrefix(repo, p)
	}
	repo = strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(repo, "/"), ".git"))
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !githubOwner.MatchString(owner) || !githubRepo.MatchString(name) || name == "." || name == ".." {
		return "", "", "", reject(http.StatusBadRequest, "invalid_repository",
			"Give the GitHub repository as owner/name, for example octo-org/retry.")
	}
	wf := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(workflow), "/"), ".github/workflows/")
	if !workflowFile.MatchString(wf) {
		return "", "", "", reject(http.StatusBadRequest, "invalid_workflow",
			"Give the workflow's file name in .github/workflows, for example release.yml.")
	}
	env := strings.TrimSpace(environment)
	if len(env) > 255 || strings.ContainsFunc(env, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", "", "", reject(http.StatusBadRequest, "invalid_environment", "Environment names are up to 255 characters, without control characters.")
	}
	return repo, wf, env, nil
}

// CanPublish reports whether u may publish modPath, with an explanation
// when they can't.
func (r *Registry) CanPublish(ctx context.Context, u *accounts.User, modPath string) error {
	namespace, err := r.CheckModulePath(modPath)
	if err != nil {
		return err
	}
	return r.canPublish(ctx, u, modPath, namespace)
}

// canManagePublishers decides who may add or remove trusted publishers:
// owners of an existing module, or, before its first release, anyone who
// could publish it.
func (r *Registry) canManagePublishers(ctx context.Context, u *accounts.User, modPath string) (exists bool, err error) {
	role, err := r.Role(ctx, u, modPath)
	switch {
	case err == nil && role >= RoleOwner:
		return true, nil
	case err == nil:
		return true, reject(http.StatusForbidden, "forbidden", "Only owners of %s can manage its trusted publishers.", modPath)
	case !errors.Is(err, module.ErrNotFound):
		return false, err
	}
	return false, r.CanPublish(ctx, u, modPath)
}

// AddPublisher lets a GitHub Actions workflow publish modPath.
func (r *Registry) AddPublisher(ctx context.Context, u *accounts.User, modPath, repository, workflow, environment string, c accounts.Client) (*Publisher, error) {
	if !u.EmailVerified {
		return nil, reject(http.StatusForbidden, "email_not_verified", "Verify your email address before adding a trusted publisher.")
	}
	repo, wf, env, err := normalizePublisher(repository, workflow, environment)
	if err != nil {
		return nil, err
	}
	exists, err := r.canManagePublishers(ctx, u, modPath)
	if err != nil {
		return nil, err
	}
	now := r.now()
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("add publisher: %w", err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM trusted_publishers WHERE module_path = ?`, modPath).Scan(&n); err != nil {
		return nil, fmt.Errorf("add publisher: %w", err)
	}
	if n >= maxPublishersPerModule {
		return nil, reject(http.StatusConflict, "too_many_publishers", "%s already has %d trusted publishers. Remove one first.", modPath, n)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO trusted_publishers (module_path, provider, repository, workflow, environment, created_by, created_at)
		VALUES (?, 'github', ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, modPath, repo, wf, env, u.ID, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("add publisher: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, reject(http.StatusConflict, "publisher_exists", "That workflow can already publish %s.", modPath)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("add publisher: %w", err)
	}
	if err := r.audit(ctx, tx, u.ID, "publisher.added", fmt.Sprintf("%s github:%s/%s env=%q", modPath, repo, wf, env), c); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("add publisher: %w", err)
	}
	return &Publisher{ID: id, ModulePath: modPath, Provider: "github", Repository: repo, Workflow: wf, Environment: env,
		CreatedBy: u.Username, CreatedByID: u.ID, CreatedAt: now, Pending: !exists}, nil
}

// RemovePublisher deletes a trusted publisher and returns it.
func (r *Registry) RemovePublisher(ctx context.Context, u *accounts.User, id int64, c accounts.Client) (*Publisher, error) {
	p, err := r.publisher(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := r.canManagePublishers(ctx, u, p.ModulePath); err != nil {
		return nil, err
	}
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("remove publisher: %w", err)
	}
	defer tx.Rollback()
	// Tokens it already minted die with it. Revoke them before the delete
	// clears their publisher_id.
	if _, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE publisher_id = ? AND revoked_at IS NULL`, r.now().Unix(), id); err != nil {
		return nil, fmt.Errorf("remove publisher: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM trusted_publishers WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("remove publisher: %w", err)
	}
	if err := r.audit(ctx, tx, u.ID, "publisher.removed", fmt.Sprintf("%s github:%s/%s env=%q", p.ModulePath, p.Repository, p.Workflow, p.Environment), c); err != nil {
		return nil, err
	}
	return p, tx.Commit()
}

const publisherColumns = `p.id, p.module_path, p.provider, p.repository, p.workflow, p.environment, p.created_by, u.username,
	p.created_at, p.last_used_at, NOT EXISTS (SELECT 1 FROM modules m WHERE m.path = p.module_path)`

func scanPublisher(row interface{ Scan(...any) error }) (Publisher, error) {
	var (
		p        Publisher
		created  int64
		lastUsed sql.NullInt64
	)
	err := row.Scan(&p.ID, &p.ModulePath, &p.Provider, &p.Repository, &p.Workflow, &p.Environment, &p.CreatedByID, &p.CreatedBy,
		&created, &lastUsed, &p.Pending)
	p.CreatedAt = time.Unix(created, 0).UTC()
	if lastUsed.Valid {
		t := time.Unix(lastUsed.Int64, 0).UTC()
		p.LastUsedAt = &t
	}
	return p, err
}

func (r *Registry) publisher(ctx context.Context, id int64) (*Publisher, error) {
	p, err := scanPublisher(r.DB.QueryRowContext(ctx, `SELECT `+publisherColumns+`
		FROM trusted_publishers p JOIN users u ON u.id = p.created_by WHERE p.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("trusted publisher %d: %w", id, module.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("load trusted publisher: %w", err)
	}
	return &p, nil
}

func (r *Registry) publishers(ctx context.Context, where string, args ...any) ([]Publisher, error) {
	//nolint:gosec // constant fragments; callers pass constant where clauses with values as ? args
	rows, err := r.DB.QueryContext(ctx, `SELECT `+publisherColumns+`
		FROM trusted_publishers p JOIN users u ON u.id = p.created_by WHERE `+where+` ORDER BY p.module_path, p.created_at, p.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list trusted publishers: %w", err)
	}
	defer rows.Close()
	var out []Publisher
	for rows.Next() {
		p, err := scanPublisher(rows)
		if err != nil {
			return nil, fmt.Errorf("list trusted publishers: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Publishers lists the trusted publishers of a module.
func (r *Registry) Publishers(ctx context.Context, modPath string) ([]Publisher, error) {
	return r.publishers(ctx, `p.module_path = ?`, modPath)
}

// PendingPublishers lists the publishers u set up for modules that don't
// exist yet.
func (r *Registry) PendingPublishers(ctx context.Context, u *accounts.User) ([]Publisher, error) {
	return r.publishers(ctx, `p.created_by = ? AND NOT EXISTS (SELECT 1 FROM modules m WHERE m.path = p.module_path)`, u.ID)
}

// Provenance records which CI run published a version, from the ID token
// the registry verified.
type Provenance struct {
	Provider    string `json:"provider"` // "github"
	PublisherID int64  `json:"publisherId"`
	Repository  string `json:"repository"` // "owner/name"
	Workflow    string `json:"workflow"`
	WorkflowRef string `json:"workflowRef"`
	Environment string `json:"environment,omitempty"`
	Ref         string `json:"ref"`
	SHA         string `json:"sha"`
	RunID       string `json:"runId"`
	RunAttempt  string `json:"runAttempt"`
	Event       string `json:"event"`
}

// RepositoryURL links to the repository that built the release.
func (p *Provenance) RepositoryURL() string { return "https://github.com/" + p.Repository }

// RunURL links to the workflow run's logs.
func (p *Provenance) RunURL() string {
	if p.RunID == "" {
		return ""
	}
	u := p.RepositoryURL() + "/actions/runs/" + p.RunID
	if p.RunAttempt != "" && p.RunAttempt != "1" {
		u += "/attempts/" + p.RunAttempt
	}
	return u
}

// CommitURL links to the commit the workflow ran on.
func (p *Provenance) CommitURL() string {
	if p.SHA == "" {
		return ""
	}
	return p.RepositoryURL() + "/commit/" + p.SHA
}

// WorkflowURL links to the workflow file at the commit that ran.
func (p *Provenance) WorkflowURL() string {
	at := p.SHA
	if at == "" {
		at = "HEAD"
	}
	return p.RepositoryURL() + "/blob/" + at + "/.github/workflows/" + p.Workflow
}

// ParseProvenance decodes a stored provenance record, or returns nil.
func ParseProvenance(s string) *Provenance {
	if s == "" {
		return nil
	}
	var p Provenance
	if json.Unmarshal([]byte(s), &p) != nil || p.Repository == "" {
		return nil
	}
	return &p
}

var (
	runIDPattern = regexp.MustCompile(`^[0-9]{1,20}$`)
	shaPattern   = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)
)

// matchGitHubPublisher finds the trusted publisher of modPath for a
// GitHub workflow run in environment, preferring one tied to that
// environment over one that isn't. It returns nil if none matches, and
// the repository owner ID the publisher pinned, if any. The rows are
// closed before it returns so the caller can keep using tx.
func matchGitHubPublisher(ctx context.Context, tx *sql.Tx, modPath, repo, workflow, environment string) (*Publisher, string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+publisherColumns+`, p.repository_owner_id
		FROM trusted_publishers p JOIN users u ON u.id = p.created_by
		WHERE p.module_path = ? AND p.provider = 'github' AND p.repository = ? AND p.workflow = ?
		ORDER BY p.environment DESC, p.id`, modPath, repo, workflow)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var p Publisher
		var pinned string
		var created int64
		var lastUsed sql.NullInt64
		if err := rows.Scan(&p.ID, &p.ModulePath, &p.Provider, &p.Repository, &p.Workflow, &p.Environment, &p.CreatedByID, &p.CreatedBy,
			&created, &lastUsed, &p.Pending, &pinned); err != nil {
			return nil, "", err
		}
		// A publisher tied to an environment only matches runs in it.
		// GitHub environment names are case-insensitive.
		if p.Environment == "" || strings.EqualFold(p.Environment, environment) {
			p.CreatedAt = time.Unix(created, 0).UTC()
			return &p, pinned, nil
		}
	}
	return nil, "", rows.Err()
}

// ExchangeGitHubToken finds the trusted publisher that lets the GitHub
// Actions run described by claims publish modPath. It consumes the ID
// token, pins the repository owner's account ID on first use, and returns
// the publisher and the provenance to record, as JSON.
func (r *Registry) ExchangeGitHubToken(ctx context.Context, modPath string, claims *oidc.GitHubClaims) (*Publisher, string, error) {
	if _, err := r.CheckModulePath(modPath); err != nil {
		return nil, "", err
	}
	workflow := claims.Workflow()
	repo := strings.ToLower(claims.Repository)
	if workflow == "" || claims.JTI == "" || claims.RepositoryOwnerID == "" {
		return nil, "", reject(http.StatusForbidden, "invalid_claims", "The ID token doesn't name a workflow in its repository.")
	}
	now := r.now()
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", fmt.Errorf("exchange token: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM oidc_token_uses WHERE expires_at < ?`, now.Add(-time.Hour).Unix()); err != nil {
		return nil, "", fmt.Errorf("exchange token: %w", err)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO oidc_token_uses (jti, expires_at) VALUES (?, ?) ON CONFLICT DO NOTHING`, claims.JTI, claims.Exp)
	if err != nil {
		return nil, "", fmt.Errorf("exchange token: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, "", reject(http.StatusForbidden, "token_reused", "This ID token was already exchanged. Request a new one.")
	}

	match, ownerID, err := matchGitHubPublisher(ctx, tx, modPath, repo, workflow, claims.Environment)
	if err != nil {
		return nil, "", fmt.Errorf("exchange token: %w", err)
	}
	if match == nil {
		env := ""
		if claims.Environment != "" {
			env = fmt.Sprintf(", environment %q", claims.Environment)
		}
		return nil, "", reject(http.StatusForbidden, "no_trusted_publisher",
			"No trusted publisher of %s matches this run (repository %s, workflow %s%s). An owner can add one on the module's Manage tab, or on their account page before the first release.",
			modPath, claims.Repository, workflow, env)
	}
	if ownerID != "" && ownerID != claims.RepositoryOwnerID {
		return nil, "", reject(http.StatusForbidden, "owner_changed",
			"The GitHub account %q isn't the one that owned it when this trusted publisher was first used. If the account was renamed or re-created on purpose, remove the publisher and add it again.",
			claims.RepositoryOwner)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE trusted_publishers SET repository_owner_id = ?, last_used_at = ? WHERE id = ?`,
		claims.RepositoryOwnerID, now.Unix(), match.ID); err != nil {
		return nil, "", fmt.Errorf("exchange token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, "", fmt.Errorf("exchange token: %w", err)
	}

	prov := Provenance{
		Provider: "github", PublisherID: match.ID, Repository: repo, Workflow: workflow,
		WorkflowRef: claims.WorkflowRef, Environment: claims.Environment, Ref: claims.Ref, Event: claims.EventName,
	}
	if shaPattern.MatchString(claims.SHA) {
		prov.SHA = claims.SHA
	}
	if runIDPattern.MatchString(claims.RunID) {
		prov.RunID = claims.RunID
	}
	if runIDPattern.MatchString(claims.RunAttempt) {
		prov.RunAttempt = claims.RunAttempt
	}
	b, err := json.Marshal(prov)
	if err != nil {
		return nil, "", err
	}
	return match, string(b), nil
}
