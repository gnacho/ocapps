# Read-only audit of `ocnews` (repo: `/mnt/agents/work/opencloud/ocnews`)

Last commit: `433c58d` (19-Aug-2026, PR #45 "shortcuts, rules, searches, ntfy, share, scraper, cluster, subfolders, websub", **+4,290 lines across 66 files**). Nothing modified.

---

## 1. Real metrics vs. plan

| Metric | Plan (16-Sep-2026) | Real | Delta |
|---|---|---|---|
| Total Go | 8,269 lines / 50 files | **11,595 lines / 79 files** | +40% lines, +29 files |
| Go without tests | - | 7,657 lines / 48 files | - |
| Go tests | - | 3,938 lines / 31 `_test.go` files | - |
| Extension TS+Vue | 2,012 lines | **2,715 lines / 3 files** (+5 from `vite.config.ts`) | +35% |

- **Clear discrepancy**: the plan figures do not match anything measurable today. The closest is "Go without tests" (7,657/48), but even that is off by about 612 lines and 2 files. Curious detail: PR #45 was already merged a month before the plan date, so the plan probably measured wrong or against a different tree.
- All the Go lives in `backend/` (there is no vendor directory). Extension: `extension/src/views/NewsApp.vue` (**2,368 lines, monolithic**), `extension/src/api.ts` (248), `extension/src/index.ts` (99). There is also `landing/` (marketing, static JS) and `assets/` (screenshots), unrelated to the product.

## 2. Backend map

- **Entry point**: `backend/cmd/ocnews/main.go` (158 lines). Module `github.com/gnacho/ocnews/backend`, Go 1.25. Startup: `config.Load()` fail-fast -> `store.Open` (SQLite plus migrations) -> validator (local|opencloud) -> HTTP plus scheduler with graceful shutdown.
- **`internal/` packages (18)**: `api` (2,743 lines without tests), `store` (1,929), `auth` (342), `feed` (380), `scheduler` (233), `refresher` (184), `config`, `cred`, `extract`, `favicon`, `i18n`, `imgproxy`, `netguard`, `notify`, `privacy`, `rules`, `sanitize`, `websub`.
- **API base**: `/index.php/apps/news/api/v1-3` (`internal/api/server.go:26`). Routes in `internal/api/routes.go`, public ones in `server.go:53-89`, and `/api/*` in `internal/api/users.go:22-32`.

**Endpoints WITH a consumer in the extension** (all through `extension/src/api.ts`, verified by grepping every `api.*` method in `NewsApp.vue`):
`/folders` (GET/POST/PUT/DELETE + `/read`), `/feeds` (GET/POST/DELETE, POST `/move`, POST `/rename`, `/read`, `/credentials`, `/filter` GET/POST/DELETE, `/rules` GET/POST/DELETE, `/retention` GET/POST, `/scraper` GET/POST, `/discover`), `/items` (GET, `/search`, `/{id}/full`, `/read`, `/{id}/read|unread|star|unstar`, `/{id}/share` POST/DELETE), `/export|opml` and `/import/opml`, `/refresh`, `/searches` (+`/{id}/items`), `/auto-read`, `/api/me/settings` GET/PUT, `/api/me/rules` GET/PUT. Public endpoints consumed **indirectly**: `/img` (the bodies are rewritten server-side, `api/imgrewrite.go`), `/favicon/{hash}` (`faviconLink`, `api/feeds.go:29`), `/share/{token}` (anonymous browsers), `/websub/` (hubs), `/healthz`.

**Endpoints WITHOUT a consumer in the extension (F3 trim candidates)**: they exist for compatibility with Nextcloud clients (news-android, spec v1.3):
1. `GET /version`, `GET /status`, `GET /user` (routes.go:7-9)
2. **PUT** variants of `/feeds/{id}/move` and `/rename` (routes.go:23-27, the extension uses POST)
3. `GET /items/updated` (routes.go:44)
4. Multiple marking: POST+PUT `/items/{read,unread,star,unstar}/multiple` (routes.go:58-63)
5. Admin updater API: `GET /feeds/update`, `/cleanup/before-update`, `/cleanup/after-update`, `/feeds/all` (routes.go:40, 70-72)
6. Admin/profile user API: `GET|PUT /api/me`, `PUT /api/me/password`, CRUD `/api/users*` (users.go:22-32)
7. `GET /ocs/v2.php/cloud/user` stub (server.go:85)

## 3. Shareable dependencies for F2

- **Auth** (`internal/auth/`, 342 lines): it **does validate against the Graph API**. `OpenCloudValidator` issues `GET {OCNEWS_OPENCOLOUD_URL}/graph/v1.0/me` with Basic or an OIDC Bearer (`validator.go:114-122`), creates unified "shadow users" by `oc_id`, and keeps a positive cache with a 5 min TTL. Alternative `LocalValidator` with bcrypt. Generic Basic/Bearer middleware (`auth.go:32`). **Highly reusable** by ocphotos.
- **WebDAV client**: **does not exist** (grep "webdav": 0 results).
- **SQLite**: `modernc.org/sqlite` driver (CGO-free, v1.56.0), WAL, `SetMaxOpenConns(1)`, embedded versioned migrations with `PRAGMA user_version` (`store/store.go`). **18 migrations** (`store/migrations/001..018`). Tables: `users, folders, feeds, items, item_full, feed_filter, user_settings, feed_rules, global_rules, saved_searches, auto_read, shared_items, websub`.
- **Generic and reusable**: `netguard` (anti-SSRF, 74 lines), `sanitize` (bluemonday, 51), `cred` (AES-256-GCM, 93), `imgproxy` (HMAC signatures, 247), `i18n` (ES/EN, 207), HTTP helpers (`writeJSON/errorStatus/decodeBody`, server.go:114-163), `config` (fail-fast env pattern).
- **News-specific**: all of `feed`, `refresher`, `scheduler`, `rules`, `websub`, `notify`, `extract`, `favicon`, `store` (models) and the `api/*` handlers.
- **Config/logging**: everything through env (no flags), prefix `OCNEWS_*` plus the generic `AUTH_USER`/`AUTH_PASS`. **Bug relevant to F2: the env var is `OCNEWS_OPENCOLOUD_URL` (double O, "OPENCLOUD" misspelled)**, in `config.go:29` and `:50`. Logging: `log/slog` JSON with `slog.SetDefault` in main.

## 4. "Untouchable" server logic: what exists and how big it is

- **RSS fetch and parsing on the server**: `internal/feed/feed.go` (380 lines, gofeed, discover, per-feed auth) + `internal/refresher/refresher.go` (184) + `internal/scheduler/scheduler.go` (233, adaptive refresh plus retention plus websub renewal). About **800 lines** in total.
- **Scraper by CSS selector**: `api/scraper.go` (66) + `andybalholm/cascadia`, and **full content**: `internal/extract/extract.go` (123, go-readability).
- **WebSub**: `internal/websub/websub.go` (60) + `api/websub.go` (111) + `store/websub.go` (101) + migration 018, about 272 lines.
- **ntfy**: `internal/notify/notify.go` (86) plus a per-user topic in `user_settings`.
- **Rules** (regex block/keep): `rules/rules.go` (245) + `store/rules.go` (135). **Duplicate cluster**: migration 016 plus the logic in `store/items.go`/`api/items.go`. **Public share**: `api/share.go` (112) + `store/shared.go` (78).
- Additional ingest pipeline: `privacy` (153), `sanitize` (51), `imgproxy` (247), `favicon` (153).

## 5. Tests and build

- **Go**: 31 `_test.go` files (3,938 lines), good coverage per package. **No Makefile**. Build through `.goreleaser.yaml` (linux amd64/arm64, `CGO_ENABLED=0`, version/commit/date ldflags).
- **CI** (`.github/workflows/ci.yml`): `go vet`, `go test -race -shuffle=on`, `go mod tidy` check, golangci-lint (new issues only), gosec SARIF (non-blocking), govulncheck (blocking). `release.yml` is separate. **CI only covers `backend/`**.
- **Extension**: scripts `build` (vite), `check:types` (vue-tsc), `lint` (eslint), `test:unit` (vitest), **but there is not a single test file** (`*.test.ts/spec.ts`: 0 results). No extension CI.

## 6. Risks for the F2 merge

1. **Colliding env vars**: the unprefixed `AUTH_USER`/`AUTH_PASS` (bootstrap admin) would clash with ocphotos when sharing a process, and `OCNEWS_OPENCOLOUD_URL` (typo) should be unified into a common variable such as `OPENCLOUD_URL`.
2. **Shared generic routes**: the `/index.php/apps/news/...` base is safe, but `/api/`, `/ocs/v2.php/cloud/user`, `/healthz` and the `ACAO *` CORS (server.go:82-88, 100-112) are generic and will collide in a single mux with ocphotos. The base path is duplicated as a constant in Go (`server.go:26`) and TS (`api.ts:4`).
3. **SQLite**: its own migrations with `PRAGMA user_version` per file and a fixed file name `ocnews.db` (`config.DBPath()`). Merging means either two SQLite files or namespaced migrations, since `user_version` is unique per database.
4. **Global state is controlled but present**: `slog.SetDefault` (main.go:50), a single global logger in a merged binary, `version/commit/date` vars from ldflags (a single version identity), and package maps (`i18n.messages`, regexes) which are read-only and harmless. There are no mutable singletons, as state lives in injected structs (`Server`, `Store`, `OpenCloudValidator` with a mutex-guarded cache), which is a good starting point.
5. **Implicit admin role**: the first user created becomes admin (`validator.go:192-195`), a semantic to coordinate if ocphotos has its own admin concept over the same `users` table.
6. **Module name**: `github.com/gnacho/ocnews/backend` will have to be renamed for the shared library.
7. **WebSub** requires `OCNEWS_PUBLIC_URL` (public callback), a deployment setting to preserve in the merged binary.
8. **Monolithic extension** (NewsApp.vue 2,368 lines) with no tests: any F3 endpoint trim must be validated by hand against the 45 `api.*` methods in use (full inventory in §2).
