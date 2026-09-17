# ocapps — backend unificado de las apps OpenCloud (news + notes + photos)

`ocapps` es un servicio Go único que sustituye a los tres backends Go de
las apps OpenCloud (`ocnews`, `ocnotes`, `ocphotos`): un solo binario
estático, un solo listener (`127.0.0.1:8096`), un solo unit systemd y un
solo proxy upstream, sirviendo los **mismos contratos de API** que los
tres servicios originales (News API v1.3, Notes API v1/v1.4 + OCS, y la
API de photos bajo `/ocphotos-api/`).

Especificación de diseño: [`docs/SPEC.md`](docs/SPEC.md).
Despliegue y migración desde los servicios antiguos:
[`deploy/README.md`](deploy/README.md).

## Arquitectura

Un único módulo Go (`github.com/gnacho/ocapps`) con tres módulos de
aplicación sobre una base común:

```
cmd/ocapps/            wiring: config común → validador Graph compartido →
                       init por módulo con degradación (D3) → mux único →
                       supervisor de loops con recover → shutdown gracioso
internal/
├── common/
│   ├── config/        env OCAPPS_* unificado + compat legacy (SPEC §3)
│   ├── auth/          validador Graph (caché + singleflight) + middleware
│   │                  + políticas MultiTenant (news/notes) / SingleTenant (photos)
│   ├── store/         apertura SQLite unificada + runner de migraciones
│   │                  (Migrate/Baseline; WAL, busy_timeout, foreign_keys)
│   ├── httpx/         WriteJSON, errores, CORS unificado (SPEC §4.4)
│   ├── module/        interfaz Module (Register/Run/Healthy) + handler 503
│   ├── log/           slog JSON con attr "module"
│   ├── cred/          secretos persistentes + AES-256-GCM (fail-loud)
│   ├── netguard/      anti-SSRF
│   ├── imgproxy/      proxy de imágenes con firmas HMAC
│   └── webdav/        cliente WebDAV/Graph generalizado
├── news/              News API v1-3 (/index.php/apps/news/api/v1-3/) + /api/me|users
├── notes/             Notes API v1/v1.4 + OCS (/ocs/v2.php/cloud/*)
└── photos/            /ocphotos-api/api/* (single-tenant, app-token + Bearer propio)
```

Decisiones clave (SPEC): **D2** — tres ficheros SQLite
(`<data>/news/ocnews.db`, `notes/notes.db`, `photos/memories.db`), sin
transformación de las BDs vivas (user_version 18/2/baseline→1); **D3** —
un módulo cuyo wiring falla queda `failed` (su namespace sirve 503) sin
tumbar el proceso: `GET /healthz` (200 siempre, detalle por módulo) y
`GET /readyz` (503 si algún módulo enabled falla) son la señal operativa.

## Quickstart de desarrollo

Requisitos: Go ≥ 1.26 (solo para build/test) y ffmpeg (opcional; solo lo
usa el módulo photos en runtime para pósters de vídeo/HLS).

```bash
# Build (binario estático, modernc.org/sqlite → sin CGO)
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 go build -o ocapps ./cmd/ocapps

# Tests (con -race; TZ=UTC para resultados deterministas)
TZ=UTC go test -race -shuffle=on ./...
go vet ./...

# Run con env mínima (modo local: solo news con auth bcrypt, sin OpenCloud)
OCAPPS_AUTH_MODE=local \
OCAPPS_NEWS_AUTH_USER=admin OCAPPS_NEWS_AUTH_PASS=admin \
OCAPPS_PHOTOS_ENABLED=false OCAPPS_NOTES_ENABLED=false \
OCAPPS_DATA_DIR=/tmp/ocapps-dev \
./ocapps

curl -s localhost:8096/healthz    # {"version":"dev",...,"modules":{...}}
```

Contra una instancia OpenCloud real (modo por defecto `opencloud`):

```bash
OCAPPS_OPENCLOUD_URL=https://cloud.example.com \
OCAPPS_PHOTOS_USER=<usuario> OCAPPS_PHOTOS_APP_TOKEN=<app-token> \
./ocapps
```

Docker (incluye ffmpeg en la etapa runtime): `docker build -t ocapps .`.
Releases: `.goreleaser.yaml` (linux/amd64+arm64, ldflags de versión).

