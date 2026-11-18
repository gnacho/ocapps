# Design: unified Go service `ocapps` (news + notes + photos)

**Status**: architecture proposal (read-only). Audience: coder subagents. Required context: this document plus the `ocnews`, `ocnotes` and `ocphotos` repos and the audits in `output/audit/`.

**Quick decisions (TL;DR)**:

| # | Decision | Summary |
|---|---|---|
| D1 | Repo | New monorepo `ocapps`, **single Go module** (no go.work, no submodules). The 3 app repos keep the extension + Store, their backends are frozen and archived after the first unified release. |
| D2 | SQLite | **Three `.db` files** (one per module) in subdirectories of a shared `DATA_DIR`. `PRAGMA user_version` keeps working per file with no namespacing. |
| D3 | Isolation | A module that fails to start means 503 on its routes plus a log entry plus a degraded `readyz`, while the process and the other modules keep going. **Shared** config errors are fatal. |
| D4 | Routing | A single `http.ServeMux`. Photos mounts under `/ocphotos-api/` and the proxy **stops stripping** (a 1-line proxy change, 0 in frontends). `/ocs/` becomes notes-only, and news drops its OCS stub (mandatory prior verification). |
| D5 | Auth | One Graph validator in `common/auth` with a 5 min cache and singleflight, plus a multi-user policy (shadow users) for news/notes and single-tenant for photos, parametrized. |
| D6 | Go/deps | `go 1.26`, `modernc.org/sqlite v1.56.0` unified, `replace` to an internal `third_party/goexif`. `CGO_ENABLED=0` binary, single system dependency: **ffmpeg**. |

---

## 1. Repo/module strategy

### 1.1 Decision: new `ocapps` repo with a single Go module

```
github.com/<org>/ocapps          <- new repo, BACKEND ONLY
├── go.mod                       <- module github.com/<org>/ocapps (a single module)
├── cmd/ocapps/main.go
├── internal/common/...
├── internal/{news,notes,photos}/...
├── third_party/goexif           <- vendored (today a replace in ocphotos)
└── deploy/...
```

**The code is ported (moved) once** from the three repos, it is not imported. Rejected alternatives:

- **go.work with the 3 repos as modules**: keeps the shared code triplicated (imgproxy, netguard, Graph auth and the HTTP helpers exist today in almost identical copies in ocnews and ocnotes), the version skew (`modernc.org/sqlite` 1.56 vs 1.38), the leak of the `replace github.com/rwcarlsen/goexif` from ocphotos, and three `go.mod` files to version and publish. A workspace does not reduce duplication, it only coexists with it in one build.
- **Git submodules**: the same problems plus a worse DX (coders would work over 4 checkouts).
- **Importing the three as packages**: the current module paths are heterogeneous (`github.com/gnacho/ocnews/backend`, `git.opencloud.example.com/gnacho/ocnotes/backend`, `github.com/opencloud-memories/photos-service`) and the imports would have to be rewritten anyway, so if imports must be touched, better to do it once towards the final layout.

### 1.2 Relationship with the existing repos (thinking about Phase 4 / Store)

The three public repos remain **the face of each app** in the Store:

| Repo | Future content | Backend |
|---|---|---|
| `ocnews` | `extension/`, `landing/`, assets, README, Store metadata | `backend/` is frozen at its last commit, a notice is added to the README and the repo is **archived** after the unified 0.1.0 release |
| `ocnotes` | `extension/`, README, metadata | same (backend in `backend/`) |
| `ocphotos` | `ocphotos/` (extension), `deploy/README.md`, metadata | `app/server-go/` frozen and archived, `app/src` (legacy PWA), `app/info.md` and the all-in-one `app/Dockerfile` are **removed** (quick-win Q6) |
| `ocapps` (new) | unified backend, `deploy/` (systemd, proxy, migration) | - |

**Phase 4 (Store)**: each app is packaged separately with its extension and a manifest that documents the `ocapps >= <version>` *companion server* as a requirement (a single installation serves the three apps, and the app declares which module it uses). Extension packaging does not change (`vite build` in each app repo). The backend stops being distributed per app and becomes a release artifact of `ocapps` (static binary + systemd unit + migration script).

**Consequence for coders**: ports are done by copying files and rewriting imports mechanically (`github.com/gnacho/ocnews/backend/internal/X` -> `github.com/<org>/ocapps/internal/news/X`, and so on). Tests travel with their packages.

---

## 2. Package layout

```
ocapps/
├── go.mod                        # go 1.26, unified deps (see §2.3)
├── cmd/
│   └── ocapps/
│       └── main.go               # wiring: shared config -> modules -> single mux -> supervisor
├── internal/
│   ├── common/
│   │   ├── config/               # unified env + legacy compatibility (§3)
│   │   ├── auth/                 # Graph validator + cache + singleflight + middleware (§6)
│   │   ├── webdav/               # generalized WebDAV/Graph client (port of ocphotos internal/dav)
│   │   ├── store/                # SQLite open + migration runner (§5)
│   │   ├── httpx/                # WriteJSON, ErrorStatus, DecodeBody, CORS, preflight (§4.4)
│   │   ├── log/                  # slog JSON with a "module" attr
│   │   ├── cred/                 # AES-256-GCM + load/generate of persistent secrets (fail-loud)
│   │   ├── netguard/             # anti-SSRF (merge of ocnews/netguard and ocnotes/netguard)
│   │   └── imgproxy/             # merge of ocnews/imgproxy and ocnotes/imgproxy (identical in spirit)
│   ├── news/
│   │   ├── api/  store/  feed/  refresher/  scheduler/  websub/  rules/
│   │   ├── notify/  extract/  favicon/  sanitize/  privacy/  i18n/
│   │   └── module.go             # module wiring (Run/RegisterRoutes/Healthy)
│   ├── notes/
│   │   ├── api/  store/  attachments/
│   │   └── module.go
│   └── photos/
│       ├── api/  store/  index/  exif/  thumb/  video/  geo/  phash/
│       └── module.go             # the scan loop lives here (today closures in main.go)
├── third_party/goexif/           # internal replace
└── deploy/
    ├── opencloud-apps.service
    ├── env.example
    ├── migrate.sh                # adoption of existing databases (§5.4)
    ├── proxy/                    # nginx/caddy snippets
    └── README.md
```

### 2.1 What goes into `common` (specifically)

