# Read-only audit: `ocphotos` (OpenCloud)

Audited repo: `/mnt/agents/work/opencloud/ocphotos` (HEAD `9f931df`). Nothing modified.

## 1. Real metrics vs. plan

| Metric | Plan | Real | Discrepancy |
|---|---|---|---|
| Go files | 26 | **24** | -2 |
| Go lines | 5,626 | **5,068** | -558 (-10%) |
| TS+Vue lines (extension) | 3,190 | **2,805** (18 files) | -385 (-12%) |

Real Go breakdown (`app/server-go`):
- **First-party: 18 files / 3,253 lines**, of which **505 are tests** (8 `*_test.go` files). Real production code: **2,748 lines in 10 files**.
- **Vendored: 6 files / 1,815 lines** in `third_party/goexif` (a fork of `rwcarlsen/goexif`, through a `replace` in `go.mod:25`). The plan probably counted this as first-party or measured another commit.

Real TS+Vue breakdown (`ocphotos/`): 12 `.vue` (2,051 lines) + 4 `.ts` in src (749) + `vite.config.ts`/`shims.d.ts` (7). The 10 largest files: `usePhotoLibrary.ts` (520), `TimelineView.vue` (348), `ViewerOverlay.vue` (232), `index.ts` (227), `AlbumsView.vue` (221).

**Note**: there is also `app/src` (the legacy React PWA, see §5) with **7,498 TS/TSX lines across 66 files** (53 of them are shadcn `components/ui`). If the plan included this in "TS+Vue", the real total would be about 10,300 lines. If it did not, the extension alone is 2,805.

## 2. Backend map

**Module**: `github.com/opencloud-memories/photos-service` (`app/server-go/go.mod`), Go 1.26.4.
**Entry point**: `cmd/photos-service/main.go` (210 lines). Sequence: `loadConfig` (env) -> `store.Open` (SQLite) -> `dav.New` (Basic user+app-token) -> `MeID` + `ListDrives` (Graph) -> `thumb.New`, `index.NewScanner`, `exif.NewWorker`, `geo.New`, `video.New` -> `api.New` -> scan goroutine (initial scan plus a `SCAN_EVERY` ticker plus an on-demand rescan channel throttled to 20 s) -> `http.ServeMux`: `/api/` and `/healthz` -> API handler, and `/` -> the **legacy static PWA** if `WEB_DIR` has an `index.html` (`spaHandler`, main.go:177-182, 200-210).

**Packages** (`internal/`): `api` (760 lines), `store` (814 plus tests), `dav` (293), `exif` (156), `thumb` (144), `video` (111), `geo` (97), `index` (87), `phash` (76).

**Endpoints** (registered in `internal/api/api.go:62-93`) and their consumers:

| Method + path | Consumer | Used? |
|---|---|---|
| GET `/healthz` | - | no (ops only) |
| GET `/api/stats` | `usePhotoLibrary.ts:212` (`syncNow`) | yes |
| GET `/api/assets` (keyset pagination, `favorites`, `archived`, `q`) | `usePhotoLibrary.ts:165,254,261,467,477` + legacy PWA | yes |
| GET `/api/assets/{id}` | - | **no consumer** (neither extension nor PWA) |
| POST `/api/assets/{id}/favorite` / `{id}/archive` | `usePhotoLibrary.ts:340,249` | yes |
| GET `/api/duplicates` | `usePhotoLibrary.ts:265` | yes |
| GET `/api/assets/{id}/thumb` (includes the ffmpeg video poster) | `usePhotoLibrary.ts:303` | yes |
| GET `/api/assets/{id}/hls/{file}` | - | **no consumer** (dead HLS transcoding, no frontend calls it) |
| POST `/api/assets/{id}/video-url` | `usePhotoLibrary.ts:315` | yes |
| GET `/api/video/{id}` (Range, HMAC signature) | `<video src>` through a signed URL (`ViewerOverlay.vue:38,126-132`) | yes |
| GET `/api/thumb` (by path, no index) | - | **no consumer** (only cited in `app/server-go/README.md:26` and `deploy/README.md:88`, and the extension does NOT call it despite what the deploy README says) |
| GET `/api/assets/{id}/original` | `usePhotoLibrary.ts:327` + legacy PWA | yes |
| GET `/api/memories/on-this-day`, `/api/memories/highlights`, `/api/timeline/calendar` | `usePhotoLibrary.ts:361,471,243` | yes |
| GET `/api/geo`, `/api/places` | `usePhotoLibrary.ts:355,441` | yes |
| GET/POST/DELETE `/api/tags...`, `/api/assets/{id}/tags...` | `usePhotoLibrary.ts:415-434` | yes |
| GET `/api/folders` | `usePhotoLibrary.ts:437` | yes |
| GET/POST/PATCH/DELETE `/api/albums...` | `usePhotoLibrary.ts:371-412` | yes |
| POST `/api/admin/rescan` | `usePhotoLibrary.ts:187,209` | yes |

