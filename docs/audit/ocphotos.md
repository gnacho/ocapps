# Auditoría read-only: `ocphotos` (OpenCloud)

Repo auditado: `/mnt/agents/work/opencloud/ocphotos` (HEAD `9f931df`). No se ha modificado nada.

## 1. Métricas reales vs. plan

| Métrica | Plan | Real | Discrepancia |
|---|---|---|---|
| Ficheros Go | 26 | **24** | −2 |
| Líneas Go | 5.626 | **5.068** | −558 (−10 %) |
| Líneas TS+Vue (extensión) | 3.190 | **2.805** (18 ficheros) | −385 (−12 %) |

Desglose Go real (`app/server-go`):
- **First-party: 18 ficheros / 3.253 líneas**, de las cuales **505 son tests** (8 ficheros `*_test.go`). Código productivo real: **2.748 líneas en 10 ficheros**.
- **Vendored: 6 ficheros / 1.815 líneas** en `third_party/goexif` (fork de `rwcarlsen/goexif`, vía `replace` en `go.mod:25`). El plan probablemente contó esto como propio o midió otro commit.

Desglose TS+Vue real (`ocphotos/`): 12 `.vue` (2.051 líneas) + 4 `.ts` de src (749) + `vite.config.ts`/`shims.d.ts` (7). Los 10 ficheros más grandes: `usePhotoLibrary.ts` (520), `TimelineView.vue` (348), `ViewerOverlay.vue` (232), `index.ts` (227), `AlbumsView.vue` (221).

**Ojo**: existe además `app/src` (la PWA legacy React, ver §5) con **7.498 líneas TS/TSX en 66 ficheros** (53 de ellos son `components/ui` de shadcn). Si el plan incluía esto en "TS+Vue", la cifra real total sería ~10.300 líneas; si no, la extensión sola son 2.805.

## 2. Mapa del backend

**Módulo**: `github.com/opencloud-memories/photos-service` (`app/server-go/go.mod`), Go 1.26.4.
**Entrada**: `cmd/photos-service/main.go` (210 líneas). Secuencia: `loadConfig` (env) → `store.Open` (SQLite) → `dav.New` (Basic user+app-token) → `MeID` + `ListDrives` (Graph) → `thumb.New`, `index.NewScanner`, `exif.NewWorker`, `geo.New`, `video.New` → `api.New` → goroutine de scan (inicial + ticker `SCAN_EVERY` + canal de rescans bajo demanda con throttle de 20 s) → `http.ServeMux`: `/api/` y `/healthz` → handler API; `/` → **PWA estática legacy** si `WEB_DIR` tiene `index.html` (`spaHandler`, main.go:177-182, 200-210).

**Paquetes** (`internal/`): `api` (760 l.), `store` (814 + tests), `dav` (293), `exif` (156), `thumb` (144), `video` (111), `geo` (97), `index` (87), `phash` (76).

**Endpoints** (registrados en `internal/api/api.go:62-93`) y consumo:

| Método + path | Consumidor | ¿Usado? |
|---|---|---|
| GET `/healthz` | — | No (ops) |
| GET `/api/stats` | `usePhotoLibrary.ts:212` (`syncNow`) | ✅ |
| GET `/api/assets` (paginación keyset, `favorites`, `archived`, `q`) | `usePhotoLibrary.ts:165,254,261,467,477` + PWA legacy | ✅ |
| GET `/api/assets/{id}` | — | **❌ sin consumidor** (ni extensión ni PWA) |
| POST `/api/assets/{id}/favorite` / `{id}/archive` | `usePhotoLibrary.ts:340,249` | ✅ |
| GET `/api/duplicates` | `usePhotoLibrary.ts:265` | ✅ |
| GET `/api/assets/{id}/thumb` (incl. póster de vídeo ffmpeg) | `usePhotoLibrary.ts:303` | ✅ |
| GET `/api/assets/{id}/hls/{file}` | — | **❌ sin consumidor** (transcodificación HLS muerta: ningún frontend la llama) |
| POST `/api/assets/{id}/video-url` | `usePhotoLibrary.ts:315` | ✅ |
| GET `/api/video/{id}` (Range, firma HMAC) | `<video src>` vía URL firmada (`ViewerOverlay.vue:38,126-132`) | ✅ |
| GET `/api/thumb` (por path, sin índice) | — | **❌ sin consumidor** (solo citado en `app/server-go/README.md:26` y `deploy/README.md:88`; la extensión NO lo llama pese a lo que dice el README de deploy) |
| GET `/api/assets/{id}/original` | `usePhotoLibrary.ts:327` + PWA legacy | ✅ |
| GET `/api/memories/on-this-day`, `/api/memories/highlights`, `/api/timeline/calendar` | `usePhotoLibrary.ts:361,471,243` | ✅ |
| GET `/api/geo`, `/api/places` | `usePhotoLibrary.ts:355,441` | ✅ |
| GET/POST/DELETE `/api/tags…`, `/api/assets/{id}/tags…` | `usePhotoLibrary.ts:415-434` | ✅ |
| GET `/api/folders` | `usePhotoLibrary.ts:437` | ✅ |
| GET/POST/PATCH/DELETE `/api/albums…` | `usePhotoLibrary.ts:371-412` | ✅ |
| POST `/api/admin/rescan` | `usePhotoLibrary.ts:187,209` | ✅ |

**Base URL**: la extensión llama todo bajo el prefijo hardcodeado `/ocphotos-api` (`usePhotoLibrary.ts:7`), que un reverse proxy strip-pea hacia el servicio (`deploy/README.md:86-87`). El propio backend **hardcodea el prefijo** al firmar URLs de vídeo: `"/ocphotos-api/api/video/%d?…"` (api.go:342).

## 3. Funciones intocables — localización y dependencias

| Función | Fichero | Tamaño | Dependencias externas |
|---|---|---|---|
| **EXIF por rangos** | `internal/exif/worker.go` (156 l.): `dav.GetRange` pide solo los **primeros 256 KB** (`rangeBytes`, worker.go:22,65) y decodifica con goexif vendored | +1.815 l. vendored | `third_party/goexif` (fork `rwcarlsen/goexif`, `go.mod:25`); Go puro, sin cgo. Nota: `LensModel` (0xA434) no mapeado (worker.go:89) |
| **Miniaturas HEIC** | `internal/thumb/thumb.go` (144 l.): `image.Decode` con registro en blanco de **`github.com/gen2brain/h265/heic`** (thumb.go:24) — decodificador HEVC **Go puro, sin cgo ni libvips**. Caché JPEG q80 en disco por sha1(href\|etag\|size) | 144 l. + test con encoder HEIC en memoria (`heic_test.go`, sin fixtures) | `gen2brain/h265 v0.2.2`, `golang.org/x/image` |
| **phash** | `internal/phash/phash.go` (76 l.): dHash 9×8→64 bits, Hamming, agrupación ≤8 (api.go:300). Se calcula desde la miniatura cacheada, sin bajar el original (api.go:413-428) | 76 l. + test | Ninguna (stdlib + x/image) |
| **Vídeo / ffmpeg** | `internal/video/video.go` (111 l.): HLS bajo demanda con `exec ffmpeg -c:v libx264 … -f hls` (video.go:72-77); póster de vídeo en `thumb.VideoPoster` (thumb.go:50-92, `ffmpeg -ss 1 -frames:v 1`); streaming progresivo con Range sin ffmpeg (`api.go:347-378` + `dav.DownloadRange`) | ~250 l. totales | **Binario externo `ffmpeg`** en PATH. ⚠️ **NO está en el Dockerfile** (`app/Dockerfile` etapa runtime solo instala `ca-certificates tzdata`): en el contenedor los pósters caen al redirect al original (api.go:448) y HLS devuelve 502. Tampoco se menciona en `deploy/README.md` |