## Configuración

Todo por variables de entorno `OCAPPS_*`. Precedencia: `OCAPPS_*` >
variable legacy (de los servicios antiguos, soportadas con warning durante
2 versiones menores, hasta 0.3.0) > default. Tabla completa con legacy y
notas: SPEC §3.2 y [`deploy/env.example`](deploy/env.example).

| Variable | Default | Descripción |
|---|---|---|
| `OCAPPS_OPENCLOUD_URL` | *(oblig. con auth opencloud)* | Raíz del servidor OpenCloud |
| `OCAPPS_LISTEN_ADDR` | `127.0.0.1:8096` | Listener único |
| `OCAPPS_DATA_DIR` | `/var/lib/ocapps` | Raíz de datos (`<dir>/{news,notes,photos}`) |
| `OCAPPS_LOG_LEVEL` | `info` | `debug\|info\|warn\|error` |
| `OCAPPS_AUTH_MODE` | `opencloud` | `opencloud` (Graph) \| `local` (bcrypt, solo news) |
| `OCAPPS_NEWS_ENABLED` / `OCAPPS_NOTES_ENABLED` / `OCAPPS_PHOTOS_ENABLED` | `true` | Apagar un módulo sin desinstalar |
| `OCAPPS_NEWS_FEED_INTERVAL` | `15m` | Refresco de feeds (≤ `MAX_GAP`) |
| `OCAPPS_NEWS_MAX_GAP` | `6h` | Hueco máximo antes de resync |
| `OCAPPS_NEWS_FETCH_TIMEOUT` | `20s` | Timeout HTTP de descarga de feeds |
| `OCAPPS_NEWS_RETENTION_DAYS` | `90` | Retención de items (0 = off) |
| `OCAPPS_NEWS_NTFY_URL` / `OCAPPS_NEWS_NTFY_TOPIC` | `https://ntfy.sh` / `""` | Notificaciones push ntfy |
| `OCAPPS_NEWS_PUBLIC_URL` | `""` | URL pública del API (callbacks WebSub) |
| `OCAPPS_NEWS_AUTH_USER` / `OCAPPS_NEWS_AUTH_PASS` | `""` | Admin bootstrap en modo `local` |
| `OCAPPS_NOTES_OWNER` | `""` | Backfill de `user` en filas vacías (migración 002) |
| `OCAPPS_PHOTOS_USER` / `OCAPPS_PHOTOS_APP_TOKEN` | *(oblig. si photos enabled)* | Usuario y app-token OpenCloud (single-tenant) |
| `OCAPPS_PHOTOS_TOKEN` | `""` | Bearer estático propio (ex-`MEMORIES_TOKEN`) |
| `OCAPPS_PHOTOS_SCAN_ROOT` | `Fotos` | Carpeta raíz de escaneo |
| `OCAPPS_PHOTOS_SCAN_EVERY` | `5m` | Intervalo de re-escaneo |

Validación (SPEC §3.3): config común inválida = fatal (no arranca); config
de módulo inválida = ese módulo `failed` (503) y el resto sigue.

## Despliegue y migración

Ver **[`deploy/README.md`](deploy/README.md)**: requisitos (ffmpeg como
única dependencia de sistema en runtime), build, instalación del binario +
unit systemd + `/etc/ocapps/env`, **cambio de proxy** (⚠ `/ocphotos-api/`
pasa a enrutarse SIN strip), migración de datos con
[`deploy/migrate.sh`](deploy/migrate.sh) (idempotente, `--dry-run`
primero), checklist de verificación y rollback. Avisos para los README de
los repos de app en [`deploy/repo-notices/`](deploy/repo-notices/).

## Estado del proyecto

Implementación por hitos (SPEC §8): **H0–H6 completados** — esqueleto y CI
(H0), base `common` (H1), ports de notes/photos/news (H2–H4), wiring
unificado con degradación D3 (H5) y artefactos de despliegue/migración
(H6, `deploy/`). Gates: `CGO_ENABLED=0 go build ./...`,
`go test -race -shuffle=on ./...` verdes. Pendiente antes de producción:
ensayo en staging con copias de las BDs reales y del rollback (checklist
SPEC §9.4) — ver `deploy/README.md`.
