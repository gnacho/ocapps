# Auditoría read-only de `ocnews` (repo: `/mnt/agents/work/opencloud/ocnews`)

Último commit: `433c58d` (19-Ago-2026, PR #45 "shortcuts, rules, searches, ntfy, share, scraper, cluster, subfolders, websub", **+4.290 líneas en 66 ficheros**). Nada modificado.

---

## 1. Métricas reales vs. plan

| Métrica | Plan (16-Sep-2026) | Real | Δ |
|---|---|---|---|
| Go total | 8.269 líneas / 50 ficheros | **11.595 líneas / 79 ficheros** | +40% líneas, +29 ficheros |
| Go sin tests | — | 7.657 líneas / 48 ficheros | — |
| Go tests | — | 3.938 líneas / 31 ficheros `_test.go` | — |
| TS+Vue extensión | 2.012 líneas | **2.715 líneas / 3 ficheros** (+5 de `vite.config.ts`) | +35% |

- **Discrepancia clara**: las cifras del plan no cuadran con nada medible hoy. Lo más cercano es "Go sin tests" (7.657/48), pero aun así descuadra en ~612 líneas y 2 ficheros. Curioso: el PR #45 ya estaba mergeado un mes antes de la fecha del plan, así que el plan probablemente midió mal o sobre un árbol distinto.
- Todo el Go vive en `backend/` (no hay vendor). Extensión: `extension/src/views/NewsApp.vue` (**2.368 líneas, monolítico**), `extension/src/api.ts` (248), `extension/src/index.ts` (99). Hay además `landing/` (marketing, JS estático) y `assets/` (screenshots), ajenos al producto.

## 2. Mapa del backend

- **Entrada**: `backend/cmd/ocnews/main.go` (158 l.). Módulo `github.com/gnacho/ocnews/backend`, Go 1.25. Arranque: `config.Load()` fail-fast → `store.Open` (SQLite+migraciones) → validator (local|opencloud) → HTTP + scheduler con shutdown gracioso.
- **Paquetes `internal/` (18)**: `api` (2.743 l. sin tests), `store` (1.929), `auth` (342), `feed` (380), `scheduler` (233), `refresher` (184), `config`, `cred`, `extract`, `favicon`, `i18n`, `imgproxy`, `netguard`, `notify`, `privacy`, `rules`, `sanitize`, `websub`.
- **Base API**: `/index.php/apps/news/api/v1-3` (`internal/api/server.go:26`). Rutas en `internal/api/routes.go`, públicas en `server.go:53-89`, y `/api/*` en `internal/api/users.go:22-32`.

**Endpoints CON consumidor en la extensión** (todos vía `extension/src/api.ts`; verificado con grep de cada método `api.*` en `NewsApp.vue`):
`/folders` (GET/POST/PUT/DELETE + `/read`), `/feeds` (GET/POST/DELETE, POST `/move`, POST `/rename`, `/read`, `/credentials`, `/filter` GET/POST/DELETE, `/rules` GET/POST/DELETE, `/retention` GET/POST, `/scraper` GET/POST, `/discover`), `/items` (GET, `/search`, `/{id}/full`, `/read`, `/{id}/read|unread|star|unstar`, `/{id}/share` POST/DELETE), `/export|opml` y `/import/opml`, `/refresh`, `/searches` (+`/{id}/items`), `/auto-read`, `/api/me/settings` GET/PUT, `/api/me/rules` GET/PUT. Públicos consumidos **indirectamente**: `/img` (los bodies se reescriben server-side, `api/imgrewrite.go`), `/favicon/{hash}` (`faviconLink`, `api/feeds.go:29`), `/share/{token}` (navegadores anónimos), `/websub/` (hubs), `/healthz`.

**Endpoints SIN consumidor en la extensión (candidatos a recorte F3)** — existen para compat con clientes Nextcloud (news-android, spec v1.3):
1. `GET /version`, `GET /status`, `GET /user` (routes.go:7-9)
2. Variantes **PUT** de `/feeds/{id}/move` y `/rename` (routes.go:23-27; la extensión usa POST)
3. `GET /items/updated` (routes.go:44)
4. Marcado múltiple: POST+PUT `/items/{read,unread,star,unstar}/multiple` (routes.go:58-63)
5. Updater API admin: `GET /feeds/update`, `/cleanup/before-update`, `/cleanup/after-update`, `/feeds/all` (routes.go:40, 70-72)
6. API usuarios admin/perfil: `GET|PUT /api/me`, `PUT /api/me/password`, CRUD `/api/users*` (users.go:22-32)
7. Stub `GET /ocs/v2.php/cloud/user` (server.go:85)

## 3. Dependencias compartibles para F2

- **Auth** (`internal/auth/`, 342 l.): **sí valida contra Graph API** — `OpenCloudValidator` hace `GET {OCNEWS_OPENCOLOUD_URL}/graph/v1.0/me` con Basic o Bearer OIDC (`validator.go:114-122`), crea "shadow users" unificados por `oc_id`, caché positiva TTL 5 min. Alternativa `LocalValidator` bcrypt. Middleware Basic/Bearer genérico (`auth.go:32`). **Altamente reutilizable** por ocphotos.
- **Cliente WebDAV**: **no existe** (grep "webdav": 0 resultados).
- **SQLite**: driver `modernc.org/sqlite` (CGO-free, v1.56.0), WAL, `SetMaxOpenConns(1)`, migraciones embed versionadas con `PRAGMA user_version` (`store/store.go`). **18 migraciones** (`store/migrations/001..018`). Tablas: `users, folders, feeds, items, item_full, feed_filter, user_settings, feed_rules, global_rules, saved_searches, auto_read, shared_items, websub`.
- **Genérico y reutilizable**: `netguard` (anti-SSRF, 74 l.), `sanitize` (bluemonday, 51), `cred` (AES-256-GCM, 93), `imgproxy` (firmas HMAC, 247), `i18n` (ES/EN, 207), helpers HTTP (`writeJSON/errorStatus/decodeBody`, server.go:114-163), `config` (patrón env fail-fast).
- **Específico de news**: todo `feed`, `refresher`, `scheduler`, `rules`, `websub`, `notify`, `extract`, `favicon`, `store` (modelos) y los handlers `api/*`.
- **Config/logging**: todo por env (no flags): prefijo `OCNEWS_*` + genéricas `AUTH_USER`/`AUTH_PASS`. **Bug relevante para F2: la env var es `OCNEWS_OPENCOLOUD_URL` (doble O, "OPENCLOUD" mal escrito)** — `config.go:29` y `:50`. Logging: `log/slog` JSON con `slog.SetDefault` en main.

## 4. Lógica de servidor "intocable" — existe y tamaño

- **Fetch/parseo RSS en servidor**: `internal/feed/feed.go` (380 l., gofeed, discover, auth por feed) + `internal/refresher/refresher.go` (184) + `internal/scheduler/scheduler.go` (233, refresco adaptativo + retención + renovación websub). Total ≈ **800 líneas**.
- **Scraper por selector CSS**: `api/scraper.go` (66) + `andybalholm/cascadia`; **full-content**: `internal/extract/extract.go` (123, go-readability).
- **WebSub**: `internal/websub/websub.go` (60) + `api/websub.go` (111) + `store/websub.go` (101) + migración 018 ≈ 272 l.
- **ntfy**: `internal/notify/notify.go` (86) + topic por usuario en `user_settings`.
- **Rules** (regex block/keep): `rules/rules.go` (245) + `store/rules.go` (135). **Cluster de duplicados**: migración 016 + lógica en `store/items.go`/`api/items.go`. **Share público**: `api/share.go` (112) + `store/shared.go` (78).
- Pipeline de ingesta adicional: `privacy` (153), `sanitize` (51), `imgproxy` (247), `favicon` (153).

## 5. Tests y build

- **Go**: 31 ficheros `_test.go` (3.938 l.), buena cobertura por paquete. **Sin Makefile**. Build vía `.goreleaser.yaml` (linux amd64/arm64, `CGO_ENABLED=0`, ldflags version/commit/date).
- **CI** (`.github/workflows/ci.yml`): `go vet`, `go test -race -shuffle=on`, `go mod tidy` check, golangci-lint (solo issues nuevos), gosec SARIF (no bloquea), govulncheck (bloquea). `release.yml` aparte. **CI solo cubre `backend/`**.
- **Extensión**: scripts `build` (vite), `check:types` (vue-tsc), `lint` (eslint), `test:unit` (vitest) — **pero no existe ningún fichero de test** (`*.test.ts/spec.ts`: 0 resultados). Sin CI de extensión.

## 6. Riesgos para la fusión F2

1. **Env vars que colisionan**: `AUTH_USER`/`AUTH_PASS` sin prefijo (bootstrap admin) chocarían con ocphotos al compartir proceso; `OCNEWS_OPENCOLOUD_URL` (typo) debería unificarse a una var común tipo `OPENCLOUD_URL`.
2. **Rutas genéricas compartidas**: la base `/index.php/apps/news/...` es segura, pero `/api/`, `/ocs/v2.php/cloud/user`, `/healthz` y el CORS `ACAO *` (server.go:82-88, 100-112) son genéricos y colisionarán en un mux único con ocphotos. Base path duplicada como constante en Go (`server.go:26`) y TS (`api.ts:4`).
3. **SQLite**: migraciones propias con `PRAGMA user_version` por fichero; fichero fijo `ocnews.db` (`config.DBPath()`). Fusión → o dos ficheros SQLite o namespacing de migraciones; `user_version` es único por DB.
4. **Global state controlado pero presente**: `slog.SetDefault` (main.go:50) — un solo logger global en binario fusionado; vars `version/commit/date` por ldflags (una sola identidad de versión); mapas de paquete (`i18n.messages`, regexes) son read-only, sin problema. No hay singletons mutables: el estado vive en structs inyectados (`Server`, `Store`, `OpenCloudValidator` con caché mutex) → buen punto de partida.
5. **Rol admin implícito**: el primer usuario creado se hace admin (`validator.go:192-195`) — semántica a coordinar si ocphotos tiene su propio concepto de admin sobre la misma tabla `users`.
6. **Nombre de módulo**: `github.com/gnacho/ocnews/backend` habrá que renombrarse para la librería común.
7. **WebSub** requiere `OCNEWS_PUBLIC_URL` (callback público) — configuración de despliegue a preservar en el binario fusionado.
8. **Extensión monolítica** (NewsApp.vue 2.368 l.) y sin tests: cualquier recorte F3 de endpoints debe validarse a mano contra los 45 métodos `api.*` usados (inventario completo en §2).
