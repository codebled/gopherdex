# Gopherdex

A package registry for Go modules, in the spirit of pypi.org.

- **Accounts:** register, confirm your email, and create API tokens. Your username is your namespace: `<module-host>/<username>/<module>`.
- **Publishing:** `gopherdex publish` builds a module zip from a git tag, checks it with the go command's own rules and uploads it. Versions are immutable.
- **Project pages:** every module has a page at `/<owner>/<module>`, like pypi.org/project/…, with the rendered README, docs, release history, downloadable files with checksums, license, owner and links.
- **Zero-setup installs:** on a public HTTPS domain, `go get gopherdex.dev/alice/retry` works with default Go settings. The registry answers the go command's `?go-get=1` lookup with a `go-import … mod` tag, and the public module mirror and checksum database pick new versions up automatically.
- **GOPROXY server:** serves hosted modules to the `go` command at `/api/proxy`.
- **Discovery:** full-text search with filters (license, Go version, last release, deprecated) and sorting (relevance, downloads, recently updated, newest), download counts and charts, and a home page with just-updated, most-downloaded and new modules. Public pkg.go.dev results follow in their own section.
- **Public modules:** modules that aren't hosted here come from `proxy.golang.org`, with search and docs from `pkg.go.dev`. Run with `-offline` to turn this off.

Roadmap: accounts (done) → publish from the CLI (done) → zero-setup `go get` (done; confirm on your domain) → project pages (done) → maintainer tools (done) → search and stats (done) → trust and operations (done) → trusted publishing from GitHub Actions (done) → launch readiness (done; see [deploy/README.md](deploy/README.md)) → accounts and feeds (done) → security advisories (done) → production storage: S3 and Litestream (done) → documentation and dependency graph (done).

## Run it

Two programs:

| Program | Who runs it |
|---|---|
| `gopherdexd` | The registry server |
| `gopherdex` | Module authors: `login`, `whoami`, `publish`, `yank`, `unyank`, `logout` |

```bash
go run ./cmd/gopherdexd
go install github.com/parthiban-sivakumar/gopherdex/cmd/gopherdex@latest   # the author CLI
```

Open http://localhost:8080 and choose **Register**. Without `-smtp-addr`, emails aren't sent. The verification link is printed in the server log instead.

| Flag | Default | |
|---|---|---|
| `-addr` | `localhost:8080` | Listen address |
| `-base-url` | `http://<addr>` | Public URL, used in email links and CLI output; `https` turns on Secure cookies |
| `-module-host` | host of `-base-url`, or `gopherdex.localhost` | Domain in module paths. Module paths can't have a port or a dot-less host, so local development uses `gopherdex.localhost/<user>/<module>` |
| `-db` | `data/gopherdex.db` | SQLite database (created and migrated on start) |
| `-blobs` | `data/blobs` | Published module zips: a directory, or an S3-compatible bucket, `s3://bucket/prefix?region=…&endpoint=…`, with credentials in `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`. `gopherdexd blobs copy -from … -to …` moves existing zips |
| `-max-upload` | 50 MiB | Largest module zip accepted |
| `-data` | (none) | Serve fixture modules from a directory too, e.g. `data/modules` for demos |
| `-smtp-addr` / `-smtp-from` / `-smtp-user` | | SMTP server for real emails; password in `$GOPHERDEX_SMTP_PASSWORD` |
| `-upstream` | `https://proxy.golang.org` | Public proxy for modules not hosted here |
| `-pkgsite` | `https://pkg.go.dev` | Public search and docs links |
| `-offline` | `false` | Hosted modules only, no outbound requests |
| `-trust-proxy` | `false` | Read client IPs from `X-Forwarded-For` (only behind your reverse proxy) |
| `-notify-mirror` | `true` | After each publish, ask `proxy.golang.org` and `sum.golang.org` to fetch the version. Skipped for local module hosts |
| `-sumdb` | `https://sum.golang.org` | Checksum database to notify |
| `-admins` | (none) | Comma-separated usernames allowed into the review queue at `/admin`; they must have two-factor authentication on |
| `-require-2fa` | `false` | Refuse uploads from accounts without two-factor authentication (PyPI requires it) |
| `-backup-dir` / `-backup-every` / `-backup-keep` | (off) / `24h` / `7` | Automatic database backups |
| `-trusted-publishing` | `true` | Let GitHub Actions workflows publish with OIDC ID tokens. Off with `-offline`, because it fetches GitHub's signing keys |
| `-oidc-audience` | module host | Audience GitHub ID tokens must be requested for |
| `-playground` | `https://play.golang.org` | Go Playground that documentation examples open in. Empty, or `-offline`, hides the Run buttons |
| `-vulndb` | `https://vuln.go.dev` | Public Go vulnerability database merged into `/vulndb` and used to flag vulnerable dependencies. Empty, or `-offline`, turns it off |
| `-v` | `false` | Debug logging |

