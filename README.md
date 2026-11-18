# ocapps - unified backend for the OpenCloud apps (news + notes + photos)

`ocapps` is a single Go service that replaces the three Go backends behind
the OpenCloud apps (`ocnews`, `ocnotes`, `ocphotos`): one static binary, one
listener (`127.0.0.1:8096`), one systemd unit and one proxy upstream, serving
the **same API contracts** as the three original services (News API v1.3,
Notes API v1/v1.4 + OCS, and the photos API under `/ocphotos-api/`).

Design spec: [`docs/SPEC.md`](docs/SPEC.md).
Deployment and migration from the old services:
[`deploy/README.md`](deploy/README.md).

## Mobile clients (Android)

All three apps have their own native Android client (Kotlin + Jetpack Compose,
AGPL-3.0), and all three speak the contract served by this binary:

| App | Client | API it consumes |
|---|---|---|
| News | [ocnews-android](https://github.com/gnacho/ocnews-android) | `/index.php/apps/news/api/v1-3/` |
| Notes | [ocnotes-android](https://github.com/gnacho/ocnotes-android) | `/index.php/apps/notes/api/v1.4/` |
| Photos | [ocphotos-android](https://github.com/gnacho/ocphotos-android) | `/ocphotos-api/api/` |

What this means for deployment: the client reaches the server through the
**same origin** as the web app, so the proxy routes in `deploy/README.md` are
also the ones the mobile apps use (notes additionally needs
`/ocs/v2.php/cloud/capabilities`). Authentication is OpenCloud **Basic
user:app token** (6-word phrase, `opencloud auth-app create`), never the
account password, and the routes cannot sit behind a browser session (mobile
requests carry no cookies).

## Architecture

A single Go module (`github.com/gnacho/ocapps`) with three application modules
on top of a shared base:

```
cmd/ocapps/            wiring: shared config -> shared Graph validator ->
                       per-module init with degradation (D3) -> single mux ->
                       loop supervisor with recover -> graceful shutdown
internal/
├── common/
│   ├── config/        unified OCAPPS_* env + legacy compatibility (SPEC §3)
│   ├── auth/          Graph validator (cache + singleflight) + middleware
│   │                  + MultiTenant (news/notes) / SingleTenant (photos) policies
│   ├── store/         unified SQLite open + migration runner
│   │                  (Migrate/Baseline; WAL, busy_timeout, foreign_keys)
│   ├── httpx/         WriteJSON, errors, unified CORS (SPEC §4.4)
│   ├── live/          per-owner SSE hub: change notices to connected
│   │                  clients (invalidation, not data)
│   ├── module/        Module interface (Register/Run/Healthy) + 503 handler
│   ├── log/           slog JSON with a "module" attr
│   ├── cred/          persistent secrets + AES-256-GCM (fail-loud)
│   ├── netguard/      anti-SSRF
│   ├── imgproxy/      image proxy with HMAC signatures
│   └── webdav/        generalized WebDAV/Graph client
├── news/              News API v1-3 (/index.php/apps/news/api/v1-3/) + /api/me|users
├── notes/             Notes API v1/v1.4 + OCS (/ocs/v2.php/cloud/*)
└── photos/            /ocphotos-api/api/* (multi-tenant via the host's OIDC Bearer)
```

Key decisions (SPEC): **D2** - three SQLite files
(`<data>/news/ocnews.db`, `notes/notes.db`, `photos/memories.db`), with no
transformation of the live databases (user_version 18/2/baseline->1). **D3** -
a module whose wiring fails stays `failed` (its namespace serves 503) without
taking the process down: `GET /healthz` (always 200, per-module detail) and
`GET /readyz` (503 if any enabled module is failing) are the operational
signal.

## Spaces (photos)

The photos scan walks **every space the user can see**, not only the personal
one: project spaces and shares included. Each asset records the space it was
found in (`spaceId`, `spaceName`, `spaceType`), so the client can show where a
photo lives and filter by it.

- The listing comes from `GET /graph/v1.0/me/drives` with the credential of the
  user whose turn it is to be scanned, so a space whose visibility changes stops
  being scanned (and its rows are soft-deleted) without any configuration.
- The soft-delete of a scan is scoped to the prefix of the space being walked:
  one space never deletes rows of another.
- A space without `OCAPPS_PHOTOS_SCAN_ROOT` (for example a project space with no
  `Fotos` folder) is skipped: it is not an error and its indexed assets are kept.
- `OCAPPS_PHOTOS_SCAN_SPACES` restricts the walk to a list of drives (`personal`
  or drive ids). Empty, the default, means every visible drive.
- The index exposes `GET /ocphotos-api/api/spaces` (spaces with photos of the
  owner and their count) and `GET /ocphotos-api/api/assets?space=<id>`; the
  folders handler takes `space=<id>` too ("" = the personal one).

## Live updates (SSE)

`GET /ocphotos-api/api/stream` is a **per-user** Server-Sent Events stream: the
client keeps it open and the server tells it when something of its own changes,
instead of the client polling every N seconds. No data travels over the stream,
only invalidations: on receiving an event, the client reloads that piece of data
through the normal API.

- Authentication: the same as the rest of `/ocphotos-api/` (host OIDC Bearer or
  Basic app-password). Each stream is bound to the user's `oc_id`, so events
  from other users are never delivered.
- Topics: `photos` (asset index or metadata: a scan that changed something, the
  EXIF backlog, favourite, archive, tags) and `albums` (create, rename, delete
  and membership).
- If the client sends `X-Ocapps-Client: <id>` on its writes, the event comes
  back with that id so the tab that made the change does not reload what it
  already has.
- Behaviour: heartbeat every 25 s, `X-Accel-Buffering: no` and `retry: 3000`.
  A client that does not read in time drops an event (it will get the next one).
  Streams are closed on shutdown, so `Shutdown` does not wait for them.
- **Proxy**: the route needs buffering disabled (`proxy_buffering off` in
  nginx). The snippets in `deploy/proxy/` already do it, and a custom vhost
  without it leaves the events stuck in the buffer.

Everything keeps working with no stream consumer: the client simply gets no
notices.

## Development quickstart

Requirements: Go >= 1.26 (build/test only) and ffmpeg (optional, only the
photos module uses it at runtime for video posters/HLS).

```bash
# Build (static binary, modernc.org/sqlite -> no CGO)
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go build -o ocapps ./cmd/ocapps

# Tests (with -race, TZ=UTC for deterministic results)
TZ=UTC go test -race -shuffle=on ./...
go vet ./...

# Run with minimal env (local mode: news only, bcrypt auth, no OpenCloud)
OCAPPS_AUTH_MODE=local \
OCAPPS_NEWS_AUTH_USER=admin OCAPPS_NEWS_AUTH_PASS=admin \
OCAPPS_PHOTOS_ENABLED=false OCAPPS_NOTES_ENABLED=false \
OCAPPS_DATA_DIR=/tmp/ocapps-dev \
./ocapps

curl -s localhost:8096/healthz    # {"version":"dev",...,"modules":{...}}
```

Against a real OpenCloud instance (default mode, `opencloud`):

```bash
OCAPPS_OPENCLOUD_URL=https://cloud.example.com \
OCAPPS_PHOTOS_USER=<user> OCAPPS_PHOTOS_APP_TOKEN=<app-token> \
./ocapps
```

Docker (includes ffmpeg in the runtime stage): `docker build -t ocapps .`.
Deployment with Docker Compose: [`deploy/README.md`](deploy/README.md#docker-compose-deployment).
Releases: `.goreleaser.yaml` (linux/amd64+arm64, version ldflags).

## Installing from a release

### One-command installer

On a server that already runs OpenCloud:

```bash
curl -fsSL https://raw.githubusercontent.com/gnacho/ocapps/main/install.sh \
  | sudo sh -s -- --url https://cloud.example.com
```

It does everything: the `ocapps` user, the binary, `/etc/ocapps/env`, the
systemd unit, an active service and, unless `--no-extensions` is given, the
`ocapps-ext` updater plus the three extensions (`--apps news,notes` to pick).
It is idempotent.

```bash
# backend only, no extensions
... | sudo sh -s -- --url https://cloud.example.com --no-extensions
# with the extension panel inside the app
... | sudo sh -s -- --url https://cloud.example.com --admin-token <token>
# show what it would do, without touching anything
... | sh -s -- --url https://cloud.example.com --dry-run
# uninstall (keeps /var/lib/ocapps) or remove everything
... | sudo sh -s -- --uninstall
... | sudo sh -s -- --purge
```

All that is left is publishing the routes on the proxy (`/ocphotos-api/`,
`/index.php/apps/news/`, `/index.php/apps/notes/`, the two
`/ocs/v2.php/cloud/*` and `/ocapps-admin/`) towards `127.0.0.1:8096`.

### Manual install

Each tag produces static binaries for `linux/amd64` and `linux/arm64` plus a
`checksums.txt` (`.goreleaser.yaml`). The archive contains the `ocapps` binary
and a copy of this README. Releases are created as drafts, so they must be
published by hand before the links work.

```bash
VERSION=0.1.4
BASE=https://github.com/gnacho/ocapps/releases/download/v${VERSION}
curl -fsSLO ${BASE}/ocapps_${VERSION}_linux_amd64.tar.gz
curl -fsSLO ${BASE}/checksums.txt
sha256sum --ignore-missing -c checksums.txt
tar xzf ocapps_${VERSION}_linux_amd64.tar.gz ocapps
```

Installing the service. The unit and `deploy/env.example` live in the repo, not
inside the release archive:

```bash
sudo install -m 755 ocapps /usr/local/bin/ocapps
sudo id -u ocapps >/dev/null 2>&1 || \
  sudo useradd --system --user-group --home-dir /var/lib/ocapps --shell /usr/sbin/nologin ocapps
sudo install -d -m 750 /etc/ocapps
sudo install -m 640 deploy/env.example /etc/ocapps/env
sudoedit /etc/ocapps/env
sudo install -m 644 deploy/opencloud-apps.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now opencloud-apps
curl -s localhost:8096/healthz
```

`/var/lib/ocapps` does not need to be created by hand: the unit declares it with
`StateDirectory` and systemd prepares it with the right permissions on start.

The binary is static (`CGO_ENABLED=0`). The only system dependency at runtime is
`ffmpeg`, and only the photos module uses it, for video posters and HLS.

Proxy, data migration from the old services and rollback:
[`deploy/README.md`](deploy/README.md).

## Configuration

Everything goes through `OCAPPS_*` environment variables. Precedence: `OCAPPS_*`
> legacy variable (from the old services, supported with a warning for 2 minor
versions, up to 0.3.0) > default. Full table with legacy names and notes: SPEC
§3.2 and [`deploy/env.example`](deploy/env.example).

| Variable | Default | Description |
|---|---|---|
| `OCAPPS_OPENCLOUD_URL` | *(required with opencloud auth)* | OpenCloud server root |
| `OCAPPS_LISTEN_ADDR` | `127.0.0.1:8096` | Single listener |
| `OCAPPS_DATA_DIR` | `/var/lib/ocapps` | Data root (`<dir>/{news,notes,photos}`) |
| `OCAPPS_LOG_LEVEL` | `info` | `debug\|info\|warn\|error` |
| `OCAPPS_AUTH_MODE` | `opencloud` | `opencloud` (Graph) \| `local` (bcrypt, news only) |
| `OCAPPS_NEWS_ENABLED` / `OCAPPS_NOTES_ENABLED` / `OCAPPS_PHOTOS_ENABLED` | `true` | Disable a module without uninstalling |
| `OCAPPS_NEWS_FEED_INTERVAL` | `15m` | Feed refresh (<= `MAX_GAP`) |
| `OCAPPS_NEWS_MAX_GAP` | `6h` | Maximum gap before a resync |
| `OCAPPS_NEWS_FETCH_TIMEOUT` | `20s` | HTTP timeout for feed downloads |
| `OCAPPS_NEWS_RETENTION_DAYS` | `90` | Item retention (0 = off) |
| `OCAPPS_NEWS_NTFY_URL` / `OCAPPS_NEWS_NTFY_TOPIC` | `https://ntfy.sh` / `""` | ntfy push notifications |
| `OCAPPS_NEWS_PUBLIC_URL` | `""` | Public API URL (WebSub callbacks) |
| `OCAPPS_NEWS_AUTH_USER` / `OCAPPS_NEWS_AUTH_PASS` | `""` | Bootstrap admin in `local` mode |
| `OCAPPS_NOTES_OWNER` | `""` | Backfill of `user` on empty rows (migration 002) |
| `OCAPPS_PHOTOS_USER` / `OCAPPS_PHOTOS_APP_TOKEN` | *(required if photos enabled)* | OpenCloud user and app token (single-tenant) |
| `OCAPPS_PHOTOS_TOKEN` | `""` | Static Bearer of its own (ex `MEMORIES_TOKEN`) |
| `OCAPPS_PHOTOS_SCAN_ROOT` | `Fotos` | Scan root folder (each space that does not have it is skipped) |
| `OCAPPS_PHOTOS_SCAN_SPACES` | *(all visible drives)* | Drives to scan, comma separated: `personal` and/or drive ids. Empty = every space the user sees (personal, project spaces and shares) |
| `OCAPPS_PHOTOS_SCAN_SPACE` | `""` | **DEPRECATED**: single drive id, folded into `OCAPPS_PHOTOS_SCAN_SPACES` |
| `OCAPPS_PHOTOS_SCAN_EVERY` | `5m` | Re-scan interval |

Validation (SPEC §3.3): invalid shared config is fatal (the process does not
start) and invalid module config leaves that module `failed` (503) while the
rest keeps running.

## Deployment and migration

See **[`deploy/README.md`](deploy/README.md)**: requirements (ffmpeg as the only
system dependency at runtime), build, binary + systemd unit + `/etc/ocapps/env`
installation, the **proxy change** (`/ocphotos-api/` becomes routed WITHOUT
strip), data migration with [`deploy/migrate.sh`](deploy/migrate.sh) (idempotent,
`--dry-run` first), the verification checklist and the rollback. Notices for the
app repos' READMEs are in [`deploy/repo-notices/`](deploy/repo-notices/).

## Project status

Milestone-based implementation (SPEC §8): **H0-H6 done** - skeleton and CI (H0),
`common` base (H1), notes/photos/news ports (H2-H4), unified wiring with D3
degradation (H5) and the deployment/migration artifacts (H6, `deploy/`).
Gates, all enforced in CI: `CGO_ENABLED=0 go build ./...`,
`go test -race -shuffle=on ./...`, `golangci-lint` with zero findings,
`govulncheck` and total statement coverage **>= 75%**
(`go test ./... -coverprofile=coverage.out`).
Pending before production: a staging dry run with copies of the real databases
and of the rollback (checklist SPEC §9.4), see `deploy/README.md`.

## License

AGPL-3.0-only. The full text is in [`LICENSE`](LICENSE), and the licenses of
every dependency (plus the vendored `goexif` fork) are inventoried in
[`THIRD_PARTY.md`](THIRD_PARTY.md).