**Base URL**: the extension calls everything under the hardcoded `/ocphotos-api` prefix (`usePhotoLibrary.ts:7`), which a reverse proxy strips towards the service (`deploy/README.md:86-87`). The backend itself **hardcodes the prefix** when signing video URLs: `"/ocphotos-api/api/video/%d?..."` (api.go:342).

## 3. Untouchable features: location and dependencies

| Feature | File | Size | External dependencies |
|---|---|---|---|
| **EXIF by ranges** | `internal/exif/worker.go` (156 lines): `dav.GetRange` requests only the **first 256 KB** (`rangeBytes`, worker.go:22,65) and decodes with the vendored goexif | +1,815 vendored lines | `third_party/goexif` (a fork of `rwcarlsen/goexif`, `go.mod:25`), pure Go with no cgo. Note: `LensModel` (0xA434) is not mapped (worker.go:89) |
| **HEIC thumbnails** | `internal/thumb/thumb.go` (144 lines): `image.Decode` with a blank import of **`github.com/gen2brain/h265/heic`** (thumb.go:24), a **pure Go HEVC decoder with no cgo and no libvips**. JPEG q80 cache on disk keyed by sha1(href\|etag\|size) | 144 lines plus a test with an in-memory HEIC encoder (`heic_test.go`, no fixtures) | `gen2brain/h265 v0.2.2`, `golang.org/x/image` |
| **phash** | `internal/phash/phash.go` (76 lines): dHash 9x8 to 64 bits, Hamming distance, grouping at <= 8 (api.go:300). Computed from the cached thumbnail, without downloading the original (api.go:413-428) | 76 lines plus a test | None (stdlib + x/image) |
| **Video / ffmpeg** | `internal/video/video.go` (111 lines): on-demand HLS with `exec ffmpeg -c:v libx264 ... -f hls` (video.go:72-77), the video poster in `thumb.VideoPoster` (thumb.go:50-92, `ffmpeg -ss 1 -frames:v 1`), and progressive streaming with Range and no ffmpeg (`api.go:347-378` + `dav.DownloadRange`) | about 250 lines in total | **External `ffmpeg` binary** in PATH. **It is NOT in the Dockerfile** (`app/Dockerfile` runtime stage only installs `ca-certificates tzdata`), so inside the container the posters fall back to a redirect to the original (api.go:448) and HLS returns 502. It is not mentioned in `deploy/README.md` either |

Confirmed: the 4 features exist and are the ones the plan declares untouchable. None of them uses cgo, and the only system dependency is **ffmpeg**.

## 4. Shareable dependencies for F2 (merge with ocnews)

| Piece | Location | Generic? | Notes |
|---|---|---|---|
| **Graph auth** | `dav.MeID`/`ListDrives` (Basic user+app-token, client.go:63-138) and `api.validOpenCloudSession` (validates the web session Bearer against `GET /graph/v1.0/me` and requires `id == ocUserID`, api.go:124-158) | **Yes, almost all of it** | Dual scheme: own token (`MEMORIES_TOKEN`) or host session. Single-tenant: rejects other users. Generic except the single-tenant policy (which ocnews presumably shares). No cache, as every request validates against Graph with a 10 s timeout, which is a candidate for improvement in the shared library |
| **WebDAV client** | `internal/dav/client.go` (293 lines): PROPFIND depth=1, GetRange, Download, DownloadRange, `SpaceFileURL` resolution, Graph drives | **Yes, 100%** | Basic auth with an app token. The `imageExt`/`videoExt` extension lists (client.go:28-35) are photos-specific, so extract them into options |
| **SQLite** | `internal/store/sqlite.go` (814 lines), driver **`modernc.org/sqlite` v1.38.0, pure Go with no cgo**. `Open` with WAL, `busy_timeout(10000)`, `synchronous(NORMAL)` and `SetMaxOpenConns(1)` (sqlite.go:113-118) | **Open and migrations: yes, schema: no** | Tables: `assets` (indexes on `taken_at`, `geo`), `scan_state`, `albums`, `album_assets`, `asset_tags`, `geocode` (sqlite.go:16-79). Migrations: 2 idempotent hot `ALTER TABLE` statements (`is_archived`, `phash`, sqlite.go:122-130) with **no `schema_version` table**, a fragile pattern to generalise |
| **HTTP helpers** | `withCORS`, `writeJSON`, `etagFor` (api.go:186-202, 468) | Partially | `Allow-Origin: *` CORS with Bearer, to review during the merge |
| **Logging** | `log/slog` JSON to stdout (main.go:62), injected through constructors in every package | Yes (pattern) | No custom library, easy to unify |
| **Config** | `loadConfig` through env (main.go:46-59): `LISTEN_ADDR` (:9210), `OC_BASE_URL`, `OC_USER`, `OC_APP_TOKEN`, `SCAN_ROOT` ("Fotos"), `SCAN_EVERY` (5m), `DATA_DIR`, `WEB_DIR`, `MEMORIES_TOKEN` | Pattern yes, **names no** | No CLI flags, env only |