| Package | Origin | Key API (signatures) | Notes |
|---|---|---|---|
| `common/config` | the 3 `config` packages | `func Load() (*Config, error)`, `type Config struct { Common; News NewsConfig; Notes NotesConfig; Photos PhotosConfig }` | reads `OCAPPS_*` with legacy fallback and warning (§3). Fail-fast validation of the **common** part only. |
| `common/auth` | ocnews `internal/auth` (the most complete) | `type Credential struct{ Username, Password, Bearer string }`, `type User struct{ ID, Username, DisplayName, Email string }`, `type Policy interface { Admit(u *User) bool }`, `func NewGraphValidator(graphURL string, pol Policy, log *slog.Logger) *GraphValidator`, `func (v *GraphValidator) Validate(ctx context.Context, c Credential) (*User, bool)`, `func Middleware(v *GraphValidator, next http.Handler) http.Handler` | positive cache of 5 min (key `sha256(token)` / `basic:user`), **negative** cache of 30 s (new, avoids hammering Graph on repeated 401s), `singleflight.Group` per key. Policies: `MultiTenant()` and `SingleTenant(ocID string)` (§6). |
| `common/webdav` | ocphotos `internal/dav` | `type Client struct{...}`, `func New(baseURL, user, appToken string) *Client`, `MeID(ctx)`, `ListDrives(ctx)`, `Propfind(ctx, href string, depth int)`, `GetRange(ctx, href string, off, n int64)`, `Download/DownloadRange(ctx, href)`, `SpaceFileURL(...)` | the `imageExt`/`videoExt` lists move out of the client into options: `type Options struct{ ImageExts, VideoExts []string }` passed to the photos scanner, not to the client. |
| `common/store` | ocnews `internal/store` (Open and migrate) | `func Open(path string) (*sql.DB, error)` (unified pragmas, §5.2), `func Migrate(db *sql.DB, fsys fs.FS, dir string) error` (`user_version` runner), `func Baseline(db *sql.DB, version int) error` (photos adoption, §5.3) | Each module still has its own domain `store` package on top of the shared `*sql.DB`. |
| `common/httpx` | helpers from the 3 (`writeJSON`, `errorStatus`, `decodeBody`, `withCORS`) | `func WriteJSON(w http.ResponseWriter, code int, v any)`, `func ErrorStatus(w http.ResponseWriter, r *http.Request, err error)`, `func DecodeBody(r *http.Request, dst any) error`, `func CORS(next http.Handler) http.Handler`, `func Preflight() http.Handler` | unified CORS (§4.4). `WriteJSON` sets `Content-Type` **before** `WriteHeader` (minor bug in ocphotos, which sets it but does not write the code). |
| `common/log` | pattern from the 3 | `func New(level string) *slog.Logger`, `func Module(log *slog.Logger, name string) *slog.Logger` -> `log.With("module", name)` | One single `slog.SetDefault` in main. |
| `common/cred` | ocnews `cred` + the secret pattern of both imgproxies | `func LoadOrCreateSecret(path string) ([]byte, error)` (0600, >= 32 bytes, **error if `rand.Read` fails**, which fixes the hardcoded photos fallback), `type Cipher struct{...}` AES-256-GCM | Reused by: feed credentials (news), imgsecret (news/notes), mediasecret (photos, Q3). |
| `common/netguard` | ocnews `netguard` (74 lines) + ocnotes | `func CheckURL(u *url.URL) error` / `func SafeClient() *http.Client` | Rejects private, loopback and link-local IPs. Used by imgproxy, the feed fetcher and the extractor. |
| `common/imgproxy` | ocnews + ocnotes (almost identical) | `func New(dataDir string, log *slog.Logger) (*Proxy, error)`, `func (p *Proxy) Sign(rawURL string) string`, `func (p *Proxy) Serve(w, r)` | One instance **per module** (each with its own `imgsecret` and `imgcache` in its subdirectory), so HMAC signatures stay isolated per module as they are today. |

### 2.2 What does NOT move into common

`feed`, `refresher`, `scheduler`, `websub`, `rules`, `notify`, `extract`, `favicon`, `sanitize`, `privacy`, `i18n` (news), `attachments` (notes), `index`, `exif`, `thumb`, `video`, `geo`, `phash` and the domain stores. `sanitize`/`extract` are only used by news, and if photos ever needs them they get promoted then.

### 2.3 Unified dependencies (go.mod)

```
module github.com/<org>/ocapps
go 1.26

require (
    github.com/PuerkitoBio/goquery v1.12.0            // news (scraper)
    github.com/andybalholm/cascadia v1.3.3            // news
    github.com/gen2brain/h265 v0.2.2                  // photos (HEIC, pure Go)
    github.com/go-shiori/go-readability v0.0.0-...    // news (full content)
    github.com/microcosm-cc/bluemonday v1.0.27        // news (sanitize)
    github.com/mmcdole/gofeed v1.4.1                  // news
    github.com/rwcarlsen/goexif v0.0.0-2019...        // photos (EXIF) -> replace
    golang.org/x/crypto v0.55.0                       // news (local bcrypt)
    golang.org/x/image v0.30.0                        // photos (thumbs/phash)
    golang.org/x/sync v0.x                            // NEW: singleflight (auth)
    modernc.org/sqlite v1.56.0                        // unified (photos goes 1.38 -> 1.56)
)
replace github.com/rwcarlsen/goexif => ./third_party/goexif
```

Risk of moving photos to sqlite 1.56: low (same driver, stable API, and the photos store tests against real SQLite validate it). `go 1.26` because of a current photos requirement (`go 1.26.4` in its go.mod), so pin the toolchain in CI/Docker (today `app/Dockerfile` uses `golang:1.24-alpine` with auto-download, an implicit quick-win: `golang:1.26-alpine` image).

---

## 3. Unified configuration schema

### 3.1 Compatibility policy (legacy)

1. **Precedence**: new `OCAPPS_*` > legacy variable > default.
2. When a legacy variable is used: `log.Warn("legacy env var in use, migrate to OCAPPS_*", "legacy", "OCNEWS_FEED_INTERVAL", "new", "OCAPPS_NEWS_FEED_INTERVAL")`, one warning per variable and per start.
3. Legacy support lasts **2 minor versions** (introduced in 0.1.0, removed in 0.3.0). In 0.2.0 the warning is raised to `Error`, visible but not fatal, and in 0.3.0 the legacy variables are ignored.
4. Implementation: a single helper in `common/config`:

```go
// get returns the value of the new var, or the legacy one (with a warn) if it
// does not exist, or def if neither does. It records every legacy used for the
// summary log.
func (l *loader) get(newVar, legacy, def string) string
func (l *loader) dur(newVar, legacy string, def time.Duration) (time.Duration, error)
```

### 3.2 Full mapping table

**Shared (new, with no equivalent or with several legacy equivalents):**

