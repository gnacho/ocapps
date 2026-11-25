# Deploying and migrating `ocapps`

Guide to install the unified backend (news + notes + photos) and migrate from
the three old services (`ocnews`, `ocnotes`, `ocphotos`).
Reference: [`docs/SPEC.md`](../docs/SPEC.md) §5.4, §8 (H6) and §9.

## What is in this directory

| File | What it is |
|---|---|
| [`opencloud-apps.service`](opencloud-apps.service) | Single systemd unit (SPEC §9.1) |
| [`env.example`](env.example) | Template for `/etc/ocapps/env` with every variable and its legacy name (SPEC §9.2/§3.2) |
| [`proxy/nginx.conf`](proxy/nginx.conf) · [`proxy/Caddyfile`](proxy/Caddyfile) · [`proxy/traefik.yml`](proxy/traefik.yml) | Proxy snippets (SPEC §9.3) |
| [`migrate.sh`](migrate.sh) | 3-to-1 data migration, idempotent, with `--dry-run` and `--force-redeploy` (SPEC §5.4) |
| [`extensions-updater.sh`](extensions-updater.sh) | Installs, updates and rolls back the web extensions from their GitHub releases (`list`, `check`, `install`, `update`, `rollback`) |
| [`extensions.conf.example`](extensions.conf.example) | Template for `/etc/ocapps/extensions.conf` (app, repo and asset pattern) |
| [`docker-compose.example.yml`](docker-compose.example.yml) | `ocapps` service for the OpenCloud compose project (§[Docker Compose deployment](#docker-compose-deployment)) |
| [`ocapps-ext-update.service`](ocapps-ext-update.service) · [`ocapps-ext-update.timer`](ocapps-ext-update.timer) | Optional weekly update |
| [`repo-notices/`](repo-notices/) | Draft notices for the app repos' READMEs |

## Web extensions: install and update without doing it by hand

```sh
install -m 755 deploy/extensions-updater.sh /usr/local/sbin/ocapps-ext
install -d /etc/ocapps
install -m 644 deploy/extensions.conf.example /etc/ocapps/extensions.conf

ocapps-ext check                 # what is installed and what is published
ocapps-ext install news          # add an app that is not there yet
ocapps-ext update --dry-run      # dry run
ocapps-ext update                # download, replace, restart and verify
ocapps-ext rollback notes        # go back to the previous copy
```

Rules it enforces:

- Every installed app carries an `.ocapps-release` stamp with `release:<tag>` or
  `local:<date>`. The updater does **not** touch a local build unless `--force`
  is given, so a `dist` built by hand is not overwritten by the latest release.
- Before replacing, it copies the folder to `/var/backups/ocapps-ext/<date>/`
  (the last 5 are kept).
- It restarts the service **once** for all the changed apps and verifies that
  `/config.json` lists them and that their `remoteEntry` responds. If anything
  fails, it restores the copies, restarts and exits with code 3.
- It checks that the service has `WEB_ASSET_APPS_PATH` pointing at the apps
  directory, since without that variable the host does not list the extensions.
- `install` adds the entry to `/etc/opencloud/apps.yaml` if it is missing.

The weekly timer (`ocapps-ext-update.timer`) is optional and ships disabled:

```sh
install -m 644 deploy/ocapps-ext-update.service deploy/ocapps-ext-update.timer /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now ocapps-ext-update.timer
```

## Extension panel inside the app

`ocapps` can serve a panel at `/ocapps-admin/` to install and update the
extensions without going in over SSH. It is disabled unless a token is
configured:

```sh
# 1. token (kept in the file, never on the shell command line)
head -c 32 /dev/urandom | base64 | tr -d '\n=+/' | cut -c1-40 > /etc/ocapps/admin-token
chmod 600 /etc/ocapps/admin-token
printf 'OCAPPS_ADMIN_TOKEN=%s\n' "$(cat /etc/ocapps/admin-token)" >> /etc/ocapps/env

# 2. the service runs as user ocapps: it needs to be able to call the helper
cat > /etc/sudoers.d/ocapps-ext <<'EOF'
ocapps ALL=(root) NOPASSWD: /usr/local/sbin/ocapps-ext
EOF
chmod 440 /etc/sudoers.d/ocapps-ext
```

With `NoNewPrivileges=true` sudo does not work, so the unit sets it to `false`
(`install.sh` already leaves it that way). The rest of the hardening does not
change: the process still runs with `ProtectSystem=strict` and only writes to
`/var/lib/ocapps`.

What it exposes:

| Route | What it does |
|---|---|
| `GET /ocapps-admin/` | HTML panel, no server-side state |
| `GET /ocapps-admin/api/extensions` | Inventory: app, installed stamp, latest release, status |
| `POST /ocapps-admin/api/extensions/{id}/update` | Runs `ocapps-ext update <id>` (`?force=1` also replaces a local build) |

Security rules enforced by the code, not by documentation:

- **Without `OCAPPS_ADMIN_TOKEN` the routes are not registered**: a config
  mistake leaves the panel nonexistent, not open.
- The token comparison is constant time and goes in the `X-Ocapps-Admin-Token`
  header.
- The `{id}` must exist in `extensions.conf`, so no arguments can be passed to
  the helper, and the HTTP surface cannot be used to run anything else.
- Only what the configured repos publish can be installed, and the helper
  validates the asset pattern plus `manifest.json` and `entrypoint` before
  replacing anything.

After restarting `opencloud`, the panel is reachable at
`https://<domain>/ocapps-admin/` (it needs its route in the proxy towards
`127.0.0.1:8096`).


## Requirements

- **Runtime**: Linux with systemd, `sqlite3` (CLI, for the migration
  verifications), `curl` (smoke checks) and a reverse proxy (nginx, Caddy or
  Traefik).
  The binary is static (`CGO_ENABLED=0`, modernc.org/sqlite), so it needs no
  libc and no other system library.
- **ffmpeg is the ONLY system dependency at runtime** (SPEC Q4): video posters
  in the photos module (HLS was removed in Q7). Without ffmpeg the service still
  starts and photos logs it as degraded (`ffmpeg not found: video posters
  degraded (the original will be served)`), but install it:
  `apt install ffmpeg` / `apk add ffmpeg`.
- **Go only to compile** (>= 1.26, see `go.mod`). It is not needed on the
  production host if you use the release artifact or the Docker image.
- For the migration: root access on the host where the three old services run.

## Build

### Local binary

```bash
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --tags --always) \
            -X main.commit=$(git rev-parse --short HEAD) \
            -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o ocapps ./cmd/ocapps
```

### Release (goreleaser)

The repo ships `.goreleaser.yaml` (linux/amd64+arm64, `CGO_ENABLED=0`, version
ldflags). `goreleaser release` produces the tarballs plus checksums.

### Docker

```bash
docker build -t ocapps .          # runtime stage: alpine + ffmpeg (SPEC Q4/Q10)
```

Docker Compose deployment, web extension and proxy routes:
[§Docker Compose deployment](#docker-compose-deployment).

## Docker Compose deployment

For an OpenCloud instance set up with the official compose project
([`opencloud-eu/opencloud-compose`](https://github.com/opencloud-eu/opencloud-compose)).
`install.sh`, `opencloud-apps.service` and `ocapps-ext` belong to the systemd
path and are not used here: with Docker the service, the extensions folder and
the proxy route come from the compose project and from the proxy you already
have in front.

### 1. Image

```bash
docker build -t ocapps https://github.com/gnacho/ocapps.git
```

The image ships `ffmpeg` (photos video posters) and runs as uid 10001.
`/var/lib/ocapps` is created and owned by that user inside the image, so a named
volume works with no permission fiddling. If you use a bind mount, chown it
yourself: `sudo chown 10001 ./ocapps-data`.

### 2. Service

Copy [`docker-compose.example.yml`](docker-compose.example.yml) into the
OpenCloud compose directory and bring it up as an overlay, so both containers
share `opencloud-net`. Do not use it as a separate project: `ocapps` resolves
`opencloud` through the DNS of that network.

```bash
docker compose -f docker-compose.yml -f docker-compose.ocapps.yml up -d
```

Port `8096` is published on `127.0.0.1` only, because only the host proxy needs
it. If another service already owns that port on the host (Jellyfin uses 8096
too), publish another one instead: the container keeps listening on 8096 inside,
so only the left side of the mapping changes.

```yaml
ports:
  - "127.0.0.1:18096:8096"   # host 18096 -> container 8096
```

Nothing else needs editing: the `healthcheck` and the proxy routes point at the
**container** port. Only the proxy upstream follows the host side
(`proxy_pass http://127.0.0.1:18096;`). Same rule with `--listen` when running
from a release with `install.sh`.

`OCAPPS_OPENCLOUD_URL=http://opencloud:9200` points at the internal API of
the OpenCloud container, so there is no need to publish 9200. Enable only the
modules you are going to use. `photos` additionally needs an app token
(`OCAPPS_PHOTOS_USERS`).

### 3. Web extension

The official compose project mounts `./config/opencloud/apps` at
`/var/lib/opencloud/web/assets/apps`, which is the default
`WEB_ASSET_APPS_PATH`. If you changed `OC_APPS_DIR`, use that folder instead.
There is no installer: you unzip the release into the host.

```bash
curl -fsSLO https://github.com/gnacho/ocnotes/releases/latest/download/notes-0.1.7.zip
unzip -o notes-0.1.7.zip -d config/opencloud/apps
```

Add it to `config/opencloud/apps.yaml`, which is the file the compose project
mounts:

```yaml
notes:
  config: {}
```

And restart the container: OpenCloud reads the app list on start.

```bash
docker compose restart opencloud
```

### 4. Proxy

The routes go to the published port, with no trailing slash in `proxy_pass`
(with a slash the path gets rewritten and the backend does not recognise it):

```nginx
location /index.php/apps/notes/ {
    proxy_pass http://127.0.0.1:8096;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
}

location = /ocs/v2.php/cloud/capabilities {
    proxy_pass http://127.0.0.1:8096;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

The other apps use `/index.php/apps/news/` and `/ocphotos-api/`. The full block
is in [`proxy/nginx.conf`](proxy/nginx.conf), with equivalents for Caddy and
Traefik. The `/ocapps-admin/` panel belongs to the systemd path, since it runs
`ocapps-ext` with sudo inside the host, so leave it unconfigured under Docker
(do not define `OCAPPS_ADMIN_TOKEN`).

### 5. Verification

```bash
docker compose exec ocapps wget -qO- http://127.0.0.1:8096/healthz
docker compose logs --tail=50 ocapps
```

`/healthz` always answers 200 with the per-module detail, while `/readyz`
returns 503 if an enabled module is failing. If the backend answers but the web
app does not, the proxy access log shows the exact URL the browser requested.

## Installation (binary + systemd)

```bash
# 1. Service user
useradd --system --home /var/lib/ocapps --shell /usr/sbin/nologin ocapps

# 2. Binary
install -m 755 ocapps /usr/local/bin/ocapps

# 3. Configuration
install -d -m 750 -o root -g ocapps /etc/ocapps
install -m 640 -o root -g ocapps deploy/env.example /etc/ocapps/env
$EDITOR /etc/ocapps/env        # fill in OCAPPS_OPENCLOUD_URL, PHOTOS_USER, APP_TOKEN...

# 4. Unit
install -m 644 deploy/opencloud-apps.service /etc/systemd/system/
systemctl daemon-reload
# Do NOT start it yet if you are going to migrate data: migrate.sh does it (step 6).
```

`/var/lib/ocapps` is created by systemd itself (`StateDirectory=ocapps`,
0700). The data layout is (SPEC §5.1):

```
/var/lib/ocapps/
├── news/     ocnews.db  (+ favicons/, imgcache/, imgsecret, feedsecret)
├── notes/    notes.db   (+ attachments/, imgcache/, imgsecret)
└── photos/   memories.db(+ thumbs/, mediasecret)
```

## Proxy change

> **Warning: the `/ocphotos-api/` strip goes away.**
>
> Today the proxy strips `/ocphotos-api/` (ocphotos sees `/api/...`). In the
> unified binary `/api/` belongs to news (`/api/me`, `/api/users`) and photos
> registers its routes as `/ocphotos-api/api/...`. The proxy must pass the
> prefix **intact** to the single upstream `127.0.0.1:8096` (SPEC §4.3/§9.3).
> This is **THE proxy change of this migration** and the only step the binary
> itself cannot revert: coordinate it with the systemd cutover (apply the new
> snippet at the same time `migrate.sh` enables `opencloud-apps`, or right
> after).

Apply [`proxy/nginx.conf`](proxy/nginx.conf), [`proxy/Caddyfile`](proxy/Caddyfile)
or [`proxy/traefik.yml`](proxy/traefik.yml) in the vhost that serves the
OpenCloud instance. Summary:

| Route | Before | Now |
|---|---|---|
| `/index.php/apps/news/` | -> ocnews :8094 (no strip) | -> `127.0.0.1:8096` (no strip) |
| `/index.php/apps/notes/` | -> ocnotes :8100 (no strip) | -> `127.0.0.1:8096` (no strip) |
| `/ocs/` | -> ocnotes :8100 | -> `127.0.0.1:8096` |
| `/api/me`, `/api/users` | -> ocnews :8094 | -> `127.0.0.1:8096` (exact patterns, **not** a `/api/` catch-all) |
| `/ocphotos-api/` | -> ocphotos :8097 **with strip** | -> `127.0.0.1:8096` **WITHOUT strip** |

Both snippets preserve `Authorization` and `Host`, and
`/ocphotos-api/api/video/` gets long timeouts and unbuffered streaming (Range
requests). After reloading the proxy, validate with `nginx -t` /
`caddy validate` before the `reload`.

The live update channel (`/ocphotos-api/api/stream`, SSE) also needs
**buffering disabled**: in nginx, its own `location` with `proxy_buffering off`
(the snippets bring it). In Traefik the service's
`responseForwarding.flushInterval: 1ms` is enough, and in Caddy,
`flush_interval -1`. A buffered stream breaks nothing, but the notices arrive
late or not at all and the clients stop updating themselves.

The same routes serve the **Android clients** of the three apps
([ocnews-android](https://github.com/gnacho/ocnews-android),
[ocnotes-android](https://github.com/gnacho/ocnotes-android),
[ocphotos-android](https://github.com/gnacho/ocphotos-android)): they come in
through the same origin as the browser with Basic user:app-token. If an app
works on the web but not on mobile, the suspects are `Authorization` (they carry
no cookies), a redirect to the login page in front of the route, or a WAF
cutting in after several 401s.

## Multi-user in photos (H8)

Photos goes from single-tenant (a single user, the one of the app token) to
**multi-tenant**: any OpenCloud user who opens the photos app gets their own
index, albums, tags, places and thumbnails, scoped to their `oc_id` in the same
`memories.db`. Key points for the operator:

- **Indexing driven by activity, progressive.** There is no global scan at
  startup: the first authenticated request from a user triggers THEIR scan
  (their personal space is resolved lazily through Graph with their own
  credential) and their index grows incrementally. Until then their responses
  are coherently empty (0 photos, no errors).
- **`OCAPPS_PHOTOS_USERS` for background scan.** If you want the photos of
  certain users to be indexed even when they do not open the app (periodic scan
  every `OCAPPS_PHOTOS_SCAN_EVERY`), give them an app token:
  `OCAPPS_PHOTOS_USERS="alice:apptoken1,bob:apptoken2"`. The legacy pair
  `OCAPPS_PHOTOS_USER`+`OCAPPS_PHOTOS_APP_TOKEN` still works and folds into that
  list with a deprecation WARN.
- **`OCAPPS_PHOTOS_TOKEN` (static Bearer) is DEPRECATED.** It only keeps working
  if `OCAPPS_PHOTOS_USER` is configured (it maps to that user's oc_id).
  Configuring it without `USER` leaves the module failed (config error). Migrate
  machine-to-machine clients to the OIDC Bearer.
- **Migration from the single-tenant deployment (backfill).** On the first
  multi-tenant start, migration 002 adds `owner` to `assets`/`albums`, leaving
  the old rows with `owner=''`. If `OCAPPS_PHOTOS_USER`+`OCAPPS_PHOTOS_APP_TOKEN`
  are configured, the module resolves that user's oc_id and adopts the rows
  automatically (INFO log "backfill multi-owner"). **If you do not configure
  them**, the module still starts, but those old photos stay invisible to
  everyone (WARN log with the count: "there are N assets from the single-tenant
  era without an owner..."). Configure the pair once and restart to adopt them,
  then you can remove it if you do not want a background scan.
  Final `user_version` of `memories.db`: **2**.
- **Privacy.** Session credentials (OIDC tokens) and the resolved spaces live
  ONLY in memory (never on disk), and web sessions idle for more than 24h are
  purged. The on-disk thumbnails and the geocoding cache hold no personal data
  tied to another user (the cache key includes the owner's space UUID).
- **The module no longer depends on OpenCloud to start**: `Healthy()` is a ping
  to its SQLite, and with no `PHOTOS_USERS` configured there is no mandatory
  credential at all.

## Behaviour changes compared with the separate backends

Beyond the routing (see §Proxy change), the unified service changes three
details observable by clients and proxies. Review them in staging before the
cutover:

1. **The photos 401 is now JSON with `WWW-Authenticate`.** ocphotos answered
   `401 Unauthorized` as plain text, while ocapps answers
   `401 {"error":{"code":"unauthorized","message":"..."}}`
   (`Content-Type: application/json`) with the header
   `WWW-Authenticate: Bearer realm="ocphotos"`. Clients that parsed the plain
   text body must move to JSON, and those that only look at the status are not
   affected.
2. **Photos no longer has an open LAN mode without auth.** The whole
   `/ocphotos-api/` namespace requires credentials (OpenCloud session Bearer
   validated against Graph, or the deprecated static `OCAPPS_PHOTOS_TOKEN`). The
   only exemptions are the CORS preflight and the signed video stream
   (`/ocphotos-api/api/video/`). A deployment that relied on open access from
   the LAN stops working: those clients need credentials.
3. **`/ocs/v2.php/cloud/user` only serves JSON when asked explicitly**:
   `?format=json` or an `Accept: application/json` header (the standard OCS
   heuristic of ocnotes, which is the one serving the endpoint now). The stub
   news carried always served JSON, with no negotiation. If your proxy or some
   client called the news stub without those headers and expected JSON, add
   `?format=json` or the `Accept` header, and **verify it in the staging proxy
   config** before the cutover.
4. **`/ocs/v2.php/cloud/capabilities` merges the OpenCloud core.** The notes
   backend answered only with the `notes` block, while ocapps forwards the
   request to the core (`OCAPPS_OPENCLOUD_URL`, with the client's credential)
   and injects the `notes` block, so clients can read `core.status` (server
   version). If the core is not configured, does not answer or returns a code
   other than 200, it answers with `notes` alone (same behaviour as the separate
   backend). If a client reports that the server is old or unsupported, check
   this endpoint.

## Migration from the 3 old services

`deploy/migrate.sh` implements the SPEC §5.4 procedure step by step (`==>`
messages on every step, `set -euo pipefail`, aborts on any failed check).
Principles: **copy, never move**, the source databases and data stay untouched
for rollback, and it is idempotent.

**Always rehearse first with `--dry-run`** (it prints every action without doing
anything):

```bash
sudo deploy/migrate.sh --dry-run
sudo deploy/migrate.sh
```

Steps it runs:

0. **Pre-flight**: checks `sqlite3`/`systemctl`/`curl` and root, and **aborts if
   `opencloud-apps` is active**, because its databases are live and copying the
   old ones on top would corrupt them. To redeploy on purpose (for instance to
   repeat the migration after a rollback) use `--force-redeploy`: the script
   stops the unit, copies and restarts it in step 6.
0b. **Stops the old services** (`ocnews ocnotes ocphotos`) and waits for them to
   become inactive.
1. **Verifies the source**: `PRAGMA integrity_check` on the three live databases
   (aborts if any does not return `ok`) and `PRAGMA wal_checkpoint(TRUNCATE)` so
   the copy is self-contained even if `-wal`/`-shm` files were left behind.
2. **Copies the databases** to the new layout with `cp -a` (including
   `-wal`/`-shm` if they existed). Never `mv`.
3. **Copies the auxiliary data** per module (favicons, imgcache, imgsecret,
   feedsecret, attachments, thumbs, mediasecret) and sets ownership to
   `ocapps:ocapps`.
4. **Verifies the destination**: `integrity_check` on the three copied
   databases.
5. **Version audit**: records `PRAGMA user_version`, expected **18** (news) and
   **2** (notes). Photos arrives unversioned (0) and ocapps applies the
   **baseline (1) + 002 multi-owner -> 2** on the first start (SPEC §5.3), with
   the single-tenant index backfill if legacy credentials exist (see
   §Multi-user in photos).
6. **systemd**: `disable` the old units (**without deleting them**) and
   `enable --now opencloud-apps`.
7. **Smoke checks** over localhost: `/healthz` (the 3 modules `ok`) and
   `/readyz` (200).

The source paths are configurable with variables at the top of the script or
through the environment (defaults `/var/lib/ocnews`, `/var/lib/ocnotes`,
`/var/lib/ocphotos`, confirm them against your real deployment, SPEC Annex
B.2):

```bash
sudo OCNEWS_DIR=/srv/ocnews OCAPPS_DATA_DIR=/var/lib/ocapps deploy/migrate.sh --dry-run
```

Notes:

- The notes migration 002 (owner backfill) is already applied in the live
  database, so `OCAPPS_NOTES_OWNER` is only needed if rows without an owner were
  left.
- The first ocapps start applies: news/notes no-op, photos baseline. Review it
  with `journalctl -u opencloud-apps -n 100`.

## Verification (checklist SPEC §9.4)

```bash
systemctl is-active opencloud-apps                       # active
systemctl is-enabled ocnews ocnotes ocphotos             # disabled (x3)
journalctl -u opencloud-apps -n 50 --no-pager            # no unexpected "legacy env" and no failed modules
curl -s localhost:8096/healthz | jq .                    # all 3 modules "ok"
curl -s -o /dev/null -w '%{http_code}\n' localhost:8096/readyz   # 200

# Contracts (internal, over localhost, with valid credentials):
curl -su user:app-token localhost:8096/index.php/apps/news/api/v1-3/version
curl -su user:app-token localhost:8096/index.php/apps/notes/api/v1/notes
curl -s  -H "Authorization: Bearer $TOK" localhost:8096/ocs/v2.php/cloud/capabilities
curl -s  -H "Authorization: Bearer $TOK" localhost:8096/ocphotos-api/api/stats

# Database integrity:
for f in /var/lib/ocapps/*/*.db; do sqlite3 "$f" 'PRAGMA integrity_check;'; done   # ok x3
sqlite3 /var/lib/ocapps/news/ocnews.db 'PRAGMA user_version;'      # 18
sqlite3 /var/lib/ocapps/notes/notes.db 'PRAGMA user_version;'      # 2
sqlite3 /var/lib/ocapps/photos/memories.db 'PRAGMA user_version;'  # 2 (baseline + 002 multi-owner)

# From the outside (through the proxy, same origin as the web app, validates the proxy change):
curl -s  -H "Authorization: Bearer $TOK" https://<host>/ocphotos-api/api/stats
curl -su user:app-token https://<host>/index.php/apps/news/api/v1-3/feeds
```

Final functional verification: open the three apps in the OpenCloud web UI and
sync an external client (news-android or Iotas).

## Rollback

**Validity window**: the rollback is trivial (no data copy) **as long as no new
data that matters has been written to the `/var/lib/ocapps` databases**. The
source directories `/var/lib/ocnews`, `/var/lib/ocnotes` and `/var/lib/ocphotos`
stay intact (the migration only read and copied them). Recommendation: decide
rollback vs. keep within the first hours or days after the cutover, before the
data divergence grows.

### Simple rollback (inside the window)

```bash
systemctl stop opencloud-apps
systemctl enable --now ocnews ocnotes ocphotos   # their data is still intact
systemctl disable opencloud-apps                 # optional, do NOT delete anything yet
```

If you already applied the proxy change (see §Proxy change), **revert it**: route
`/ocphotos-api/` back to ocphotos :8097 **with strip** (trailing slash) and the
news/notes/OCS prefixes back to their old ports, then reload the proxy. The data
copied into `/var/lib/ocapps` can be kept as a backup or deleted once the
rollback is final.

### Rollback with new data written after the cutover

If users already generated data in the new databases that you do not want to
lose, it has to be dumped back (it is the migration procedure in reverse):

```bash
# 1. Stop EVERYTHING (new and old)
systemctl stop opencloud-apps ocnews ocnotes ocphotos

# 2. Verify the NEW databases before copying back
for f in /var/lib/ocapps/*/*.db; do
  sqlite3 "$f" 'PRAGMA integrity_check;' | grep -qx ok || echo "ABORT: $f corrupt"
done
sqlite3 /var/lib/ocapps/news/ocnews.db   'PRAGMA wal_checkpoint(TRUNCATE);'
sqlite3 /var/lib/ocapps/notes/notes.db   'PRAGMA wal_checkpoint(TRUNCATE);'
sqlite3 /var/lib/ocapps/photos/memories.db 'PRAGMA wal_checkpoint(TRUNCATE);'

# 3. Copy back (cp -a, back up the source .db files first, just in case)
cp -a /var/lib/ocnews/ocnews.db /var/lib/ocnews/ocnews.db.pre-rollback.bak     # same for notes/photos
cp -a /var/lib/ocapps/news/ocnews.db     /var/lib/ocnews/ocnews.db
cp -a /var/lib/ocapps/notes/notes.db     /var/lib/ocnotes/notes.db
cp -a /var/lib/ocapps/photos/memories.db /var/lib/ocphotos/memories.db
# and the auxiliary folders that changed (favicons/, attachments/, thumbs/...)

# 4. Verify again in the old directories (integrity_check) and start
systemctl enable --now ocnews ocnotes ocphotos
systemctl disable opencloud-apps
```

Warning, known limitation: if ocapps applied the **photos baseline + 002**
(`user_version` 0->2, multi-owner schema), the old ocphotos ignores that pragma
and starts with the extra `owner` columns. Note that uniqueness went from
`UNIQUE(path)` to `UNIQUE(owner, path)`, so a full reindex with the old binary
could insert duplicates (owner=''). If you are going back for good, deduplicate
first with
`DELETE FROM assets WHERE id NOT IN (SELECT min(id) FROM assets GROUP BY owner, path)`.
News and notes keep the `user_version` they had (18/2), with no transformation.

### After N days of stability (>= 7 recommended, SPEC §5.4)

```bash
# Archive the old data and remove the old units (until now they were only
# disabled):
tar -C /var/lib -czf /root/ocapps-migration-archive.tgz ocnews ocnotes ocphotos
rm -f /etc/systemd/system/{ocnews,ocnotes,ocphotos}.service
systemctl daemon-reload
```

## Monitoring

- **`GET /readyz` is the key alert**: 200 only if every enabled module is `ok`,
  503 if any is `failed`. A failed module does NOT restart the process (decision
  D3, SPEC §4.5), so systemd sees nothing (`Restart=on-failure` only acts if the
  whole process dies).
- `GET /healthz` returns 200 as long as the process is alive, with the
  per-module detail:
  `{"version":"...","modules":{"news":"ok","notes":"ok","photos":"failed: ..."}}`.
- JSON logs in journald: `journalctl -u opencloud-apps -f` (look for `"module
  failed"` and the `legacy env var in use` warnings).