**Photos-specific** (do not move into the shared library): `index/scanner.go` (BFS per folder filtering media), `exif`, `thumb`, `video`, `phash`, `geo` (Nominatim, hardcoded user agent `ocphotos/0.1`, geo.go:60) and the whole `store` schema and tables.

## 5. Extension structure

- **Extension points used today** (`ocphotos/src/index.ts`, 227 lines): `defineWebApplication` with `appInfo`, 11 routes of its own (`/timeline`, `/memories`, `/explore`, `/albums`, `/places`, `/archive`, `/duplicates`, `/tags`, `/folders`, `/map`, `/favorites`), `navItems` (the native left sidebar, index.ts:133-200), `translations` (l10n/translations.json) and **a single `Extension`: `appMenuItem`** (the app switcher, index.ts:204-214).
- It does **NOT use** `sidebarPanel`, `app-registry` (the default app for file types) or any other extension point (grep: 0 matches). Clear candidates for F3:
  - **`sidebarPanel`**: there is no metadata panel of its own, since the EXIF only shows up as a "date, camera" caption in the viewer (`ViewerOverlay.vue:113-120`), even though the backend exposes camera, ISO, aperture, shutter speed, focal length and GPS. A details panel (metadata plus tags plus albums) is the obvious candidate. Also, the viewer is its own full-screen overlay instead of integrating with the host's native preview.
  - **`app-registry`**: register ocphotos as an image and video type handler to open HEIC files and photos from Files (today the user cannot jump from Files to the app).
- **Packaging**: `@opencloud-eu/extension-sdk` (`vite.config.ts`, 5 lines, name `web-app-ocphotos`), and `pnpm build` emits `dist/` with module federation (`manifest.json` + `remoteEntry-*.mjs`), per `deploy/README.md:12,40-43`. `dist/` is **not committed** (gitignored). Installation: copy to `WEB_ASSET_APPS_PATH/ocphotos`, add the entry to `apps.yaml` and restart (`deploy/README.md:16-43`). `dev/docker/opencloud/apps.yaml` only holds the example skeleton, not ocphotos.
- **`deploy/`**: only `README.md` (102 lines). It describes a native deployment (systemd, `/etc/ocphotos/env`, port **:8097**, which disagrees with the `:9210` default in main.go:49) and a `CGO_ENABLED=0 go build` build. **Its content is outdated**: it claims the extension calls `/ocphotos-api/api/thumb?path=...` (line 88) and that it uses `usePreviewService().loadPreview` (lines 56-59), and neither happens in the current code.
- **Legacy `app/` prototype**: **it does exist**, a **React 19** PWA with react-router 7, radix/shadcn (53 `ui/` components) and Tailwind 3 (`app/package.json`, `app/src` = 7,498 lines), served by the backend itself through `WEB_DIR`/`spaHandler`. It consumes the same API with `MEMORIES_TOKEN`. `app/info.md` also exists (notes on the shadcn scaffolding), and both are what the plan wants to retire. Note that retiring the PWA also means removing `spaHandler` and `WEB_DIR` from the backend, and the all-in-one `app/Dockerfile`/`docker-compose.yml` become obsolete.

## 6. Tests and build