Confirmado: las 4 funciones existen y son las que el plan declara intocables. Ninguna usa cgo; la única dependencia de sistema es **ffmpeg**.

## 4. Dependencias compartibles para F2 (fusión con ocnews)

| Pieza | Ubicación | ¿Genérica? | Notas |
|---|---|---|---|
| **Auth contra Graph** | `dav.MeID`/`ListDrives` (Basic user+app-token, client.go:63-138) y `api.validOpenCloudSession` (valida el Bearer de la sesión web contra `GET /graph/v1.0/me` y exige `id == ocUserID`, api.go:124-158) | **Sí, casi toda** | Esquema dual: token propio (`MEMORIES_TOKEN`) o sesión del host. Single-tenant: rechaza otros usuarios. Genérica salvo la política single-tenant (que ocnews presumiblemente comparte). Sin caché: cada request valida contra Graph (10 s timeout) — candidato a mejora en la librería común |
| **Cliente WebDAV** | `internal/dav/client.go` (293 l.): PROPFIND depth=1, GetRange, Download, DownloadRange, resolución `SpaceFileURL`, Graph drives | **Sí, 100 %** | Auth Basic con app-token; listas de extensiones `imageExt`/`videoExt` (client.go:28-35) son específicas de photos → extraer a opciones |
| **SQLite** | `internal/store/sqlite.go` (814 l.), driver **`modernc.org/sqlite` v1.38.0 — Go puro, sin cgo**. `Open` con WAL, `busy_timeout(10000)`, `synchronous(NORMAL)`, `SetMaxOpenConns(1)` (sqlite.go:113-118) | **Open/migraciones: sí; esquema: no** | Tablas: `assets` (+índices `taken_at`, `geo`), `scan_state`, `albums`, `album_assets`, `asset_tags`, `geocode` (sqlite.go:16-79). Migraciones: 2 `ALTER TABLE` idempotentes en caliente (`is_archived`, `phash`, sqlite.go:122-130) — **sin tabla `schema_version`**; patrón frágil a generalizar |
| **Helpers HTTP** | `withCORS`, `writeJSON`, `etagFor` (api.go:186-202, 468) | Parcialmente | CORS `Allow-Origin: *` con Bearer — repasar en fusión |
| **Logging** | `log/slog` JSON a stdout (main.go:62), inyectado por constructor en todos los paquetes | Sí (patrón) | Sin librería propia; fácil de unificar |
| **Config** | `loadConfig` por env (main.go:46-59): `LISTEN_ADDR` (:9210), `OC_BASE_URL`, `OC_USER`, `OC_APP_TOKEN`, `SCAN_ROOT` ("Fotos"), `SCAN_EVERY` (5m), `DATA_DIR`, `WEB_DIR`, `MEMORIES_TOKEN` | Patrón sí; nombres **no** | Sin flags CLI, solo env |

**Específico de photos** (no subir a la librería común): `index/scanner.go` (BFS por carpetas filtrando media), `exif`, `thumb`, `video`, `phash`, `geo` (Nominatim, UA hardcodeado `ocphotos/0.1`, geo.go:60), y todo el esquema/tablas de `store`.

## 5. Estructura de la extensión