Every flag can also come from the environment as `GOPHERDEX_<FLAG>` (`-base-url` → `GOPHERDEX_BASE_URL`); flags on the command line win. `gopherdexd version` prints the build, and `gopherdexd healthcheck` exits 0 when `/healthz` answers (for container health checks). With an `https` base URL, the server logs a warning for each missing production setting (SMTP, admins, backups, `-trust-proxy`).

## Publish a module

```bash
go install github.com/parthiban-sivakumar/gopherdex/cmd/gopherdex@latest   # or: make build

# go.mod
module gopherdex.localhost/alice/retry          # <module-host>/<your-username>/<name>

git tag v0.1.0
gopherdex login --registry http://localhost:8080   # paste a token from /account
gopherdex publish --dry-run                        # build and check, upload nothing
gopherdex publish
```

What `publish` checks before uploading:
- **Built from the tag:** the zip comes from the tagged commit (`golang.org/x/mod/zip.CreateFromVCS`), so uncommitted edits are never published. Modules in a subdirectory use tags like `sub/v1.2.3`.
- **Right namespace:** the module path must be under your namespace, and v2+ releases need the `/vN` suffix.
- **Same hash on both sides:** the CLI compares the `h1:` hash it computed with the one the registry stored.

The server checks everything again:
- **Token:** your token's namespace scope and a verified email.
- **Path and version:** module path rules, a full semver version (no pseudo-versions) and the major version suffix.
- **Zip contents:** `zip.CheckZip` rules, and a `go.mod` whose module line matches.
- **Size:** the upload limit.

Re-uploading identical content is a no-op, so CI retries are safe. Different content for an existing version gets `409`.

For CI, prefer **trusted publishing** (next section): nothing secret to store. Or skip `login` and set `GOPHERDEX_REGISTRY` and `GOPHERDEX_TOKEN` from a CI secret.

## Documentation, source and dependencies

Project pages document every package from the published zip, using `go/doc` and `go/doc/comment`, the same packages pkg.go.dev uses.

**Docs tab**
- **Contents:** the package overview, then constants, variables, functions, types, and methods, each with a source link (`retry.go:42`).
- **Doc comments:** headings, lists and code blocks render properly. `[Name]` and `[pkg.Name]` references become links: to anchors in the same module, to other modules hosted here, or to pkg.go.dev.
- **Deprecated symbols:** anything with a `Deprecated:` paragraph is marked.
- **Examples:** examples from `_test.go` files show with their expected output. Self-contained ones get a **Run in the Go Playground** button (`-playground`, off with `-offline`).
- **Finding things:** an index and a filter box ("Jump to a function, type or constant…") at the top.

**Source tab.** Every file in the version's zip, with line anchors (`?tab=source&file=retry.go#L42`). It's exactly what the go command downloads and verifies, not a link to a repository that may have changed.