- **Go**: 8 test files, 505 lines: `store` (sqlite, albums, archive, tags_folders, with real SQLite in memory or tmp), `phash`, `thumb/heic` (HEIC encode and decode in memory, no fixtures), `thumb/video` and `video` (**they skip if there is no ffmpeg** in PATH, `video_test.go:20-21`, `thumb/video_test.go`). There are no tests for `api`, `dav`, `exif`, `index` or `geo`.
- **Frontend**: `package.json` has `test:unit` (vitest + @vue/test-utils + happy-dom) but **0 test files**. `check:types` is `vue-tsc --noEmit` (the README claims it passes clean) and there is also `lint` (OpenCloud eslint config) and `format:check` (prettier). The legacy PWA has `build: tsc -b && vite build` plus eslint, with no tests.
- **CI**: **none** (no `.github/`, `.woodpecker` or `.drone`). The whole quality gate is manual and local.
- **Backend build**: `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"` (deploy/README.md:96). Note that `go.mod` requires `go 1.26.4` but `app/Dockerfile` uses `golang:1.24-alpine`, which relies on toolchain auto-download, so pin the version during the merge.

## 7. Risks for the F2 merge

1. **Mutable global state**:
   - `geo.Geocoder.last` (geo.go:22,46-53): a Nominatim throttle **with no mutex**, hence a data race under concurrent requests to `/api/places` (Go would catch it with `-race`).
   - `main.go`: `scanMu`/`lastScan` in main closures (properly scoped) and `rescanCh` with a buffer of 1 (rescan requests are silently dropped, api.go:750-756).
   - `api.Server.mediaSecret()` (api.go:318-329): reads or creates `DATA_DIR/mediasecret` with a **hardcoded fallback `"ocphotos-fallback-secret"`** if `rand.Read` fails, which would make the signed video URLs predictable in that case.
   - `main()` calls `os.Exit(1)` on startup errors, whereas in a merged binary a photos failure should not take ocnews down, so the wiring has to become a `Run(ctx) error`.
2. **Hardcoded routes**: the `/ocphotos-api` prefix in both the backend (video signature, api.go:342) and the frontend (`SERVICE_BASE`, usePhotoLibrary.ts:7), the default scan root `"Fotos"` (main.go:53), and the Nominatim user agent carrying a personal repo (geo.go:60). Merging means parametrizing the prefix (`/ocnews-api` + `/ocphotos-api` on the same mux?).
3. **Env vars that would collide**: ocphotos uses **generic unprefixed names** (`OC_BASE_URL`, `OC_USER`, `OC_APP_TOKEN`, `DATA_DIR`, `LISTEN_ADDR`, `SCAN_ROOT`, `SCAN_EVERY`, `WEB_DIR`, `MEMORIES_TOKEN`), while **ocnews already uses its own prefix** (`OCNEWS_PUBLIC_URL`, `OCNEWS_NTFY_TOPIC`, `OCNEWS_RETENTION_DAYS`, `AUTH_USER`, `AUTH_PASS`, grepped in `/mnt/agents/work/opencloud/ocnews/backend`). A direct collision on `OC_*`/`DATA_DIR`/`LISTEN_ADDR` if they share a process means unifying the schema (for instance `OCPHOTOS_*` or a single shared config). On top of that, `MEMORIES_TOKEN` is a confusing inherited name.
4. **SQLite migrations**: no versions table, just two `ALTER TABLE` statements tolerant of the "duplicate column" error (sqlite.go:122-130). Two services opening the same `.db` file would be a problem (single writer, `SetMaxOpenConns(1)`), so during the merge each domain needs its own `.db` file or a shared store with namespaced tables (`assets` is a very generic name).
5. **ffmpeg and HEIC in the binary**: HEIC is **pure Go** (`gen2brain/h265`) and SQLite is **pure Go** (`modernc.org/sqlite`), so the merged binary can still be static with `CGO_ENABLED=0`. The only system dependency is the **`ffmpeg` binary** (posters and HLS), which is **missing from the Dockerfile** and from the native deployment instructions today, so the merge inherits that packaging gap with silent degradation (a redirect to the original, and 502 on HLS, which nobody consumes anyway).
6. **Other**: `*` CORS (api.go:188), session validation against Graph with no per-request cache (api.go:124-158), where a shared middleware with a short cache would avoid duplicating latency in the merge, and dead code to prune before or after the merge: the `/api/thumb`, `/api/assets/{id}/hls/{file}` and `/api/assets/{id}` endpoints (no consumer), `spaHandler`/`WEB_DIR` (legacy PWA) and all of `app/src` plus `app/info.md`.

**Confidence**: high on metrics, endpoints, consumers (verified by grepping both frontends) and dependencies (go.mod plus imports). Minor uncertainty: the exact origin of the plan figures (an earlier commit? another counting criterion?) cannot be determined from the current working tree.
