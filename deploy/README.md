# Deploying Gopherdex

What's in this directory:

| File | For |
|---|---|
| `docker-compose.yml` + `Caddyfile` | The simplest setup: the registry behind [Caddy](https://caddyserver.com), which gets HTTPS certificates by itself |
| `gopherdex.env.example` | Settings. Every `gopherdexd` flag can be set as `GOPHERDEX_<FLAG>` (`-base-url` → `GOPHERDEX_BASE_URL`) |
| `gopherdexd.service` | Running the binary under systemd instead of containers |
| `smoke-test.sh` | Checks a live deployment from the outside, including a real zero-setup `go get` |

## Requirements

- **A domain you control, served over HTTPS on port 443.** The domain is part of every module path (`<domain>/<owner>/<module>`), so choose it once. Changing it later breaks every import.
- **A small server with ports 80 and 443 open.** Caddy needs port 80 to get its certificate. One CPU and 1 GB of RAM is plenty to start; the data is one SQLite file plus the module zips.
- **An SMTP service** (Postmark, SES, Mailgun, SendGrid, your provider's…). Without one, nobody can verify their email, so nobody can publish, and password resets never arrive.

## Install with Docker Compose

```bash
# on the server
git clone https://github.com/codebled/gopherdex && cd gopherdex/deploy
cp gopherdex.env.example gopherdex.env && chmod 600 gopherdex.env
$EDITOR gopherdex.env           # domain, base URL, SMTP
docker compose up -d --build    # or drop --build once images are on ghcr.io
docker compose logs -f gopherdex
```

The start-up log line should say `zero_config_go_get=true notify_mirror=true smtp=true`. Any `level=WARN` line explains a setting that's still missing.

## Launch checklist

Work through it in order. Steps 5 and 6 are the first real test of zero-setup installs and trusted publishing, which the automated tests can only simulate.

1. **DNS:** an `A`/`AAAA` record for the domain points at the server. `dig +short gopherdex.example.com`
2. **HTTPS:** `https://<domain>/healthz` says `ok`, and `http://<domain>` redirects to https.
3. **Email:** register your own account and receive the verification email. Also try "Forgot your password?".
4. **Admin and 2FA:**
   - Turn on two-factor at `/account/security`.
   - Put your username in `GOPHERDEX_ADMINS`, then `docker compose up -d`.
   - `/admin` should open.
5. **Publish and install:**
   - Create an API token and publish a small test module:
     ```bash
     gopherdex login --registry https://<domain>
     gopherdex publish
     ```
   - From any other machine, with default Go settings:
     ```bash
     deploy/smoke-test.sh https://<domain> <domain>/<you>/<module>@v0.1.0
     ```
     This includes a real `go get` through `proxy.golang.org` and `sum.golang.org`. If that step fails right after publishing, wait a minute and retry, since the public mirror fetches new versions asynchronously.
6. **Trusted publishing:** add a trusted publisher on the test module's Manage tab. Then push a tag from a GitHub repository that uses the example workflow. The project page should show **Verified source** with a link to the run.
7. **Backups:**
   - Check that `/data/backups` fills up: `docker compose exec gopherdex ls /data/backups` won't work (distroless has no `ls`), so use `docker run --rm -v deploy_gopherdex-data:/d alpine ls /d/backups`.
   - Better: set up S3 storage and Litestream (below), so nothing important lives only on the machine. Otherwise copy the volume (backups plus `blobs/`) off the machine.
   - Restore once onto a scratch machine to prove it works.
8. **Monitoring:** point an uptime checker at `https://<domain>/healthz`. It returns 503 if the database is unreachable.
9. **Search engines:** submit `https://<domain>/sitemap.xml` to Google Search Console and Bing Webmaster Tools.

## S3-compatible storage (optional)

Out of the box, the database and module zips live in the `gopherdex-data` volume, backed up daily inside it. For a production registry, move both to an S3-compatible bucket (AWS S3, Cloudflare R2, Backblaze B2, MinIO…). Then losing the server loses nothing.

**Module zips.** Set `GOPHERDEX_BLOBS` and the credentials in `gopherdex.env` (see the example file), copy the zips already published, and restart:

```bash
docker compose run --rm gopherdex blobs copy -from /data/blobs -to "s3://my-bucket/zips?region=us-east-1"
docker compose up -d
```

Uploads use conditional writes (`If-None-Match: *`), so a published zip can never be overwritten. The server checks the bucket at start-up and refuses to start with wrong credentials. `blobs copy` skips zips already copied, so it's safe to run again.

**The database, with Litestream.** Fill in the `LITESTREAM_*` settings, then start the replication sidecar:

```bash
docker compose --profile litestream up -d
```

[Litestream](https://litestream.io) streams every database change to the bucket within about a second and keeps a week of history.

**Recovering on a new server.** Restore before the registry starts:

```bash
docker compose run --rm --no-deps --user 0 --entrypoint sh litestream -c \
  'litestream restore -if-replica-exists /data/gopherdex.db && rm -f /data/gopherdex.db.tmp-* && chown -R 65532:65532 /data'
docker compose --profile litestream up -d
```

`--no-deps` keeps the registry from starting first and creating an empty database. The `chown` gives the files to the registry's user; the image runs as non-root.

## Upgrading

```bash
docker compose pull && docker compose up -d      # or: git pull && docker compose up -d --build
```

Database migrations run automatically on start. Take a backup first (`gopherdexd backup`, or copy the latest file from `/data/backups`).

## Without containers

1. Build or download `gopherdexd` for the server:
   ```bash
   make dist
   ```
   Or use the binaries attached to a GitHub release.
2. Install `gopherdexd.service`.
3. Put Caddy (or nginx) in front with the same site block as `Caddyfile`, proxying to `127.0.0.1:8080`.
