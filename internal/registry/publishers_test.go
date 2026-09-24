package registry

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/codebled/gopherdex/internal/accounts"
	"github.com/codebled/gopherdex/internal/oidc"
)

var ghClaimsSeq int

// ghClaims are verified claims for a run of workflow in repo.
func ghClaims(repo, workflow string) *oidc.GitHubClaims {
	ghClaimsSeq++
	return &oidc.GitHubClaims{
		JTI: "jti-" + strconv.Itoa(ghClaimsSeq), Exp: time.Now().Add(5 * time.Minute).Unix(),
		Repository: repo, RepositoryOwner: "Alice", RepositoryOwnerID: "2002",
		WorkflowRef: repo + "/.github/workflows/" + workflow + "@refs/tags/v1.0.0",
		Ref:         "refs/tags/v1.0.0", SHA: "0123456789abcdef0123456789abcdef01234567",
		RunID: "42", RunAttempt: "2", EventName: "push",
	}
}

// mint exchanges claims and mints the token, as the server does.
func (f *fixture) mint(t *testing.T, modPath string, c *oidc.GitHubClaims) (*accounts.User, *accounts.Token, error) {
	t.Helper()
	ctx := context.Background()
	p, prov, err := f.reg.ExchangeGitHubToken(ctx, modPath, c)
	if err != nil {
		return nil, nil, err
	}
	u, err := f.accts.UserByID(ctx, p.CreatedByID)
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := f.accts.MintPublisherToken(ctx, u, "GitHub Actions", accounts.ScopeModulePrefix+modPath, 15*time.Minute, p.ID, prov, client)
	if err != nil {
		t.Fatal(err)
	}
	return f.accts.AuthenticateToken(ctx, secret)
}