- **Puntos de extensión usados hoy** (`ocphotos/src/index.ts`, 227 l.): `defineWebApplication` con `appInfo`, 11 rutas propias (`/timeline`, `/memories`, `/explore`, `/albums`, `/places`, `/archive`, `/duplicates`, `/tags`, `/folders`, `/map`, `/favorites`), `navItems` (sidebar izquierdo nativo, index.ts:133-200), `translations` (l10n/translations.json) y **un solo `Extension`: `appMenuItem`** (app switcher, index.ts:204-214).
- **NO usa** `sidebarPanel`, `app-registry` (app por defecto para tipos de fichero), ni ningún otro extension point (grep: 0 coincidencias). Candidatos claros para F3:
  - **`sidebarPanel`**: no existe panel de metadatos propio — el EXIF solo aparece como caption "fecha · cámara" en el visor (`ViewerOverlay.vue:113-120`), pese a que el backend expone cámara/ISO/apertura/obturación/focal/GPS. Un panel de detalles (metadatos + tags + álbumes) es el candidato obvio; además el visor es un overlay propio a pantalla completa en vez de integrarse con el preview nativo del host.
  - **`app-registry`**: registrar ocphotos como handler de tipos imagen/vídeo para abrir ficheros HEIC/fotos desde Files (hoy el usuario no puede saltar de Files a la app).
- **Empaquetado**: `@opencloud-eu/extension-sdk` (`vite.config.ts`, 5 l., name `web-app-ocphotos`); `pnpm build` emite `dist/` con module federation (`manifest.json` + `remoteEntry-*.mjs`), según `deploy/README.md:12,40-43`. `dist/` **no está commiteado** (gitignored). Instalación: copiar a `WEB_ASSET_APPS_PATH/ocphotos` + entrada en `apps.yaml` + restart (`deploy/README.md:16-43`). `dev/docker/opencloud/apps.yaml` solo contiene el skeleton de ejemplo (no ocphotos).
- **`deploy/`**: solo `README.md` (102 l.). Describe despliegue nativo (systemd, `/etc/ocphotos/env`, puerto **:8097** — discrepa con el default `:9210` de main.go:49) y build `CGO_ENABLED=0 go build`. **Contenido desactualizado**: afirma que la extensión llama `/ocphotos-api/api/thumb?path=…` (línea 88) y que usa `usePreviewService().loadPreview` (líneas 56-59) — ninguna de las dos cosas ocurre en el código actual.
- **Prototipo legacy `app/`**: **Sí existe** — PWA **React 19** + react-router 7 + radix/shadcn (53 componentes `ui/`) + Tailwind 3 (`app/package.json`, `app/src` = 7.498 líneas), servida por el propio backend vía `WEB_DIR`/`spaHandler`. Consume la misma API con `MEMORIES_TOKEN`. Existe **`app/info.md`** (notas del scaffolding shadcn) — ambos son lo que el plan quiere retirar. Ojo: retirar la PWA implica eliminar también `spaHandler` y `WEB_DIR` del backend, y el `app/Dockerfile`/`docker-compose.yml` all-in-one quedan obsoletos.

## 6. Tests y build

- **Go**: 8 ficheros de test, 505 líneas: `store` (sqlite, albums, archive, tags_folders — con SQLite real en memoria/tmp), `phash`, `thumb/heic` (encode+decode HEIC en memoria, sin fixtures), `thumb/video` y `video` (**se saltan si no hay ffmpeg** en PATH, `video_test.go:20-21`, `thumb/video_test.go`). No hay tests de `api`, `dav`, `exif`, `index`, `geo`.
- **Frontend**: `package.json` tiene `test:unit` (vitest + @vue/test-utils + happy-dom) pero **0 ficheros de test**; `check:types` = `vue-tsc --noEmit` (el README afirma que pasa limpio); `lint` (eslint config OpenCloud) y `format:check` (prettier). La PWA legacy tiene `build: tsc -b && vite build` + eslint, sin tests.
- **CI**: **ninguna** (sin `.github/`, `.woodpecker`, `.drone`). Todo el quality-gate es manual/local.
- **Build backend**: `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"` (deploy/README.md:96). ⚠️ `go.mod` exige `go 1.26.4` pero `app/Dockerfile` usa `golang:1.24-alpine` — depende del auto-download de toolchain; fijar versión en la fusión.

## 7. Riesgos para la fusión F2

