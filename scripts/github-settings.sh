#!/bin/sh
# github-settings.sh applies the repository settings an open-source project
# needs: protected main, pull-request reviews, required checks, labels for
# newcomers, private vulnerability reports and safe Actions defaults.
# Run it once as a repository admin (it uses your gh login). It's safe to
# run again: every step replaces the previous setting.
set -eu

repo=${1:-codebled/gopherdex}
echo "Configuring $repo"

# Pull requests are squash-merged; merged branches are deleted.
gh repo edit "$repo" --enable-squash-merge --enable-merge-commit=false --enable-rebase-merge=false \
	--delete-branch-on-merge --enable-auto-merge=false

# Private vulnerability reporting (SECURITY.md points here), Dependabot
# alerts, and Dependabot's automatic security fixes.
optional() { "$@" >/dev/null 2>&1 || echo "warning: couldn't apply: $*" >&2; }
optional gh api -X PUT "repos/$repo/private-vulnerability-reporting"
optional gh api -X PUT "repos/$repo/vulnerability-alerts"
optional gh api -X PUT "repos/$repo/automated-security-fixes"

# Actions: workflows get a read-only token unless they ask for more, can't
# approve pull requests, and first-time contributors' runs wait for a
# maintainer's approval.
optional gh api -X PUT "repos/$repo/actions/permissions/workflow" \
	-f default_workflow_permissions=read -F can_approve_pull_request_reviews=false
optional gh api -X PUT "repos/$repo/actions/permissions/fork-pr-contributor-approval" \
	-f approval_policy=first_time_contributors

# Labels for triage and newcomers.
label() { gh label create "$1" --repo "$repo" --color "$2" --description "$3" --force >/dev/null; }
label "good first issue" 7057ff "Small and self-contained: a good start for new contributors"
label "help wanted"      008672 "Maintainers would welcome a pull request"
label "needs-triage"     fbca04 "Not looked at by a maintainer yet"
label "bug"              d73a4a "Something doesn't work"
label "enhancement"      a2eeef "New feature or improvement"
label "security"         b60205 "Security hardening (report vulnerabilities privately: SECURITY.md)"
label "dependencies"     0366d6 "Dependency updates"

# main: no direct pushes, force-pushes or deletion. Changes arrive by pull
# request, with resolved conversations, a linear history, and every CI job
# passing on an up-to-date branch.
#
# No approving review is required while one person maintains the project:
# GitHub won't let anyone approve their own pull request, so the rule only
# forced the maintainer to tick "bypass rules" on every change. Merging
# still needs write access, so an outside contributor's pull request waits
# for a maintainer either way. When a second maintainer joins, restore
# "required_approving_review_count": 1 and "require_code_owner_review":
# true below, and drop the bypass_actors entry.
existing=$(gh api "repos/$repo/rulesets" --jq '.[] | select(.name == "main") | .id')
method=POST path="repos/$repo/rulesets"
if [ -n "$existing" ]; then method=PUT path="repos/$repo/rulesets/$existing"; fi
gh api -X "$method" "$path" --input - >/dev/null <<'JSON'
{
  "name": "main",
  "target": "branch",
  "enforcement": "active",
  "conditions": { "ref_name": { "include": ["~DEFAULT_BRANCH"], "exclude": [] } },
  "bypass_actors": [ { "actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "pull_request" } ],
  "rules": [
    { "type": "deletion" },
    { "type": "non_fast_forward" },
    { "type": "required_linear_history" },
    { "type": "pull_request", "parameters": {
        "required_approving_review_count": 0,
        "dismiss_stale_reviews_on_push": true,
        "require_code_owner_review": false,
        "require_last_push_approval": false,
        "required_review_thread_resolution": true,
        "allowed_merge_methods": ["squash"]
    } },
    { "type": "required_status_checks", "parameters": {
        "strict_required_status_checks_policy": true,
        "required_status_checks": [
          { "context": "Test" }, { "context": "Lint" }, { "context": "Checks" },
          { "context": "Migrations" }, { "context": "Docker build" }
        ]
    } }
  ]
}
JSON

echo "Done. Review it at https://github.com/$repo/settings/rules"