func TestTrustedPublishing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mod := host + "/alice/retry"

	// Before the first release: a pending publisher, typed sloppily.
	p, err := f.reg.AddPublisher(ctx, f.alice, mod, " https://github.com/Alice/Retry.git ", ".github/workflows/release.yml", "", client)
	if err != nil {
		t.Fatal(err)
	}
	if p.Repository != "alice/retry" || p.Workflow != "release.yml" || !p.Pending {
		t.Errorf("publisher = %+v", p)
	}
	if _, err := f.reg.AddPublisher(ctx, f.alice, mod, "alice/retry", "release.yml", "", client); !isReject(err, http.StatusConflict, "publisher_exists") {
		t.Errorf("duplicate: %v", err)
	}
	pending, err := f.reg.PendingPublishers(ctx, f.alice)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %v, %v", pending, err)
	}

	// The workflow publishes the first release.
	u, tok, err := f.mint(t, mod, ghClaims("Alice/Retry", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != f.alice.ID || tok.PublisherID != p.ID || tok.Scope != accounts.ScopeModulePrefix+mod {
		t.Errorf("minted token = %+v for %s", tok, u.Username)
	}
	up := f.upload(mod, "v1.0.0", f.zipModule(t, mod, "v1.0.0", retryFiles(mod, "")))
	up.User, up.Token, up.Repository = u, tok, "https://evil.example/fake"
	if _, err := f.reg.Publish(ctx, up); err != nil {
		t.Fatal(err)
	}
	d, err := f.reg.Module(ctx, mod)
	if err != nil {
		t.Fatal(err)
	}
	v := d.Versions[0]
	if v.Provenance == nil || v.Provenance.Repository != "alice/retry" || v.Provenance.Workflow != "release.yml" || v.Provenance.RunID != "42" {
		t.Fatalf("provenance = %+v", v.Provenance)
	}
	if v.Repository != "https://github.com/alice/retry" {
		t.Errorf("repository = %q; the verified one should replace what the client sent", v.Repository)
	}
	if got := v.Provenance.RunURL(); got != "https://github.com/alice/retry/actions/runs/42/attempts/2" {
		t.Errorf("run URL = %q", got)
	}
	// Token uploads carry no provenance.
	if err := f.publishAs(t, f.alice, f.token, mod, "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	if d, _ := f.reg.Module(ctx, mod); d.Versions[0].Provenance != nil {
		t.Errorf("token upload has provenance %+v", d.Versions[0].Provenance)
	}

	if list, _ := f.reg.PendingPublishers(ctx, f.alice); len(list) != 0 {
		t.Errorf("publisher still pending after the first release: %+v", list)
	}

	// A minted token can't touch other modules.
	other := host + "/alice/other"
	up = f.upload(other, "v1.0.0", f.zipModule(t, other, "v1.0.0", retryFiles(other, "")))
	up.User, up.Token = u, tok
	if _, err := f.reg.Publish(ctx, up); !isReject(err, http.StatusForbidden, "token_scope") {
		t.Errorf("publish other module with minted token: %v", err)
	}

	// Runs that don't match are refused.
	reused := ghClaims("alice/retry", "release.yml")
	if _, _, err := f.mint(t, mod, reused); err != nil {
		t.Fatal(err)
	}
	wrongOwner := ghClaims("alice/retry", "release.yml")
	wrongOwner.RepositoryOwnerID = "666"
	noWorkflow := ghClaims("alice/retry", "release.yml")
	noWorkflow.WorkflowRef = "mallory/tools/.github/workflows/release.yml@refs/heads/main"
	for name, tc := range map[string]struct {
		mod    string
		claims *oidc.GitHubClaims
		code   string
	}{
		"reused token":      {mod, reused, "token_reused"},
		"other repository":  {mod, ghClaims("mallory/retry", "release.yml"), "no_trusted_publisher"},
		"other workflow":    {mod, ghClaims("alice/retry", "test.yml"), "no_trusted_publisher"},
		"other module":      {other, ghClaims("alice/retry", "release.yml"), "no_trusted_publisher"},
		"re-created owner":  {mod, wrongOwner, "owner_changed"},
		"reusable workflow": {mod, noWorkflow, "invalid_claims"},
	} {
		if _, _, err := f.mint(t, tc.mod, tc.claims); !isReject(err, http.StatusForbidden, tc.code) {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
	}

	// Environments: a publisher tied to one only matches runs in it.
	envMod := host + "/alice/envmod"
	if _, err := f.reg.AddPublisher(ctx, f.alice, envMod, "alice/retry", "release.yml", "PyPI-Release", client); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.mint(t, envMod, ghClaims("alice/retry", "release.yml")); !isReject(err, http.StatusForbidden, "no_trusted_publisher") {
		t.Errorf("run without the environment: %v", err)
	}
	inEnv := ghClaims("alice/retry", "release.yml")
	inEnv.Environment = "pypi-release"
	if _, _, err := f.mint(t, envMod, inEnv); err != nil {
		t.Errorf("run in the environment: %v", err)
	}

	// Removing the publisher revokes what it minted.
	_, live, err := f.mint(t, mod, ghClaims("alice/retry", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.RemovePublisher(ctx, f.alice, p.ID, client); err != nil {
		t.Fatal(err)
	}
	var revoked bool
	f.accts.DB.QueryRowContext(ctx, `SELECT revoked_at IS NOT NULL FROM api_tokens WHERE id = ?`, live.ID).Scan(&revoked)
	if !revoked {
		t.Error("token minted by a removed publisher still works")
	}
	if tokens, _ := f.accts.Tokens(ctx, f.alice.ID); len(tokens) != 1 {
		t.Errorf("account token list shows %d tokens; minted ones should be hidden", len(tokens))
	}
}

func TestPublisherPermissions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bob, _ := f.user(t, "bob")
	mod := host + "/alice/retry"
	if err := f.publishAs(t, f.alice, f.token, mod, "v1.0.0"); err != nil {
		t.Fatal(err)
	}

	// Strangers can't add publishers, to existing modules or new ones in
	// someone else's namespace.
	if _, err := f.reg.AddPublisher(ctx, bob, mod, "bob/retry", "release.yml", "", client); !isReject(err, http.StatusForbidden, "forbidden") {
		t.Errorf("stranger on existing module: %v", err)
	}
	if _, err := f.reg.AddPublisher(ctx, bob, host+"/alice/new", "bob/new", "release.yml", "", client); !isReject(err, http.StatusForbidden, "forbidden_namespace") {
		t.Errorf("stranger on new module in alice's namespace: %v", err)
	}
	// Maintainers publish but don't manage publishers.
	if err := f.setCollaborator(ctx, f.alice, mod, "bob", "maintainer", client); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.AddPublisher(ctx, bob, mod, "bob/retry", "release.yml", "", client); !isReject(err, http.StatusForbidden, "forbidden") {
		t.Errorf("maintainer: %v", err)
	}

	for name, in := range map[string][3]string{
		"bad repository":  {"not a repo", "release.yml", ""},
		"bad workflow":    {"alice/retry", "release.sh", ""},
		"nested workflow": {"alice/retry", "ci/release.yml", ""},
	} {
		if _, err := f.reg.AddPublisher(ctx, f.alice, mod, in[0], in[1], in[2], client); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A publisher set up by bob stops working once bob loses access, and
	// the server can say why before any upload.
	if err := f.setCollaborator(ctx, f.alice, mod, "bob", "owner", client); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.AddPublisher(ctx, bob, mod, "bob/retry", "release.yml", "", client); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.RemoveCollaborator(ctx, f.alice, mod, "bob", client); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.CanPublish(ctx, bob, mod); !isReject(err, http.StatusForbidden, "forbidden_module") {
		t.Errorf("CanPublish after removal: %v", err)
	}
}