| New | Default | Legacy (in order of precedence among them) | Notes |
|---|---|---|---|
| `OCAPPS_OPENCLOUD_URL` | *(required)* | `OCNEWS_OPENCOLOUD_URL` (**historic typo, double O**), `OCNOTES_GRAPH_URL`†, `OC_BASE_URL` | OpenCloud server root. †`OCNOTES_GRAPH_URL` is the full URL `.../graph/v1.0/me`, so mapping it derives the root (strip the `/graph/v1.0/me` suffix if present) and logs the transformation. |
| `OCAPPS_LISTEN_ADDR` | `127.0.0.1:8096` | `OCNEWS_ADDR`, `OCNOTES_ADDR`, `LISTEN_ADDR` | **A single listener.** The legacy names are only used to detect old deployments in the logs, the port is not inherited (each service had its own). Documented in the migration. |
| `OCAPPS_DATA_DIR` | `/var/lib/ocapps` | `OCNEWS_DATA_DIR`, `OCNOTES_DATA_DIR`, `DATA_DIR` | The per-module subdirectories are derived: `<dir>/news`, `<dir>/notes`, `<dir>/photos` (§5.1). If a per-module legacy variable is set, that path is used for that module (smooth transition). |
| `OCAPPS_LOG_LEVEL` | `info` | `OCNEWS_LOG_LEVEL` | debug\|info\|warn\|error. |
| `OCAPPS_AUTH_MODE` | `opencloud` | `OCNEWS_AUTH_MODE`, `OCNOTES_AUTH_MODE` | `opencloud` (Graph) \| `local` (bcrypt, news only, for bootstrap/dev). The default changes from `local` (ocnews) to `opencloud`, which is already the mode used in real deployments. |
| `OCAPPS_NEWS_ENABLED` / `OCAPPS_NOTES_ENABLED` / `OCAPPS_PHOTOS_ENABLED` | `true` | - | Lets you turn a module off without uninstalling. |

**News (`OCAPPS_NEWS_*`):**

| New | Default | Legacy | Notes |
|---|---|---|---|
| `OCAPPS_NEWS_FETCH_TIMEOUT` | `20s` | `OCNEWS_FETCH_TIMEOUT` | > 0 |
| `OCAPPS_NEWS_FEED_INTERVAL` | `15m` | `OCNEWS_FEED_INTERVAL` | > 0 |
| `OCAPPS_NEWS_MAX_GAP` | `6h` | `OCNEWS_MAX_GAP` | >= FEED_INTERVAL |
| `OCAPPS_NEWS_RETENTION_DAYS` | `90` | `OCNEWS_RETENTION_DAYS` | 0 = disabled |
| `OCAPPS_NEWS_NTFY_URL` | `https://ntfy.sh` | `OCNEWS_NTFY_URL` | |
| `OCAPPS_NEWS_NTFY_TOPIC` | `""` | `OCNEWS_NTFY_TOPIC` | |
| `OCAPPS_NEWS_PUBLIC_URL` | `""` | `OCNEWS_PUBLIC_URL` | WebSub callbacks |
| `OCAPPS_NEWS_AUTH_USER` / `OCAPPS_NEWS_AUTH_PASS` | `""` | `AUTH_USER` / `AUTH_PASS` | bootstrap admin in `local` mode. The generic unprefixed names disappear from the new schema. |

**Notes (`OCAPPS_NOTES_*`):**

| New | Default | Legacy | Notes |
|---|---|---|---|
| `OCAPPS_NOTES_OWNER` | `""` | `OCNOTES_OWNER` | backfill of `user` on empty rows (migration 002) |

(Notes needs nothing else, since its addr/dataDir/graph become shared.)

**Photos (`OCAPPS_PHOTOS_*`):**