1. **Estado global mutable**:
   - `geo.Geocoder.last` (geo.go:22,46-53): throttle de Nominatim **sin mutex** — data race con peticiones concurrentes a `/api/places` (Go lo detectaría con `-race`).
   - `main.go`: `scanMu`/`lastScan` en closures de main (bien acotado), `rescanCh` con buffer 1 (peticiones de rescan se pierden silenciosamente, api.go:750-756).
   - `api.Server.mediaSecret()` (api.go:318-329): lee/crea `DATA_DIR/mediasecret` con **fallback hardcodeado `"ocphotos-fallback-secret"`** si falla `rand.Read` — las URLs firmadas de vídeo serían predecibles en ese caso.
   - `main()` hace `os.Exit(1)` en errores de arranque — en un binario fusionado, un fallo de photos no debería tumbar ocnews: hay que convertir el wiring en un `Run(ctx) error`.
2. **Rutas hardcodeadas**: prefijo `/ocphotos-api` tanto en backend (firma de vídeo, api.go:342) como en frontend (`SERVICE_BASE`, usePhotoLibrary.ts:7); raíz de escaneo por defecto `"Fotos"` (main.go:53); UA de Nominatim con repo personal (geo.go:60). Al fusionar hay que parametrizar el prefijo (¿`/ocnews-api` + `/ocphotos-api` sobre el mismo mux?).
3. **Env vars que colisionarían**: ocphotos usa nombres **genéricos sin prefijo** (`OC_BASE_URL`, `OC_USER`, `OC_APP_TOKEN`, `DATA_DIR`, `LISTEN_ADDR`, `SCAN_ROOT`, `SCAN_EVERY`, `WEB_DIR`, `MEMORIES_TOKEN`); **ocnews ya usa prefijo propio** (`OCNEWS_PUBLIC_URL`, `OCNEWS_NTFY_TOPIC`, `OCNEWS_RETENTION_DAYS`, `AUTH_USER`, `AUTH_PASS` — grep en `/mnt/agents/work/opencloud/ocnews/backend`). Colisión directa en `OC_*`/`DATA_DIR`/`LISTEN_ADDR` si comparten proceso: unificar esquema (p. ej. `OCPHOTOS_*` o config común única). Además `MEMORIES_TOKEN` es un nombre heredado confuso.
4. **Migraciones SQLite**: sin tabla de versiones; dos `ALTER TABLE` con tolerancia al error "duplicate column" (sqlite.go:122-130). Dos servicios abriendo el mismo fichero .db sería un problema (single-writer, `SetMaxOpenConns(1)`); en la fusión cada dominio debe tener su propio .db o un store común con namespacing de tablas (`assets` es un nombre muy genérico).
5. **ffmpeg/HEIC en el binario**: HEIC es **Go puro** (`gen2brain/h265`), SQLite es **Go puro** (`modernc.org/sqlite`) → el binario fusionado puede seguir siendo estático `CGO_ENABLED=0`. La única dependencia de sistema es el **binario `ffmpeg`** (pósters + HLS), que hoy **falta en el Dockerfile** y en las instrucciones de despliegue nativo → la fusión hereda ese gap de empaquetado (degradación silenciosa: redirect al original / 502 en HLS, que además nadie consume).
6. **Otros**: CORS `*` (api.go:188); validación de sesión contra Graph sin caché por request (api.go:124-158) — al fusionar, un middleware común con caché corta evitaría duplicar latencia; `dead code` a podar antes/después de fusionar: endpoints `/api/thumb`, `/api/assets/{id}/hls/{file}`, `/api/assets/{id}` (sin consumidor), `spaHandler`/`WEB_DIR` (PWA legacy) y todo `app/src` + `app/info.md`.

**Confianza**: alta en métricas, endpoints, consumidores (verificado con grep sobre ambos frontends) y dependencias (go.mod + imports). Incertidumbre menor: el origen exacto de las cifras del plan (¿commit anterior? ¿conteo con otro criterio?) no es determinable desde el working tree actual.
