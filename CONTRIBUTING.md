# Contributing to Gopherdex

Thanks for helping build Gopherdex. This guide covers everything from a first typo fix to a new feature. If something here is unclear, that's a bug too: open an issue.

## Before you start

- **Small fixes** (typos, docs, an obvious bug with a test): open a pull request directly.
- **Anything bigger** (a feature, a new dependency, a change to the database schema, the API or how publishing works): open an issue first and describe what you want to do. We'll agree on the approach before you spend time on code.
- **Security problems:** never in a public issue or pull request. See [SECURITY.md](SECURITY.md).
- New here? Issues labelled [`good first issue`](https://github.com/codebled/gopherdex/labels/good%20first%20issue) are small, self-contained and have a pointer to where to start. Comment on one to say you're taking it.

Everyone taking part follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## Contributor License Agreement

Before your first pull request can be merged, you sign the [Contributor License Agreement](CLA.md), once. A bot comments on your pull request with instructions: you reply with one sentence. The CLA confirms you have the right to contribute the code and lets the project keep its licensing options open; you keep the copyright to your contribution.

## Set up

You need [Go](https://go.dev/dl/) (the version in `go.mod` is downloaded automatically) and `make`. Nothing else: the linters are run at pinned versions through `go run`.

```sh
git clone https://github.com/<you>/gopherdex && cd gopherdex
make run-offline          # a local registry at http://localhost:8080
make check                # everything CI checks; run it before you push
```

The [README](README.md) explains how the pieces fit together, and the layout section says what lives where.

## What CI checks

Every pull request must pass all of these. `make check` runs the same checks locally:

| Check | Command | What it wants |
|---|---|---|
| Formatting | `make fmt` | `gofmt` and `goimports` formatting; imports grouped standard library, third party, then this module |
| Lint | `make lint` | [golangci-lint](.golangci.yml): no unchecked errors, errors wrapped with `%w`, SQL rows closed, no security findings, correct spelling, and a doc comment on every exported name |
| Text files | `make text` | No trailing spaces, tabs or spaces consistent with `.editorconfig`, and a final newline in every file |
| Modules | `make tidy` | `go.mod` and `go.sum` match `go mod tidy` |
| Tests | `make test` | All tests pass with the race detector |
| Vulnerabilities | `make vuln` | `govulncheck` finds nothing reachable |
| Migrations | (CI only) | Existing files in `internal/database/migrations` are never edited, renamed or deleted: add a new, higher-numbered file instead |

If the linter flags something that is genuinely fine, suppress that one line with a reason: `//nolint:gosec // the query is built from constant fragments only`. Bare `//nolint` without a linter name and a reason fails the lint.

## Writing code

- **Match the code around you.** Naming, comment density and error messages should read like the rest of the package.
- **Doc comments** start with the name they describe and are full sentences: `// Publish stores an uploaded version after checking it.` Explain what and why, not a restatement of the signature. Every package has a package comment.
- **Errors** are wrapped with context: `fmt.Errorf("publish %s: %w", mod, err)`. Messages users see are plain, specific and tell them what to do next.
- **Tests** accompany every change in behavior. Bug fixes include a test that fails without the fix. Prefer table tests and the existing helpers (`newFixture`, `newTestEnv`) over new frameworks.
- **The database:** schema changes are new migration files (`NNN_description.sql`); never edit one that has been released. Queries use `?` placeholders for every value.
- **Security-sensitive code** (accounts, sessions, tokens, uploads, permissions) gets a test for the attack, not only the happy path.
- **Dependencies:** avoid new ones. If one is needed, explain why in the pull request.
- **User-facing text:** US English, short sentences, no jargon.

## Commits and pull requests

- **One change per pull request.** Refactors go in their own pull request.
- **Commit messages:** a short summary line in the imperative ("Add team invitations", not "Added…"), under about 72 characters, then a blank line and the why, if it isn't obvious.
- **The pull request description** says what changed, why, and how you tested it. The template lists what reviewers look for.
- **Keep your branch up to date** with `main` by rebasing: `git fetch origin && git rebase origin/main`.
- Pull requests are merged by squashing, so the history on `main` has one commit per change.

## Review

A maintainer reviews every pull request. Expect questions: they're about the code, never about you. Reply to each comment, push fixes as new commits while in review, and mark conversations resolved once addressed. If a pull request goes quiet for a while, a friendly ping is welcome.

First-time contributors: GitHub asks a maintainer to approve CI runs for your first pull request, so checks may show as waiting until someone looks. That's normal.

## Releases

Maintainers tag releases (`vX.Y.Z`); the release workflow builds binaries and the container image. Contributors don't need to do anything.