| New | Default | Legacy | Notes |
|---|---|---|---|
| `OCAPPS_PHOTOS_USERS` | `""` | - | **(H8)** `alice:apptoken1,bob:apptoken2`: users with a periodic background scan (per-user app token, memory only). A malformed entry is a module error (D3). |
| `OCAPPS_PHOTOS_USER` | `""` | `OC_USER` | *(optional since H8)* user of the legacy app token. Still used for (a) the identity of the static Bearer and (b) the single-tenant index backfill (§5.3). Folds into `PHOTOS_USERS` with a deprecation WARN. |
| `OCAPPS_PHOTOS_APP_TOKEN` | `""` | `OC_APP_TOKEN` | *(optional since H8)* OpenCloud app token (same). |
| `OCAPPS_PHOTOS_TOKEN` | `""` | `MEMORIES_TOKEN` | **DEPRECATED (H8)**: static machine-to-machine Bearer. Only valid if `OCAPPS_PHOTOS_USER` is configured (it maps to the legacy oc_id), and configuring it without `USER` is a module config error. Clients should use the OpenCloud OIDC Bearer. |
| `OCAPPS_PHOTOS_SCAN_ROOT` | `Fotos` | `SCAN_ROOT` | applies to every user alike; a space without that folder is skipped |
| `OCAPPS_PHOTOS_SCAN_SPACES` | *(all)* | - | **(#145)** drives to walk: `personal` and/or ids (the one after `$` in `/dav/spaces/<storage>$<drive>`), comma separated. Empty = every drive visible to the user (personal, project spaces and shares). An id that is not visible is a scan error, never a silent fallback. |
| `OCAPPS_PHOTOS_SCAN_SPACE` | `""` | - | **DEPRECATED**: single drive id, folded into `OCAPPS_PHOTOS_SCAN_SPACES` |
| `OCAPPS_PHOTOS_SCAN_EVERY` | `5m` | `SCAN_EVERY` | |
| - | - | `WEB_DIR` | **Removed** (legacy PWA retired, Q6). If defined: warn "ignored". |

### 3.3 Validation rules

- **Fatal (shared config)**: empty `OCAPPS_OPENCLOUD_URL` with auth mode `opencloud`, invalid `OCAPPS_LOG_LEVEL`, `OCAPPS_DATA_DIR` that cannot be created or written.
- **Degrades the module, not the process**: invalid `OCAPPS_NEWS_*` values (durations) -> news failed, `OCAPPS_PHOTOS_TOKEN` without `OCAPPS_PHOTOS_USER` or a malformed `OCAPPS_PHOTOS_USERS` -> photos failed (H8: empty `PHOTOS_USER/APP_TOKEN` no longer degrade anything, since photos is multi-tenant and requires no user). Rationale in §4.5/D3.

---

## 4. Routing

### 4.1 Single mux with strict namespacing

`cmd/ocapps/main.go` builds an `http.ServeMux` (Go 1.22+, method patterns). Each module registers **only** under its namespace:

| Prefix / route | Module | Auth | Origin |
|---|---|---|---|
| `/index.php/apps/news/api/v1-3/` | news | Basic/Bearer (except the public exemptions below) | News API v1.3 contract |
| `/index.php/apps/news/api/v1-3/img` · `/favicon/` · `/share/` · `/websub/` | news | **public** (HMAC signature / share token / hub) | same as today |
| `GET\|PUT /api/me/settings`, `GET\|PUT /api/me/rules`, `/api/me`, `/api/users*` | news | Basic/Bearer | today registered as `/api/` at the root of the ocnews mux, and the extension calls `/api/me/*` at the host root (verified in `extension/src/api.ts:177-197`), so **the proxy already routes `/api/*` to news, which is kept** but with exact patterns instead of a catch-all |
| `/index.php/apps/notes/api/v1/` and `/v1.4/` | notes | Basic/Bearer, `GET /v1/img` public and signed | Notes API contract |
| `/ocs/v2.php/cloud/capabilities` · `/ocs/v2.php/cloud/user` | notes | Basic/Bearer | **collision resolved**: see §4.2 |
| `/ocphotos-api/api/` | photos | session Bearer / own token, `GET /ocphotos-api/api/video/{id}` public and signed | **proxy change**: it stops stripping (§4.3) |
| `GET /healthz` | common | public | liveness plus per-module status (§4.5) |
| `GET /readyz` | common | public | 200 only if every enabled module is healthy, 503 otherwise |

### 4.2 `/ocs/v2.php/cloud/user` collision (news stub vs real notes)

Today **both** serve `/ocs/v2.php/cloud/user`: ocnews as a stub for news-android (`server.go:85`) and ocnotes as the real handler (`handleUserInfo`). In the unified mux only one can stay.

**Decision**: **notes** serves it and news drops its stub. **Mandatory prior verification task** (H4): diff the OCS payloads of both handlers (envelope `ocs.meta` + `ocs.data`: id, displayname, email). If the news stub includes fields that news-android needs and the notes one does not emit, the notes handler gets extended, so a single OCS handler serves news-android, Iotas and the web. Low risk: both derive from the same Graph shadow user.

### 4.3 `/api/` collision (news vs photos) and the proxy change

Today the proxy strips `/ocphotos-api/`, so photos sees `/api/...` (`deploy/README.md`: "trailing slash strips the prefix"). In the unified binary `/api/` already belongs to news (`/api/me/*`), so:

**Decision**: the proxy routes `/ocphotos-api/` to `http://127.0.0.1:8096` **with no strip**, and photos registers its routes as `/ocphotos-api/api/...`. Zero frontend changes (the extension already calls `/ocphotos-api/api/...`) and the prefix hardcoded in the video signature (`"/ocphotos-api/api/video/%d"`, `api.go:342`) is still correct as is. The prefix is extracted into a constant `photos.PublicPrefix = "/ocphotos-api"` used both in the route registration and in the video signature (parametrizable with `OCAPPS_PHOTOS_PUBLIC_PREFIX` only if it is ever needed, do not expose it without demand).

Alternative considered and rejected: keep the strip and mount photos at `/api/` with news moving `/api/me`, which would break the news extension (it calls `/api/me/*` with no app prefix) and would mix two contracts under one prefix.

### 4.4 Unified CORS

The three services use `Access-Control-Allow-Origin: *` today with header auth (Bearer/Basic, **no cookies**). Decision: **`ACAO: *` is kept**, for these reasons:

1. With no cookies and no `Access-Control-Allow-Credentials`, a Bearer token is not ambient authority, so the classic `*` risk (response theft with session credentials) does not apply.
2. The web extensions call **same-origin** through the host proxy (CORS does not even come into play) and native clients (Iotas, news-android) ignore CORS. Narrowing it to a single origin would only add a config variable that breaks deployments with the backend on another host or port (a case documented in ocphotos), with no security gain.

Real unification: a single `httpx.CORS` middleware with the **union** of the three method and header sets:
- `Allow-Methods: GET, POST, PUT, PATCH, DELETE, OPTIONS`
- `Allow-Headers: Authorization, Content-Type, If-Match, OCS-APIRequest`
- `Expose-Headers: ETag, Last-Modified, X-Notes-API-Versions, X-Notes-Chunk-Cursor`
- `OPTIONS` preflight -> 204 without going through auth (register `OPTIONS <prefix>/` before the authenticated handler, as ocnews already does).

### 4.5 Single healthz and failure policy (D3)

```json
GET /healthz  -> 200 as long as the process is alive
{"version":"0.1.0","modules":{"news":"ok","notes":"ok","photos":"failed: sqlite: disk I/O error"}}
GET /readyz   -> 200 if every enabled module is ok, 503 if any is failing (same body)
```

**Failure policy, also on the initial start** (decision D3, justified):

- A module whose wiring fails (DB does not open, broken migration, Graph unreachable) stays in the `failed` state: its routes answer `503 {"error":"module <name> unavailable"}` (a substitute handler registered in its namespace), the error is logged with its cause, and the process **keeps going**. A partial service beats a total outage, the operator sees it in `readyz` (503) and in the logs, and systemd does not enter a restart loop.
- **Fatal exception**: errors in the shared base (invalid shared config, unwritable `DATA_DIR`, port already in use) mean the process does not start (`main` returns an error, `os.Exit(1)`), because there is nothing useful to serve.
- Photos also gains an improvement: if `ListDrives`/Graph fails at startup, the module stays `failed` and **retries in the background** (30 s to 5 min backoff) instead of the current `os.Exit(1)`. **(H8: no longer applies, since photos is multi-tenant and does NO OpenCloud init at startup. The module is born healthy, `Healthy()` is a SQLite ping, and personal space resolution is lazy per user.)**
- Background loops (the news scheduler, the photos scanner) run with a per-goroutine `recover()`: a panic marks the module `failed` without taking the process down. There is no module auto-restart in v0.1 (that is future work, and systemd only restarts if the whole process dies).

Module interface (in `internal/common/httpx` or a small `common/module` package):

```go
type Module interface {
    Name() string
    Enabled() bool
    // Register mounts the module routes on the mux (or the 503 handler if failed).
    Register(mux *http.ServeMux)
    // Run executes the background loops; it must return when ctx is cancelled.
    Run(ctx context.Context) error
    // Healthy reports to /healthz and /readyz.
    Healthy() error
}
```

---

## 5. SQLite

### 5.1 Decision: three `.db` files (D2)

```
$OCAPPS_DATA_DIR/
├── news/     ocnews.db  (+favicons/, imgcache/, imgsecret, feedsecret)
├── notes/    notes.db   (+attachments/, imgcache/, imgsecret)
└── photos/   memories.db(+thumbs/, hls/, mediasecret)
```

**Justification against a single database with table prefixes:**

1. **`PRAGMA user_version` is per file.** With three files, the existing migration schemas of news (user_version=18) and notes (user_version=2) work **without touching a single line**, while a single database would need namespaced versions (a `schema_migrations(module, version)` table) and re-baselining news anyway.
2. **Failure isolation** (consistent with D3): corruption or a lock in one database does not affect the others, `SetMaxOpenConns(1)` is per connection and the single-writer pattern is preserved per file.
3. **Per-module backup/rollback** (copy one file) and trivial data migration: copy the existing `.db` files into their new subdirectory (§5.4) with no transformation at all.
4. The generic table names of photos (`assets`) and notes (`settings`) would conceptually collide in a shared database.

The per-module data directory also holds its auxiliary files (secrets, caches, attachments), keeping the current names so the copy is 1:1.

### 5.2 Unified open (`common/store`)

A single `Open` with the union of the three pragma sets (they differ today: news uses `busy_timeout(5000)` + WAL + `foreign_keys(1)`, photos uses `busy_timeout(10000)` with no explicit WAL in the DSN, since it enables it in the schema, and **no foreign_keys in the DSN**, and notes is similar to news):

```go
func Open(path string) (*sql.DB, error) {
    // mkdir 0700 of the parent dir
    dsn := "file:" + url.PathEscape(path) +
        "?_pragma=busy_timeout(10000)" +
        "&_pragma=journal_mode(WAL)" +
        "&_pragma=synchronous(NORMAL)" +
        "&_pragma=foreign_keys(1)"
    // SetMaxOpenConns(1)
}
```

Warning: verify in tests that enabling `foreign_keys(1)` on the photos database breaks nothing (its tables carry `REFERENCES ... ON DELETE CASCADE`, so enabling it is semantically more correct, but its suite has to be run).

### 5.3 Unified migration runner and adoption of existing databases

**Shared runner** (ported from the ocnews one, which is the cleanest pattern):

```go
// Migrate applies migrations/*.sql (embed) with a number > PRAGMA user_version,
// each in its own transaction, setting user_version at the end.
func Migrate(db *sql.DB, fsys fs.FS, dir string) error
// Baseline sets user_version without running SQL (adoption of live schemas).
func Baseline(db *sql.DB, version int) error
```

| Module | Current state | Adoption in ocapps |
|---|---|---|
| **news** | 18 migrations, `user_version=18` | **No changes.** Copy `migrations/001..018` to `internal/news/store/migrations/` and use `Migrate`. The live database arrives with `user_version=18` and nothing is applied. |
| **notes** | 2 migrations plus a prior backup and an owner backfill | Port `migrate.go` onto the shared runner **keeping** its pre-migration backup (`notes.db.<ts>.bak`) and the backfill with `OCAPPS_NOTES_OWNER`. Live database: `user_version=2`, no-op. |
| **photos** | schema built with `CREATE TABLE IF NOT EXISTS` plus 2 idempotent `ALTER` statements tolerant of "duplicate column", **with no versions** | **Baseline it**: `001_baseline.sql` is the full current schema (CREATE IF NOT EXISTS + indexes + the two columns `is_archived`/`phash` already included in the `assets` CREATE). The adoption function: (1) opens the live database, (2) checks that the expected tables exist (`assets`, `scan_state`, `albums`, `album_assets`, `asset_tags`, `geocode`) and the columns `is_archived` and `phash` (`PRAGMA table_info(assets)`), (3) if everything is there, `Baseline(db, 1)`, (4) if the database is empty or new, `Migrate` applies `001_baseline.sql` and sets `user_version=1`. Future migrations start at `002_*.sql`. The fragile pattern of hot ALTERs disappears. **(H8)** `002_multiowner.sql` takes the schema to multi-owner (rebuild of `assets` with `owner` + `UNIQUE(owner,path)`, `ALTER albums ADD owner`, indexes with an owner prefix, while `geocode` deliberately stays global, as it is a cache of public names, not personal data). Baseline 001 was updated so new databases are **born** multi-owner and the final state converges to `user_version=2` (a new database applies 001+002, and 002 over empty tables is an effective no-op, while an adopted live database at 1 receives the owner through 002). The runner executes the migrations with `foreign_keys` OFF, because otherwise the `DROP TABLE` of the rebuild would fire the `ON DELETE CASCADE` of `album_assets`/`asset_tags`. The **backfill** of the single-tenant era rows (`owner=''`) cannot be SQL: the module resolves the oc_id with `OCAPPS_PHOTOS_USER`+`APP_TOKEN` (`MeID`) and runs `UPDATE ... SET owner=?` on startup, and without those credentials it logs a WARN with the count and starts anyway (see §6.2 and `deploy/README.md`). |

**New rule for all three**: every future migration is an immutable `NNN_description.sql` file in its module's `migrations/` directory, and error-tolerant ALTERs outside the runner are forbidden.

### 5.4 Data migration procedure (production: 3 live databases and systemd x3 -> x1)

Principles: **copy, never move while hot**, verify with `PRAGMA integrity_check`, and roll back without data loss. Script `deploy/migrate.sh` (idempotent, with `--dry-run`):

```bash
# 0. Pre-flight
systemctl stop ocnews ocnotes ocphotos        # order does not matter, wait for inactive
# 1. Verify the source (before copying)
for db in /var/lib/ocnews/ocnews.db /var/lib/ocnotes/notes.db /var/lib/ocphotos/memories.db; do
  sqlite3 "$db" "PRAGMA integrity_check;" | grep -qx ok || abort
done
# 2. Copy to the new layout (cp preserves, never mv)
install -d -m 700 -o ocapps /var/lib/ocapps/{news,notes,photos}
cp -a /var/lib/ocnews/ocnews.db*   /var/lib/ocapps/news/      # includes -wal/-shm if they exist
cp -a /var/lib/ocnotes/notes.db*   /var/lib/ocapps/notes/
cp -a /var/lib/ocphotos/memories.db* /var/lib/ocapps/photos/
# 3. Copy the auxiliary data per module
cp -a /var/lib/ocnews/{favicons,imgcache,imgsecret,feedsecret}  /var/lib/ocapps/news/
cp -a /var/lib/ocnotes/{attachments,imgcache,imgsecret}         /var/lib/ocapps/notes/
cp -a /var/lib/ocphotos/{thumbs,hls,mediasecret}                /var/lib/ocapps/photos/
# 4. Verify the destination
for db in .../ocapps/*/*.db; do sqlite3 "$db" "PRAGMA integrity_check;" | grep -qx ok || abort; done
# 5. Record the expected versions (audit log)
sqlite3 /var/lib/ocapps/news/ocnews.db "PRAGMA user_version;"   # expected: 18
sqlite3 /var/lib/ocapps/notes/notes.db "PRAGMA user_version;"   # expected: 2
# 6. systemd: disable the old ones (do NOT delete), enable and start the unified one
systemctl disable ocnews ocnotes ocphotos
systemctl enable --now opencloud-apps
# 7. Smoke checks (§9.4)
```

Notes:
- With the services stopped there should be no `-wal`/`-shm` files. If they exist, copy them next to the `.db` file (or run `sqlite3 $db "PRAGMA wal_checkpoint(TRUNCATE);"` before stopping and copying).
- The first ocapps start applies: news/notes no-op, photos baseline (§5.3). The notes migration 002 (owner backfill) is already applied in the live database, so `OCAPPS_NOTES_OWNER` is only needed if rows without an owner were left.
- **Rollback** (valid as long as no new data that matters has been written, document the window): `systemctl stop opencloud-apps` -> `systemctl enable --now ocnews ocnotes ocphotos` (their data in `/var/lib/ocnews|ocnotes|ocphotos` is intact, only read and copied) -> revert the proxy change if the one from §4.3 was applied. If data was already written to the new databases and has to be dumped back: stop everything, copy the `.db` files back (same verification) and start the old services.
- After N days of stability (>= 7 recommended): archive `/var/lib/ocnews*` and remove the old units.

---

## 6. Unified auth

### 6.1 A single Graph validator in `common/auth`

The base is the ocnews `OpenCloudValidator` (the most complete: Basic+Bearer, shadow users, TTL), and the ocnotes (in-memory cache with eviction) and ocphotos (validation with no cache plus a single-tenant check) variants are absorbed into it:

```go
type GraphValidator struct {
    graphURL string                 // <OCAPPS_OPENCLOUD_URL>/graph/v1.0/me
    client   *http.Client           // 10 s timeout
    cache    sync.Map               // key -> cacheEntry{user *User, expiry, negative bool}
    sf       singleflight.Group     // a single Graph call per key in flight
    policy   Policy
}
```

- **Cache keys**: `basic:<username>:<sha256(password)>` and `bearer:<sha256(token)>` (ocnotes truncates `token[:32]` in clear as the key today, and a full hash is better, since it avoids collisions and does not store the token). The Basic key includes the password **only as a hash** (never in clear), as the original ocnews did (`sha256("basic\x00user\x00pass")`). Without it, after a legitimate login any password of that user would be accepted from the cache during the TTL, and a negative entry cached under the same key would allow a DoS of the legitimate user with a single failed attempt.
- **TTL**: positive 5 min (same as news/notes today). **Negative 30 s (new)**: a 401/403 from Graph is cached for 30 s so a looping client does not hammer the IdP. **Only explicit rejections (401/403) are cached**: network failures, 5xx and malformed responses are NOT cached, because a down IdP must not poison the cache and turn a transient glitch into a 30 s outage. Document the change: credential revocation takes up to 5 min to propagate (already the case in news/notes, and in photos it goes from 0 to 5 min, an acceptable window aligned with the rest).
- **Singleflight**: on a concurrent cache miss for the same key, a single request goes to Graph and the rest wait for its result. It removes the stampede when hot entries expire (the photos extension fires bursts of thumbnail requests).

### 6.2 Multi-user in the three modules (photos multi-tenant since H8)

One validator with an **injected policy**:

```go
type Policy interface { Admit(u *User) bool }
type multiTenant struct{}                    // the three modules (photos since H8)
type singleTenant struct{ ocID string }      // no longer used by photos, kept in common/auth
```

- **News**: keeps its `users` table (shadow users with `oc_id`, admin role for the first user), as that is its domain model. The shared validator returns the Graph `User` and `internal/news` resolves or creates its local row as `OpenCloudValidator` does today (that part stays thin in `internal/news/auth.go`).
- **Notes**: keeps its in-memory shadow user plus scoping by graph ID in the `user` column (its "persistence" is the `notes` table itself).
- **Photos (H8)**: `MultiTenant()`. The `*auth.User` resolved by `Validate` is injected into the request context (`api.OwnerFrom`) and ALL handlers scope to the owner (oc_id), with the store filtering by `owner` in every query (IDOR rule: someone else's id gives `sql.ErrNoRows`, hence 404, never 403). Basic app-password also comes in. The web extension already sends the OIDC Bearer on every call.
  - **In-memory sessions (registry)**: every authenticated request runs `Touch(owner, cred)`, which creates or updates `map[owner]*session{dav, webdavURL, isBasic, lastSeen}` with a `webdav.NewBearer` (or Basic) client. **Nothing is persisted to disk** (privacy: OIDC tokens only live in RAM). Bearer sessions idle for more than 24h are purged, while Basic ones and those seeded by `OCAPPS_PHOTOS_USERS` are not. The `webDavUrl` of the personal space is resolved lazily (on the user's first request or scan) through `ListDrives` with THEIR credential.
  - **Activity-driven indexing (scheduler)**: a user's first use triggers their scan (progressive index), a `SCAN_EVERY` ticker scans the seeded users and the active Basic sessions, and the queue has per-owner dedupe, a semaphore of 2 concurrent scans and a 2h timeout per scan. On 401/403 (`webdav.ErrUnauthorized`) the scan ABORTS with no soft-delete (the index stays intact) and is postponed until the user's next activity.
  - **Static Bearer `OCAPPS_PHOTOS_TOKEN` (DEPRECATED)**: checked BEFORE Graph as always, but only valid if `OCAPPS_PHOTOS_USER` is configured, since it maps to the legacy oc_id resolved with the app token (deprecation WARN at startup). Without `USER` it is a config error (the token no longer has an identity).
  - **Signed video (`/api/video/{id}`)**: a capability URL with no session, as before. The HMAC signature is only generated after verifying ownership in `video-url`, and the stream resolves the owner from the id (ids are global) to serve with that owner's DAV session.
  - **Thumbs**: a shared on-disk cache with no collisions (the key includes the href, which contains the user's space UUID).
- Result: a single Graph cache shared by the three modules (a web user who opens news and photos validates once against Graph), with all three modules multi-tenant.

### 6.3 Middleware

`auth.Middleware(validator, next)` with the current ocnews semantics (accepts `Authorization: Basic` and `Bearer`, 401 with `WWW-Authenticate`). Photos loses its own `validOpenCloudSession`, so its `withAuth` moves to the shared middleware plus exemptions (signed `/ocphotos-api/api/video/`) plus the own-token check.

---

## 7. Quick-wins to fold in during the merge

| # | Task | Source file | Detail |
|---|---|---|---|
| Q1 | `OCNEWS_OPENCOLOUD_URL` typo | ocnews `config.go:29,50` | Disappears in favour of `OCAPPS_OPENCLOUD_URL`, and the legacy (typo) name is still read with a warning (§3.2). |
| Q2 | `geo.Geocoder.last` data race | ocphotos `geo.go:22,46-53` | Add a `mu sync.Mutex` around the throttle (read and update `last` under the lock). Detectable with `go test -race` plus a new test with two concurrent `Name()` calls. |
| Q3 | Hardcoded media secret fallback | ocphotos `api.go:318-329` | `mediaSecret()` moves to `common/cred.LoadOrCreateSecret(<photos>/mediasecret)`, which **returns an error if `rand.Read` fails**, so the photos module starts `failed` (503) instead of signing predictable URLs with `"ocphotos-fallback-secret"`. |
| Q4 | ffmpeg | `app/Dockerfile`, deploy | (a) the new ocapps Dockerfile includes `ffmpeg` in the runtime stage, (b) at photos startup: `exec.LookPath("ffmpeg")`, and if it is missing, `log.Warn` "ffmpeg not found: video posters degraded (the original will be served)" (not fatal, since the rest works, whereas today it fails silently with a redirect or 502), (c) document it in `deploy/README.md` as the **only system dependency**. |
| Q5 | `rescanCh` with buffer 1 | ocphotos `api.go:750-756`, `main.go` | Raise the buffer to 4 and log drops (`"scan queue full: dropped"`), keeping the 20 s throttle in the consumer. |
| Q6 | Retire the legacy PWA | ocphotos `app/src`, `app/info.md`, `app/Dockerfile`, `docker-compose.yml`, `main.go:177-182,200-210` (`spaHandler`, `WEB_DIR`) | Remove from the ocphotos repo, and port none of it into ocapps. A defined `WEB_DIR` logs a warn "ignored". |
| Q7 | Dead ocphotos endpoints | `api.go:62-93` | Candidates: `GET /api/thumb` (by path), `GET /api/assets/{id}/hls/{file}`, `GET /api/assets/{id}`. **Before deleting**: re-grep over `ocphotos/ocphotos/src` (extension) and `app/src` (which disappears in Q6) at the current commit. The audit marks them as having no consumer, but that has to be revalidated at port time. If confirmed: delete the handler and the routes. Note that `/api/video/{id}` (signed stream) **is used** and stays. If HLS goes, `internal/video` is left for... nothing, so evaluate deleting `video.go` and the `hls/` cache (document the decision, and ffmpeg would still be used for posters in `thumb.VideoPoster`). |
| Q8 | CORS | the 3 | Unified in `httpx.CORS` (§4.4). |
| Q9 | Single `slog.SetDefault` | ocnews `main.go:50` | Only in `cmd/ocapps/main.go`, with packages receiving the logger through their constructor (a pattern already dominant). |
| Q10 | Dockerfile/Go toolchain | ocphotos `app/Dockerfile` (`golang:1.24-alpine` vs `go 1.26.4`) | Pin `golang:1.26-alpine` in the new ocapps Dockerfile. |

---

## 8. Milestone-based implementation plan

Dependencies: **H0 -> H1 -> {H2, H3, H4} -> H5 -> H6**. H2/H3/H4 can run in parallel across subagents (disjoint directories, and the only contention point is `go.mod`, which H0 already leaves complete). Recommended order with less parallelism: H2 (notes, the smallest, validates `common` early) -> H4 (news, the longest) -> H3 (photos, with the quick-wins).

| Milestone | Content | Acceptance criteria | Risks |
|---|---|---|---|
| **H0 - Skeleton** | `ocapps` repo, unified `go.mod` (deps §2.3, `third_party/goexif` + replace), minimal `cmd/ocapps/main.go` (healthz only), CI (vet, `test -race -shuffle=on`, mod tidy check, golangci-lint, govulncheck, inheriting the ocnews workflow extended to the whole module), `.goreleaser.yaml` (`CGO_ENABLED=0`, unique version/commit/date ldflags), Dockerfile (`golang:1.26-alpine` + `ffmpeg` runtime). | `CGO_ENABLED=0 go build ./...` OK, CI green, the binary starts and answers `/healthz`. | Low. |
| **H1 - common** | All the packages of §2.1 with their new tests (§10.2): config (full legacy table), auth (validator, middleware, policies), store (Open, Migrate, Baseline), httpx, log, cred, netguard, imgproxy (merge), webdav (generalized port of `internal/dav`). | `go test -race ./internal/common/...` green, with coverage of the config legacy paths. | Merging the two imgproxy/netguard packages: verify that HMAC signatures generated with the old secret still validate (the algorithm does not change, only the package), with a fixed-vector test. |
| **H2 - Port notes** | Copy `internal/{api,store,attachments}` to `internal/notes/...`, rewrite imports, adopt `common/{config,auth,store,httpx,imgproxy,cred}`, `module.go` with `Run/Register/Healthy`. Tests travel along. | `go test -race ./internal/notes/...` green (the 6 existing test files, 963 lines), manual smoke test: note CRUD plus OCS capabilities against a real instance. | `store.Open(dataDir, owner)` changes signature (it receives the shared `*sql.DB`), so keep the pre-migration backup and the owner backfill. |
| **H3 - Port photos** | Copy `internal/{api,store,index,exif,thumb,video,geo,phash}` to `internal/photos/...`, move the scan loop (today closures in main.go) into `module.go`, routes under `/ocphotos-api/`, auth to the shared middleware plus SingleTenant plus own token, quick-wins Q2, Q3, Q4, Q5 and Q7 (re-grep first), migration baseline (§5.3). | Photos suite green with `-race` (including the new geo race test), baseline test over a real `memories.db` fixture, and the ffmpeg tests still self-skipping without ffmpeg. | The modernc/sqlite bump 1.38 -> 1.56, enabling `foreign_keys(1)`, and Q7 (endpoint deletion) which needs a fresh verification against the extension. |
| **H4 - Port news** | Copy the 14 internal packages to `internal/news/...`, shadow-user glue in `internal/news/auth.go`, scheduler/websub/notify untouched, OCS stub verification (§4.2) and removal, `/api/me|users` routes with exact patterns. | `go test -race ./internal/news/...` green (31 test files), the news extension `check:types`/build unchanged, and the OCS payload diff documented. | It is the largest module (7,657 lines) and risks drifting if H1 changed APIs, which is mitigated by fixing the common signatures in H1. |
| **H5 - Unified wiring** | Full `cmd/ocapps/main.go`: shared config -> per-module init with degradation (§4.5) -> single mux -> `Run(ctx)` supervisor with recover -> graceful shutdown (10 s) -> aggregated healthz/readyz. | Startup integration tests (a module forced to fail gives 503 in its namespace, 200 in the others and readyz 503) and global `go test -race ./...` green. | Degradation behaviour is new code with no precedent in the repos, so cover it with tests. |
| **H6 - Deployment and migration** | `deploy/`: systemd unit, `env.example`, proxy snippets, `migrate.sh` (§5.4) with `--dry-run`, deployment and rollback README, deprecation notices in the READMEs of the 3 app repos. | Rehearsal on a staging instance with copies of the 3 real databases: the §9.4 checklist complete and the rollback rehearsed. | The proxy change (§4.3) is the only step the binary cannot revert, so coordinate it with the systemd cutover. |

**Cross-cutting gates per milestone**: `CGO_ENABLED=0` build, `go vet ./...`, `go test -race -shuffle=on ./...`, and for those touching a contract (H2-H5) also the build and `check:types` of the corresponding extension in its own repo (unmodified) as proof that the contract is intact.

---

## 9. systemd and deployment

### 9.1 Single unit `/etc/systemd/system/opencloud-apps.service`

```ini
[Unit]
Description=OpenCloud Apps (news+notes+photos unified backend)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=ocapps
Group=ocapps
EnvironmentFile=/etc/ocapps/env
ExecStart=/usr/local/bin/ocapps
Restart=on-failure
RestartSec=5s
StateDirectory=ocapps
StateDirectoryMode=0700
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/ocapps
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

Note: `Restart=on-failure` only acts if the whole process dies, and a failed module restarts nothing (by design, §4.5), so the alert is `readyz` 503.

### 9.2 `/etc/ocapps/env` (example, 640 root:ocapps)

```bash
OCAPPS_OPENCLOUD_URL=https://cloud.example.com
OCAPPS_LISTEN_ADDR=127.0.0.1:8096
OCAPPS_DATA_DIR=/var/lib/ocapps
OCAPPS_LOG_LEVEL=info
OCAPPS_AUTH_MODE=opencloud
# news
OCAPPS_NEWS_PUBLIC_URL=https://cloud.example.com/index.php/apps/news/api/v1-3
OCAPPS_NEWS_RETENTION_DAYS=90
OCAPPS_NEWS_NTFY_TOPIC=
# notes
OCAPPS_NOTES_OWNER=
# photos
OCAPPS_PHOTOS_USER=admin
OCAPPS_PHOTOS_APP_TOKEN=<app-token>
OCAPPS_PHOTOS_TOKEN=<own-token-optional>
OCAPPS_PHOTOS_SCAN_ROOT=Fotos
OCAPPS_PHOTOS_SCAN_EVERY=30m
```

### 9.3 Proxy changes

Today (inferred from the audits and the deploy README): `/index.php/apps/news/*` and `/api/*` -> ocnews :8094, `/index.php/apps/notes/*` and `/ocs/*` -> ocnotes :8100, and `/ocphotos-api/` -> ocphotos :8097 **with strip**. Goal: **everything to the same upstream without strip**:

```nginx
# /index.php/apps/news/ and /index.php/apps/notes/ and /ocs/ as today (no strip),
# only the upstream changes:
location /index.php/apps/news/  { proxy_pass http://127.0.0.1:8096; }
location /index.php/apps/notes/ { proxy_pass http://127.0.0.1:8096; }
location /ocs/                  { proxy_pass http://127.0.0.1:8096; }
location /api/me                { proxy_pass http://127.0.0.1:8096; }   # news extension contract
# photos: REMOVE the trailing slash that stripped the prefix:
location /ocphotos-api/         { proxy_pass http://127.0.0.1:8096; }   # before: http://...:8097/
```

(Replicate the `Authorization` and `Host` headers and the long timeouts for `/ocphotos-api/api/video/` for Range/streaming, the same as today.) **Decision**: direct routing by public prefixes, with no new `/ocnews-api/` or `/ocnotes-api/` prefixes, since they would break the Nextcloud News and Iotas client contracts.

### 9.4 Verification checklist on an instance

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
sqlite3 /var/lib/ocapps/news/ocnews.db 'PRAGMA user_version;'    # 18
sqlite3 /var/lib/ocapps/notes/notes.db 'PRAGMA user_version;'    # 2
sqlite3 /var/lib/ocapps/photos/memories.db 'PRAGMA user_version;' # 1 (baseline applied)
# From the outside (through the proxy, same origin as the web app):
curl -s -H "Authorization: Bearer $TOK" https://<host>/ocphotos-api/api/stats
curl -su user:app-token https://<host>/index.php/apps/news/api/v1-3/feeds
```

Final functional verification: open the three apps in the OpenCloud web UI and sync an external client (news-android or Iotas).

---

## 10. Tests

### 10.1 Port of the existing suite

- The `_test.go` files travel **with their packages** (news: 31 files, notes: 6, photos: 8) and only their imports change. Assertions are not rewritten, as they are the safety net of the port.
- Global gate: `go test -race -shuffle=on ./...` in CI (inherited from ocnews). `-race` is especially valuable now that three schedulers and loops live in one process.
- Tests that use real SQLite (the notes/photos stores with tmp or memory) work the same through `common/store.Open`.
- The photos tests that skip without ffmpeg (`video_test.go`, `thumb/video_test.go`) keep that behaviour, and a CI job with ffmpeg can be added to actually run them.
- Extension: no changes, but its `build` and `check:types` (where it exists, and adding the script to ocnotes is a quick-win in the app repo) are used as the contract gate in H2-H5.

### 10.2 Minimum new tests for `common`

| Package | Test |
|---|---|
| `config` | Table-driven: for each row of §3.2, new value > legacy > default, a deprecation warning logged when the legacy is used, `OCNOTES_GRAPH_URL` mapping to the root (strip `/graph/v1.0/me`), and rejection of invalid durations. |
| `auth` | `httptest.Server` simulating Graph `/me`: (1) a cache hit does not issue a request within the TTL, (2) singleflight: N concurrent validations with the same token produce a single Graph request, (3) 401 cached for 30 s, (4) MultiTenant vs SingleTenant policies (a different id is rejected), (5) Basic and Bearer resolve to the same user through `oc_id`. |
| `store` | `Migrate` applies in order and sets `user_version`, re-opening is a no-op, `Baseline` over a copy of the real `memories.db` fixture verifies tables and columns and sets 1, and a faulty migration rolls back leaving `user_version` intact. |
| `httpx` | CORS: preflight 204 with no auth, exact union headers, `WriteJSON` sets Content-Type and code. |
| `cred` | `LoadOrCreateSecret` creates with 0600, reuses, **fails** on an invalid file length, plus a fixed AES-GCM vector. |
| `imgproxy` | Fixed HMAC vector: a URL signed with the old ocnews algorithm and secret validates against the unified proxy (signature compatibility guarantee). |

### 10.3 New integration tests (H5)

- Startup with one module forced to fail (for instance `OCAPPS_DATA_DIR` pointing at a read-only photos subdirectory): its routes give 503, the others 200, `/readyz` 503, and the process stays alive.
- Route registration: a table of all the public paths of §4.1 against the real mux (guaranteeing no namespace steps on another, including the uniqueness of `/ocs/v2.php/cloud/user`).

---

## Annex A. Reference source files (for the coders)

| Topic | File |
|---|---|
| news wiring (startup order reference) | `ocnews/backend/cmd/ocnews/main.go` |
| photos wiring (scan closures -> module.go) | `ocphotos/app/server-go/cmd/photos-service/main.go` |
| notes wiring | `ocnotes/backend/main.go` |
| auth base to port | `ocnews/backend/internal/auth/{validator,auth}.go` |
| base migration runner | `ocnews/backend/internal/store/store.go` (`Open` + `migrate`) |
| notes backup and backfill | `ocnotes/backend/internal/store/migrate.go` |
| photos baseline (current ALTERs) | `ocphotos/app/server-go/internal/store/sqlite.go:113-131` |
| news CORS and routes | `ocnews/backend/internal/api/server.go` |
| photos CORS, session and secret | `ocphotos/app/server-go/internal/api/api.go` |
| WebDAV client to generalize | `ocphotos/app/server-go/internal/dav/client.go` |
| full legacy env sets | `ocnews/backend/internal/config/config.go`, `ocnotes/backend/internal/config/config.go`, `ocphotos .../main.go:46-59` |

## Annex B. Assumptions to confirm before H6

1. **Real production proxy configuration**: this design assumes (from the ocphotos `deploy/README.md` and the hardcoded routes in news) that the proxy does not strip `/index.php/...` or `/ocs/` today but does strip `/ocphotos-api/`. Confirm by reading the real config before writing the final snippets.
2. **Real ports and data dirs in production** (the ocphotos deploy README mentions `:8097` and `/var/lib/ocphotos`, while news/notes are assumed to be `/var/lib/ocnews` and `/var/lib/ocnotes`), so adjust `migrate.sh` with the real paths of the deployment.
3. **OCS payloads** of the two `/ocs/v2.php/cloud/user` handlers (§4.2): a mandatory diff in H4.
4. **Dead photos endpoints** (Q7): re-grep against the extension at the port commit before deleting.
5. modernc.org/sqlite 1.56 plus `foreign_keys(1)` over the live photos database: validated by its suite, but the first staging start must be followed by the §9.4 checklist.