**Dependencies tab**
- **Requires:** the version's requirements from go.mod, direct and indirect. Hosted modules link to their pages, others to pkg.go.dev.
- **Used by:** every module on the registry whose latest release requires this one. Dependents stuck on a version with a security advisory are flagged.
- **Sidebar:** shows the "Used by" count.
- **Older versions:** requirements of versions published earlier are indexed at start-up.

## Security advisories

Owners publish advisories for their modules from the module's **Security** tab. Like GitHub's security advisories and PyPI's vulnerability notices, each one has:
- A summary and details.
- Affected version ranges, meaning the first affected version and the version with the fix (several ranges if needed).
- Optionally, the affected packages and functions, CVE or GHSA IDs, links and credit.

Each gets an ID like `GDX-2026-0001` and a page at `/advisories/<id>`. After publishing:
- **Warnings:** affected versions show a warning on their project page and a **Vulnerable** tag in the release history.
- **Email:** every maintainer is emailed.
- **Edits:** owners can edit an advisory, for example to add the fixed version once it's out. They can also withdraw one published in error; it stays visible, marked withdrawn, and tools stop reporting it.
- **Listing and feed:** everything is listed at `/advisories`, with an Atom feed at `/feeds/advisories.atom`.

**govulncheck.** The registry serves the [Go vulnerability database format](https://go.dev/security/vuln/database) at `/vulndb`: its own advisories as OSV entries, merged with Go's public database (refreshed hourly). One command checks hosted and public modules:

```bash
govulncheck -db https://gopherdex.dev/vulndb ./...
```

govulncheck reports only vulnerabilities your code can reach. When an advisory names functions, it points to the call. Entries from the public database are redirected to vuln.go.dev.

**Dependencies.** A module's Security tab checks every module version it requires against the registry's advisories and the public database, and lists known vulnerabilities with their fixed versions.

## Trusted publishing from GitHub Actions

Like PyPI's trusted publishers: a GitHub Actions workflow publishes with no API token stored anywhere.

1. **Register the workflow.** On the module's **Manage** tab, add a trusted publisher: GitHub repository (`owner/name`), workflow file (`release.yml`), and optionally an environment. Before a module's first release, add it from **Publish from GitHub Actions** on your account page instead.
2. **Give the job `id-token: write`** and run `gopherdex publish`:

```yaml
on:
  push:
    tags: ["v*"]
permissions:
  contents: read
  id-token: write
jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: stable }
      - run: go install <where-this-source-lives>/cmd/gopherdex@latest
      - run: gopherdex publish --registry https://gopherdex.dev
```

With no token configured, `publish` sees it's running in GitHub Actions. It fetches the audience from `/api/oidc/audience` and asks GitHub for an ID token with that audience. It then trades the token at `/api/oidc/mint-token` for a registry token that expires after 15 minutes and can only upload that one module.

The registry checks, in order:
- **Signature:** the ID token is signed by `https://token.actions.githubusercontent.com` (RS256, keys from GitHub's discovery document, refreshed hourly).
- **Audience and time:** it was issued for this registry and hasn't expired.
- **Single use:** it hasn't been exchanged before (its `jti` is recorded).
- **Registered publisher:** its repository and workflow file (from `workflow_ref`) match a trusted publisher of the module, and so does the environment when the publisher names one. Reusable workflows called from another repository don't match.
- **Owner account:** the repository owner's GitHub account ID matches the one recorded on first use. If the owner is deleted and someone re-registers the name, their workflows can't publish.
- **Person behind it:** whoever added the publisher can still publish the module, and has two-factor authentication on if `-require-2fa` is set.

Afterwards:
- **Provenance:** each release it uploads records the verified repository, workflow, environment, ref, commit and run ID. Project pages show them under **Verified source**, and the release history links to the run.
- **Repository link:** the repository link on the page comes from GitHub's claims, not from what the uploader sent.
- **Email:** maintainers are emailed when a publisher is added, removed or used.
- **Removal:** removing a publisher revokes any tokens it has been issued.
- **Upload only:** minted tokens can only upload; they can't yank.
- **Environments:** tie the publisher to one, then protect that environment in GitHub with required reviewers, so every release needs a human approval.

Install it on a local development host, where the public mirror can't reach:

```bash
GOPROXY=http://localhost:8080/api/proxy GONOSUMDB=gopherdex.localhost go get gopherdex.localhost/alice/retry@latest
```

On a public HTTPS deployment, run just `go get gopherdex.dev/alice/retry@latest`. See **Deploy** below.

## How `go get` finds modules

For `go get gopherdex.dev/alice/retry/backoff`, with default settings (`GOPROXY=https://proxy.golang.org,direct`):

1. **Lookup.** `proxy.golang.org` requests `https://gopherdex.dev/alice/retry/backoff?go-get=1`.
2. **Answer.** Gopherdex finds the module that contains the package and replies with `<meta name="go-import" content="gopherdex.dev/alice/retry mod https://gopherdex.dev/api/proxy">`. A trailing `/vN` resolves to that major version's module when one is published.
3. **Download.** The mirror downloads through this registry's GOPROXY endpoints and caches the result. `sum.golang.org` records the `h1:` hash in its public log, so users get checksum verification without `GONOSUMDB`.
4. **Early fetch.** After each publish, Gopherdex requests the new version from the mirror and checksum database right away (`-notify-mirror`). The first user doesn't wait, and the hash is logged while the registry is known to serve it.

People who open `https://gopherdex.dev/alice/retry` in a browser get the project page, and a package path such as `…/retry/backoff` opens that package's docs.

Good to know:
- **Nothing can be deleted.** The public mirror caches versions permanently and the checksum log can't be edited. That's why versions here are immutable.
- **`@latest` can lag.** The mirror refreshes version lists on its own schedule, so `@latest` may show a new release a few minutes late. Asking for the exact version (`@v1.2.3`) works at once.
- **Skipping the public mirror:** users who can't or don't want to use it can set `GOPROXY=https://gopherdex.dev/api/proxy`, or `GOPROXY=direct`, and still use the checksum database.

## Deploy

Zero-setup installs need a real domain served over HTTPS on the standard port, with the module host equal to that domain. [deploy/README.md](deploy/README.md) has the Docker Compose + Caddy setup, a systemd unit, the environment file, a launch checklist, and `deploy/smoke-test.sh`, which checks a live deployment from outside, including a real `go get` with default Go settings.

```bash
cd deploy && cp gopherdex.env.example gopherdex.env   # domain, SMTP, admins
docker compose up -d --build
./smoke-test.sh https://gopherdex.example.com gopherdex.example.com/you/module@v0.1.0
```

Releases: pushing a `v*` tag runs `.github/workflows/release.yml`. It attaches `make dist` binaries (Linux, macOS and Windows; `SHA256SUMS`) to a GitHub release and pushes the image to `ghcr.io/parthiban-sivakumar/gopherdex`. `make docker` builds the image locally.

## Project pages

`/<owner>/<module>` is a module's home, modeled on pypi.org/project/…:

| Part | What it shows |
|---|---|
| **Header** | Name, major version, synopsis, install command, and either "Latest version" or a link to the newer release |
| **Banners** | Deprecation (from the latest go.mod) and a retraction notice when viewing a retracted version |
| **Description** | The README. Markdown is rendered with GitHub extensions. Raw HTML and `javascript:` links are removed, and outbound links get `rel="nofollow ugc"`. Relative links and images point into the repository at the release tag (GitHub, GitLab, Codeberg), or are unlinked when there's no known repository |
| **Documentation** | Every package's docs, generated from the zip with `go/doc`, plus the module's dependencies |
| **Release history** | Every version with its date, publisher, commit, and pre-release or retracted markers |
| **Download files** | The module zip and go.mod with SHA-256 and Go checksums, and the exact `go.sum` lines |
| **Sidebar** | License (SPDX IDs detected by `licensecheck`, the same scanner as pkg.go.dev), owner, publisher, Go version, dependency count, size, links, and other major versions |

Add `@version` to pin a page to a release, for example `/alice/retry@v1.0.0?tab=files`. Pages built from a version's zip are cached in memory, because published versions never change.

## Search and downloads

`/search` is both search and browse, and it works without JavaScript: filters and sorting are ordinary GET forms, and the script only applies them as soon as they change.

- **Index:** SQLite FTS5 over each module's path, name, one-line summary and README, taken from its latest installable version. Every word is a prefix match, so `retr` finds `retry`. Matches in the path and name rank above the summary, which ranks above the README. Typed operators and punctuation are treated as plain text.
- **Filters:** license (SPDX IDs detected with `licensecheck`), "works with Go 1.xx" (the module's `go` directive is that version or older), last release in the past 30 days or year, and hide deprecated.
- **Sorting:** relevance, downloads in the last 30 days, recently updated, or newest.
- **Keeping it current:** summary, README text, license and Go version are captured when a version is published. Yanking, restoring and deprecating update the index immediately. Versions published before search existed are filled in automatically when the server starts.

**Downloads** count module zips served by `/api/proxy`; go.mod and `.info` requests don't count.
- **Batching:** counts are kept in memory and written every 15 seconds (and on shutdown), per version per day.
- **Where they show:** project pages show the last day, week and month, a 30-day bar chart, and counts per version in the release history. Search results and the home page's "Most downloaded" list use the 30-day total.
- **Undercounting:** zips served from `proxy.golang.org`'s cache never reach this registry, so for modules installed through the public mirror the numbers undercount, as mirrors do for PyPI.

## Maintaining modules

Owners and maintainers see a **Manage** tab on the project page.

| Action | Who | What it does |
|---|---|---|
| **Yank / restore a release** | owners, maintainers | Hides the version from `@v/list` and `@latest`, so new installs skip it. Its `.info`, `.mod` and `.zip` stay downloadable, so builds that already use it keep working, as on PyPI. Also available as `gopherdex yank MODULE@VERSION --reason "…"` and `gopherdex unyank MODULE@VERSION` |
| **Deprecate** | owners | Shows a notice, and optionally a replacement module, on the project page. The go command only reports `// Deprecated:` comments in go.mod, so add one to your next release too |
| **Co-owners** | owners | Give another user the *owner* role (everything) or the *maintainer* role (publish and yank) |
| **Transfer** | owners | Add the new owner, then remove yourself. A module always keeps at least one owner |

A module path never changes, because every program that imports it depends on it. Moving a module to a new path means publishing it there and deprecating the old one with the new path as its replacement.

Yanking only affects this registry. `proxy.golang.org` has its own copy of version lists, so to warn users of the public mirror too, add `retract vX.Y.Z` to go.mod and publish a new version.

### Organizations

Create one on your account page. It gets its own namespace, for example `gopherdex.dev/acme/…`, and a page at `/acme`.

| Role | Can |
|---|---|
| **Owner** | Manage members, and act as owner of every module in the namespace |
| **Member** | Publish new modules under the namespace, and publish and yank existing ones |

Organizations and users share one namespace list, so a name can't be both.

## Trust and safety

| Feature | Details |
|---|---|
| **Password reset** | `/forgot-password` emails a single-use link that expires after an hour. The response is the same whether or not the email has an account. Resetting signs the account out everywhere. |
| **Two-factor login** | On `/account/security`, scan a QR code with any authenticator app (TOTP, RFC 6238, checked against the RFC's test vectors). Sign-in then asks for a code after the password. You get 10 one-time recovery codes. Codes can't be reused, and five wrong codes end the attempt. |
| **Account security page** | Change password (signs out other sessions), "Sign out other sessions", and a log of the last 30 account and publishing events with their IP addresses. |
| **Scoped API tokens** | A token can publish all your modules, or only one. Revoke and set expiry on the account page. |
| **Email alerts** | Every owner and maintainer is emailed when a version is published, with the publisher, token name and IP, so a leaked token is spotted quickly. Users are also emailed about password, two-factor and token changes. |
| **Reports** | Signed-in users can report a module (malware, typosquatting, spam, license, other) from its project page, up to 10 a day. |
| **Review queue** | Admins (`-admins`, with two-factor on) see open reports at `/admin`. They can dismiss a report or quarantine the module. |
| **Quarantine** | A quarantined module is hidden from its page (410), search, listings, the proxy and `go get`, and can't receive new versions. Its files are kept, releasing it restores everything, and its maintainers are emailed. |
| **Trusted publishing** | GitHub Actions publishes without stored tokens; each release records the verified run. See **Trusted publishing** above. |
| **Rate limits** | Sign-in (including 2FA codes), sign-up, password resets, verification emails, uploads, reports, search pages and the JSON API are all limited. The GOPROXY endpoints aren't, because the go command and the public mirror fetch many files at once. |

## Backups

For production, keep module zips in S3-compatible storage (`-blobs s3://bucket/prefix?region=…&endpoint=…`) and replicate the database continuously with Litestream. [deploy/README.md](deploy/README.md) has the setup and the recovery steps. The rest of this section covers the built-in local backups.

```bash
gopherdexd backup -db data/gopherdex.db -out /backups/gopherdex.db    # one-off, safe while the server runs
gopherdexd -backup-dir /backups -backup-every 24h -backup-keep 7 …    # automatic
```

Database backups use SQLite's `VACUUM INTO`, which makes a consistent copy of the live database. Published module zips live in the blob directory (`-blobs`). They're never modified once written, so copy that directory with any file backup tool (`rsync`, snapshots, object storage sync).

To restore, stop the server, put the database file and the blob directory back, and start it again.

## Accounts and API tokens

| Route | |
|---|---|
| `GET/POST /signup`, `GET/POST /login`, `POST /logout` | Sign up, sign in and sign out |
| `GET /verify-email?token=` | Confirm an email address (single use, expires after 24 hours) |
| `GET/POST /forgot-password`, `GET/POST /reset-password` | Password reset |
| `GET/POST /login/2fa` | Second sign-in step when two-factor authentication is on |
| `GET /account/security`, `POST /account/security/{2fa,2fa/disable,password,sessions}` | Two-factor setup, password change, sessions, security log |
| `GET/POST /report?module=` | Report a module |
| `GET /admin`, `POST /-/admin/{dismiss,quarantine,release}` | Review queue for admins |
| `GET /account` | Email status and API tokens |
| `POST /account/email` | Change email: needs the current password; the address changes only when the link sent to the new one is opened, and the old address is told |
| `POST /account/preferences` | Optional emails: new versions of your modules, and being given access to a module or organization. Security emails can't be turned off |
| `POST /account/delete` | Delete the account (password, 2FA code if on, and the username typed to confirm). Refused while you're an organization's only owner |
| `POST /account/tokens`, `POST /account/tokens/{id}/revoke` | Create or revoke a token (requires a verified email) |
| `GET /api/whoami` | `Authorization: Bearer gdx_…` returns the token's user, namespaces (own and organizations') and scope |
| `POST /api/yank` | Bearer token; JSON `{"module", "version", "reason", "yank": true\|false}` |
| `POST /-/yank`, `/-/unyank`, `/-/deprecate`, `/-/undeprecate`, `/-/collaborators[/remove]`, `/-/orgs`, `/-/orgs/members[/remove]` | Website forms behind the Manage tab, the account page and organization pages |
| `GET /api/oidc/audience` | The audience CI must request its ID token for |
| `POST /api/oidc/mint-token` | JSON `{"token": "<GitHub Actions ID token>", "module": "<path>"}` returns `{"token","expiresAt","module","publisher"}`: a 15-minute token for that module |
| `POST /-/publishers`, `/-/publishers/remove` | Add or remove trusted publishers (Manage tab, or the account page before a module's first release) |
| `POST /api/upload` | Bearer token; `multipart/form-data` with `module`, `version`, optional `repository`, `commit`, `ref`, then a `zip` file. `201` when published, `200` when identical content already exists, `4xx` with `{"error","code"}` otherwise |

**Deleting an account:**
- **What's removed:** the account, its sessions, API tokens and trusted publishers, and its roles on modules and organizations.
- **What stays:** published modules. Versions are immutable and other programs depend on them, so co-maintainers keep managing them.
- **The username:** stays reserved forever, so nobody can publish under the old module paths.
- **The audit log:** keeps the events, with the username recorded in the deletion entry.

Security notes:
- **Passwords:** hashed with argon2id. Two-factor secrets are stored as-is, so protect database backups like the live database.
- **Secrets:** sessions, verification links and API tokens are 32 random bytes each, and only their SHA-256 hash is stored. Tokens start with `gdx_`, so secret scanners can recognize leaked ones.
- **Cookies:** the session cookie is HttpOnly and SameSite=Lax, and also Secure when served over HTTPS.
- **Form protection:** browser form posts from other sites are rejected by `http.CrossOriginProtection`. Sign-in, sign-up and email resends are rate limited per IP or account.
- **Audit log:** account creation, sign-ins (including failures), email verification and token changes are recorded in `audit_log`.
- **Rate limits and proxies:** limits live in memory, so they're per process. Behind a reverse proxy, start the server with `-trust-proxy` so limits apply per client instead of to the proxy's IP.

## Routes

| Route | Returns |
|---|---|
| `GET /` | Home: search box, registry numbers, just updated, most downloaded and new modules |
| `GET /search?q=&license=&go=&updated=&sort=&hide_deprecated=&page=` | Search and browse; `/modules` redirects here |
| `GET /<owner>` | A user's or organization's modules; organizations also list members |
| `GET /<owner>/<module>[/vN][@version]` | Project page; `?tab=docs`, `?tab=versions` or `?tab=files` |
| `GET /<owner>/<module>/<package>` | Redirect to that package on the docs tab |
| `GET /<owner>/<module>[/…]?go-get=1` | `go-import` tag for the go command |
| `GET /api/search?q=&scope=` | Search results (JSON). `scope=hosted` uses the registry's search index; `scope=public` is pkg.go.dev only |
| `GET /api/modules/{module}?version=` | Release history, go.mod and docs (JSON) |
| `GET /api/proxy/{module}/@v/list` | Tagged versions, one per line (`text/plain`) |
| `GET /api/proxy/{module}/@v/{version}.info` | `{"Version","Time","Origin":{"VCS","URL","Hash","Ref"}}` (`application/json`) |
| `GET /api/proxy/{module}/@v/{version}.mod` | go.mod (`text/plain`) |
| `GET /api/proxy/{module}/@v/{version}.zip` | Module zip, streamed (`application/zip`) |
| `GET /api/proxy/{module}/@latest` | Info of the latest version (`application/json`) |
| `GET /tokens.css` | Design tokens generated from `internal/tokens` |
| `GET /healthz` | `ok`, or `503` when the database is unreachable |
| `GET /feeds/releases.atom`, `/feeds/new.atom` | Atom feeds of new releases and new modules on the whole registry |
| `GET /feeds/<owner>.atom`, `/feeds/<owner>/<module>[/vN].atom` | New releases by one user or organization, or of one module. Home, owner and project pages link their feed in `<head>` for feed readers |
| `GET /advisories`, `GET /advisories/<id>` | Security advisories; owners edit or withdraw on the advisory page |
| `POST /-/advisories`, `/-/advisories/<id>`, `/-/advisories/<id>/withdraw` | Publish (from the Security tab), edit and withdraw advisories |
| `GET /vulndb/index/{db,modules,vulns}.json[.gz]`, `GET /vulndb/ID/<id>.json[.gz]` | Go vulnerability database for `govulncheck -db`: the registry's advisories plus the public database |
| `GET /feeds/advisories.atom` | New and updated advisories |
| `GET /robots.txt`, `GET /sitemap.xml` | Crawler rules (accounts, forms and the API are off limits) and every project page with its last release date |

Module paths and versions in proxy URLs are case-escaped (`github.com/Azure/x` → `github.com/!azure/x`). Missing modules return 404, so the `go` command moves on to the next proxy in `GOPROXY`.

Proxy routes are registered as one `GET /api/proxy/` pattern and parsed in `internal/goproxy`. Go's router can't do this: `{module}` matches only one path segment, but module paths contain slashes, and a pattern like `{version}.info` isn't allowed.

## Fixture modules (the -data directory)

For tests and demos, modules can also be served straight from a directory. Published modules live in the database and blob store instead.

```
data/modules/<module>/@v/<version>/        source tree: go.mod, *.go, LICENSE, README.md
data/modules/<module>/@v/<version>.json    optional: {"Time": "...", "Origin": {"VCS","URL","Hash","Ref"}}
```

- **Path escaping:** module paths and versions are stored in escaped form, as in proxy URLs.
- **Missing metadata:** without a `.json` file, the release time is the directory's modification time.
- **Treat versions as immutable:** once a version is published, don't edit it. The `go` command records its checksum in `go.sum`.
- **Retractions and deprecation:** add `retract` or a `// Deprecated:` comment to the latest version's `go.mod`.

Zips follow the module zip rules: files go under `<module>@<version>/`, and VCS directories, `vendor/`, nested modules, symlinks and files over the size limits are left out. This is a simplified version of `golang.org/x/mod/zip`. For private modules the result only has to be consistent. If you mirror a public module, check that its `h1:` hash matches `sum.golang.org`.

## Layout

```
cmd/gopherdexd/       registry server: flags, wiring, graceful shutdown
cmd/gopherdex/        author CLI: login, whoami, publish, logout
internal/cli/         CLI implementation (git tags → module zip → upload)
internal/registry/    publish validation, immutable versions, serving published modules, import path lookup,
                      roles, yanking, deprecation, organizations, full-text search, download counts
internal/version/     build version (release tag, go install version, or devel+commit)
internal/vulndb/      Go vulnerability database format (OSV), version ranges, public database cache and merge
internal/oidc/        verifies GitHub Actions OIDC ID tokens (RS256, discovery, key rotation) for trusted publishing
internal/mirror/      warms proxy.golang.org and sum.golang.org after each publish
internal/license/     SPDX license detection shared by publishing and project pages
internal/project/     project pages: safe README rendering (goldmark), license detection (licensecheck), docs, files
internal/accounts/    users, namespaces, sessions, email verification, API tokens
internal/database/    SQLite (modernc.org/sqlite, no cgo) and embedded migrations
internal/blob/        write-once, content-addressed storage for published zips
internal/mail/        email: log in development, SMTP in production
internal/ratelimit/   per-IP limits for sign-in and sign-up
internal/module/      path/version validation, case escaping, semver ordering
internal/gomod/       small go.mod parser (require, retract, deprecation)
internal/store/       on-disk module store (os.Root) and streaming zips
internal/goproxy/     GOPROXY handler and client
internal/godoc/       package docs from source via go/parser + go/doc
internal/discovery/   search, release history, docs; hosted first, then public
internal/server/      routes, JSON API, middleware
internal/tokens/      Go brand color tokens → /tokens.css, contrast tests
deploy/               Docker Compose + Caddy, systemd unit, env file, launch checklist, smoke test
.github/workflows/    CI (gofmt, vet, race tests, image build) and tag releases (binaries + ghcr.io image)
web/                  embedded templates (layout + pages), CSS, JS, icons
data/modules/         sample module example.com/hello (v1.0.0 retracted, v1.1.0, v1.2.0-beta.1)
```

```bash
make test   # go test ./...
make build  # bin/gopherdexd and bin/gopherdex
```

## License

[MIT](LICENSE)
