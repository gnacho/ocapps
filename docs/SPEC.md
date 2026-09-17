# Diseño: servicio Go unificado `ocapps` (news + notes + photos)

**Estado**: propuesta de arquitectura (read-only). Audiencia: subagentes coder. Contexto necesario: este documento + los repos `ocnews`, `ocnotes`, `ocphotos` y las auditorías en `output/audit/`.

**Decisiones rápidas (TL;DR)**:

| # | Decisión | Resumen |
|---|---|---|
| D1 | Repo | Monorepo nuevo `ocapps`, **módulo Go único** (no go.work, no submódulos). Los 3 repos de app conservan extensión + Store; sus backends quedan congelados y se archivan tras la primera release unificada. |
| D2 | SQLite | **Tres ficheros `.db`** (uno por módulo) en subdirectorios de un `DATA_DIR` común. `PRAGMA user_version` por fichero sigue funcionando sin namespacing. |
| D3 | Aislamiento | Fallo de arranque de un módulo = 503 en sus rutas + log + `readyz` degradado; el proceso y los otros módulos siguen. Errores de config **común** sí son fatales. |
| D4 | Enrutado | Un `http.ServeMux`. Photos se monta bajo `/ocphotos-api/` y el proxy **deja de strip-pear** (cambio de 1 línea en el proxy, 0 en frontends). `/ocs/` pasa a ser exclusivo de notes; news retira su stub OCS (verificación previa obligatoria). |
| D5 | Auth | Un validador Graph en `common/auth` con caché 5 min + singleflight; política multiusuario (shadow users) para news/notes y single-tenant para photos, parametrizada. |
| D6 | Go/deps | `go 1.26`, `modernc.org/sqlite v1.56.0` unificado, `replace` a `third_party/goexif` interno. Binario `CGO_ENABLED=0`; única dependencia de sistema: **ffmpeg**. |

---

## 1. Estrategia de repo/módulo

### 1.1 Decisión: repo nuevo `ocapps` con un único módulo Go

```
github.com/<org>/ocapps          ← repo nuevo, SOLO backend
├── go.mod                       ← module github.com/<org>/ocapps (un único módulo)
├── cmd/ocapps/main.go
├── internal/common/...
├── internal/{news,notes,photos}/...
├── third_party/goexif           ← vendored (hoy replace en ocphotos)
└── deploy/...
```

**El código se porta (mueve) una vez** desde los tres repos; no se importa. Rechazadas las alternativas:

- **go.work con los 3 repos como módulos**: mantiene el código común triplicado (imgproxy, netguard, auth Graph, helpers HTTP existen hoy en copias casi idénticas en ocnews y ocnotes), el skew de versiones (`modernc.org/sqlite` 1.56 vs 1.38), la fuga del `replace github.com/rwcarlsen/goexif` de ocphotos, y tres `go.mod` que versionar/publicar. Un workspace no reduce duplicación, solo la convive en un build.
- **Submódulos git**: mismos problemas + peor DX (los coders trabajarían sobre 4 checkouts).
- **Importar los tres como paquetes**: los module paths actuales son heterogéneos (`github.com/gnacho/ocnews/backend`, `git.opencloud.example.com/gnacho/ocnotes/backend`, `github.com/opencloud-memories/photos-service`) y habría que reescribir imports igualmente; si hay que tocar imports, mejor hacerlo una sola vez hacia el layout definitivo.

### 1.2 Relación con los repos existentes (pensando en Fase 4 / Store)

Los tres repos públicos siguen siendo **la cara de cada app** en la Store:

| Repo | Contenido a futuro | Backend |
|---|---|---|
| `ocnews` | `extension/`, `landing/`, assets, README, metadata de Store | `backend/` se congela en el último commit, se añade aviso en README y se **archiva** tras la release unificada 0.1.0 |
| `ocnotes` | `extension/`, README, metadata | ídem (backend en `backend/`) |
| `ocphotos` | `ocphotos/` (extensión), `deploy/README.md`, metadata | `app/server-go/` congelado/archivado; `app/src` (PWA legacy), `app/info.md` y `app/Dockerfile` all-in-one se **eliminan** (quick-win Q6) |
| `ocapps` (nuevo) | backend unificado, `deploy/` (systemd, proxy, migración) | — |

**Fase 4 (Store)**: cada app se empaqueta por separado con su extensión + manifest que documenta como requisito el *companion server* `ocapps >= <versión>` (una sola instalación sirve a las tres apps; la app declara qué módulo usa). El empaquetado de la extensión no cambia (`vite build` en cada repo de app). El backend deja de distribuirse por app y pasa a ser un artefacto de release de `ocapps` (binario estático + unit systemd + script de migración).

**Consecuencia para coders**: los ports se hacen copiando ficheros y reescribiendo imports de forma mecánica (`github.com/gnacho/ocnews/backend/internal/X` → `github.com/<org>/ocapps/internal/news/X`, etc.). Los tests viajan con sus paquetes.

---

## 2. Layout de paquetes

```
ocapps/
├── go.mod                        # go 1.26; deps unificadas (ver §2.3)
├── cmd/
│   └── ocapps/
│       └── main.go               # wiring: config común → módulos → mux único → supervisor
├── internal/
│   ├── common/
│   │   ├── config/               # env unificado + legacy-compat (§3)
│   │   ├── auth/                 # validador Graph + caché + singleflight + middleware (§6)
│   │   ├── webdav/               # cliente WebDAV/Graph generalizado (port de ocphotos internal/dav)
│   │   ├── store/                # apertura SQLite + runner de migraciones (§5)
│   │   ├── httpx/                # WriteJSON, ErrorStatus, DecodeBody, CORS, preflight (§4.4)
│   │   ├── log/                  # slog JSON con attr "module"
│   │   ├── cred/                 # AES-256-GCM + carga/generación de secretos persistentes (fail-loud)
│   │   ├── netguard/             # anti-SSRF (merge de ocnews/netguard y ocnotes/netguard)
│   │   └── imgproxy/             # merge de ocnews/imgproxy y ocnotes/imgproxy (idénticos en espíritu)
│   ├── news/
│   │   ├── api/  store/  feed/  refresher/  scheduler/  websub/  rules/
│   │   ├── notify/  extract/  favicon/  sanitize/  privacy/  i18n/
│   │   └── module.go             # wiring del módulo (Run/RegisterRoutes/Healthy)
│   ├── notes/
│   │   ├── api/  store/  attachments/
│   │   └── module.go
│   └── photos/
│       ├── api/  store/  index/  exif/  thumb/  video/  geo/  phash/
│       └── module.go             # el loop de scan vive aquí (hoy closures en main.go)
├── third_party/goexif/           # replace interno
└── deploy/
    ├── opencloud-apps.service
    ├── env.example
    ├── migrate.sh                # adopción de BDs existentes (§5.4)
    ├── proxy/                    # snippets nginx/caddy
    └── README.md
```

### 2.1 Qué entra en `common` (específico)

| Paquete | Origen | API clave (firmas) | Notas |
|---|---|---|---|
| `common/config` | los 3 `config` | `func Load() (*Config, error)`; `type Config struct { Common; News NewsConfig; Notes NotesConfig; Photos PhotosConfig }` | lectura `OCAPPS_*` con fallback legacy + warning (§3). Validación fail-fast **solo** de lo común. |
| `common/auth` | ocnews `internal/auth` (el más completo) | `type Credential struct{ Username, Password, Bearer string }`; `type User struct{ ID, Username, DisplayName, Email string }`; `type Policy interface { Admit(u *User) bool }`; `func NewGraphValidator(graphURL string, pol Policy, log *slog.Logger) *GraphValidator`; `func (v *GraphValidator) Validate(ctx context.Context, c Credential) (*User, bool)`; `func Middleware(v *GraphValidator, next http.Handler) http.Handler` | caché positiva 5 min (clave `sha256(token)` / `basic:user`), caché **negativa** 30 s (nuevo: evita martillear Graph ante 401 en bucle), `singleflight.Group` por clave. Políticas: `MultiTenant()` y `SingleTenant(ocID string)` (§6). |
| `common/webdav` | ocphotos `internal/dav` | `type Client struct{...}`; `func New(baseURL, user, appToken string) *Client`; `MeID(ctx)`, `ListDrives(ctx)`, `Propfind(ctx, href string, depth int)`, `GetRange(ctx, href string, off, n int64)`, `Download/DownloadRange(ctx, href)`, `SpaceFileURL(...)` | listas `imageExt`/`videoExt` salen del cliente a opciones: `type Options struct{ ImageExts, VideoExts []string }` pasadas al scanner de photos, no al cliente. |
| `common/store` | ocnews `internal/store` (Open+migrate) | `func Open(path string) (*sql.DB, error)` (pragmas unificados, §5.2); `func Migrate(db *sql.DB, fsys fs.FS, dir string) error` (runner `user_version`); `func Baseline(db *sql.DB, version int) error` (adopción photos, §5.3) | Cada módulo sigue teniendo su propio paquete `store` de dominio encima del `*sql.DB` común. |
| `common/httpx` | helpers de los 3 (`writeJSON`, `errorStatus`, `decodeBody`, `withCORS`) | `func WriteJSON(w http.ResponseWriter, code int, v any)`; `func ErrorStatus(w http.ResponseWriter, r *http.Request, err error)`; `func DecodeBody(r *http.Request, dst any) error`; `func CORS(next http.Handler) http.Handler`; `func Preflight() http.Handler` | CORS unificado (§4.4). `WriteJSON` fija `Content-Type` **antes** de `WriteHeader` (bug menor en ocphotos: lo fija pero no escribe código). |
| `common/log` | patrón de los 3 | `func New(level string) *slog.Logger`; `func Module(log *slog.Logger, name string) *slog.Logger` → `log.With("module", name)` | Un solo `slog.SetDefault` en main. |
| `common/cred` | ocnews `cred` + patrón secret de ambos imgproxy | `func LoadOrCreateSecret(path string) ([]byte, error)` (0600, ≥32 bytes, **error si `rand.Read` falla** — corrige el fallback hardcodeado de photos); `type Cipher struct{...}` AES-256-GCM | Reutilizado por: cred de feeds (news), imgsecret (news/notes), mediasecret (photos, Q3). |
| `common/netguard` | ocnews `netguard` (74 l.) + ocnotes | `func CheckURL(u *url.URL) error` / `func SafeClient() *http.Client` | Rechaza IPs privadas/loopback/link-local. Usado por imgproxy, feed fetcher y extractor. |
| `common/imgproxy` | ocnews + ocnotes (casi idénticos) | `func New(dataDir string, log *slog.Logger) (*Proxy, error)`; `func (p *Proxy) Sign(rawURL string) string`; `func (p *Proxy) Serve(w, r)` | Una instancia **por módulo** (cada una con su `imgsecret` e `imgcache` en su subdir) — las firmas HMAC quedan aisladas por módulo como hoy. |

### 2.2 Qué NO sube a common

`feed`, `refresher`, `scheduler`, `websub`, `rules`, `notify`, `extract`, `favicon`, `sanitize`, `privacy`, `i18n` (news); `attachments` (notes); `index`, `exif`, `thumb`, `video`, `geo`, `phash` y los stores de dominio. `sanitize`/`extract` solo los usa news; si photos los necesitara algún día, se promueven entonces.

### 2.3 Dependencias unificadas (go.mod)

```
module github.com/<org>/ocapps
go 1.26

require (
    github.com/PuerkitoBio/goquery v1.12.0            // news (scraper)
    github.com/andybalholm/cascadia v1.3.3            // news
    github.com/gen2brain/h265 v0.2.2                  // photos (HEIC, Go puro)
    github.com/go-shiori/go-readability v0.0.0-...    // news (full-content)
    github.com/microcosm-cc/bluemonday v1.0.27        // news (sanitize)
    github.com/mmcdole/gofeed v1.4.1                  // news
    github.com/rwcarlsen/goexif v0.0.0-2019...        // photos (EXIF) → replace
    golang.org/x/crypto v0.55.0                       // news (bcrypt local)
    golang.org/x/image v0.30.0                        // photos (thumbs/phash)
    golang.org/x/sync v0.x                            // NUEVO: singleflight (auth)
    modernc.org/sqlite v1.56.0                        // unificado (photos sube de 1.38 → 1.56)
)
replace github.com/rwcarlsen/goexif => ./third_party/goexif
```

Riesgo de subir photos a sqlite 1.56: bajo (mismo driver, API estable; los tests de store de photos con SQLite real lo validan). `go 1.26` por exigencia actual de photos (`go 1.26.4` en su go.mod); fijar la toolchain en CI/Docker (hoy `app/Dockerfile` usa `golang:1.24-alpine` con auto-download — quick-win implícito: imagen `golang:1.26-alpine`).

---

## 3. Esquema de configuración unificado

### 3.1 Política de compatibilidad (legacy)

1. **Precedencia**: `OCAPPS_*` nueva > variable legacy > default.
2. Si se usa una variable legacy: `log.Warn("env legacy en uso; migra a OCAPPS_*", "legacy", "OCNEWS_FEED_INTERVAL", "nueva", "OCAPPS_NEWS_FEED_INTERVAL")` — un warning por variable y por arranque.
3. Soporte de legacy durante **2 versiones menores** (introducido en 0.1.0 → eliminado en 0.3.0). En 0.2.0 el warning sube a `Error` visible pero no fatal; en 0.3.0 se ignoran las legacy.
4. Implementación: helper único en `common/config`:

```go
// get devuelve el valor de la var nueva; si no existe, la legacy (con warn);
// si ninguna, def. Registra cada legacy usada para el log resumen.
func (l *loader) get(nueva, legacy, def string) string
func (l *loader) dur(nueva, legacy string, def time.Duration) (time.Duration, error)
```

### 3.2 Tabla de mapeo completa

**Comunes (nuevas, sin equivalente o con múltiples equivalentes legacy):**

| Nueva | Default | Legacy (en orden de precedencia entre ellas) | Notas |
|---|---|---|---|
| `OCAPPS_OPENCLOUD_URL` | *(obligatoria)* | `OCNEWS_OPENCOLOUD_URL` (**typo histórico, doble O**), `OCNOTES_GRAPH_URL`†, `OC_BASE_URL` | Raíz del servidor OpenCloud. †`OCNOTES_GRAPH_URL` es la URL completa `.../graph/v1.0/me`: al mapearla se deriva la raíz (strip del sufijo `/graph/v1.0/me` si presente) y se loguea la transformación. |
| `OCAPPS_LISTEN_ADDR` | `127.0.0.1:8096` | `OCNEWS_ADDR`, `OCNOTES_ADDR`, `LISTEN_ADDR` | **Un solo listener.** Las legacy solo se usan para detectar despliegues viejos en logs; no se hereda el puerto (cada servicio tenía uno distinto). Documentado en migración. |
| `OCAPPS_DATA_DIR` | `/var/lib/ocapps` | `OCNEWS_DATA_DIR`, `OCNOTES_DATA_DIR`, `DATA_DIR` | Los subdirs por módulo se derivan: `<dir>/news`, `<dir>/notes`, `<dir>/photos` (§5.1). Si se define legacy por módulo, esa ruta se usa para ese módulo (transición suave). |
| `OCAPPS_LOG_LEVEL` | `info` | `OCNEWS_LOG_LEVEL` | debug\|info\|warn\|error. |
| `OCAPPS_AUTH_MODE` | `opencloud` | `OCNEWS_AUTH_MODE`, `OCNOTES_AUTH_MODE` | `opencloud` (Graph) \| `local` (bcrypt, solo news, para bootstrap/dev). Default cambia de `local` (ocnews) a `opencloud`: en despliegues reales ya es el modo usado. |
| `OCAPPS_NEWS_ENABLED` / `OCAPPS_NOTES_ENABLED` / `OCAPPS_PHOTOS_ENABLED` | `true` | — | Permite apagar un módulo sin desinstalar. |

**News (`OCAPPS_NEWS_*`):**

| Nueva | Default | Legacy | Notas |
|---|---|---|---|
| `OCAPPS_NEWS_FETCH_TIMEOUT` | `20s` | `OCNEWS_FETCH_TIMEOUT` | > 0 |
| `OCAPPS_NEWS_FEED_INTERVAL` | `15m` | `OCNEWS_FEED_INTERVAL` | > 0 |
| `OCAPPS_NEWS_MAX_GAP` | `6h` | `OCNEWS_MAX_GAP` | ≥ FEED_INTERVAL |
| `OCAPPS_NEWS_RETENTION_DAYS` | `90` | `OCNEWS_RETENTION_DAYS` | 0 = desactivada |
| `OCAPPS_NEWS_NTFY_URL` | `https://ntfy.sh` | `OCNEWS_NTFY_URL` | |
| `OCAPPS_NEWS_NTFY_TOPIC` | `""` | `OCNEWS_NTFY_TOPIC` | |
| `OCAPPS_NEWS_PUBLIC_URL` | `""` | `OCNEWS_PUBLIC_URL` | callbacks WebSub |
| `OCAPPS_NEWS_AUTH_USER` / `OCAPPS_NEWS_AUTH_PASS` | `""` | `AUTH_USER` / `AUTH_PASS` | bootstrap admin en modo `local`. Las genéricas sin prefijo desaparecen del esquema nuevo. |

**Notes (`OCAPPS_NOTES_*`):**

| Nueva | Default | Legacy | Notas |
|---|---|---|---|
| `OCAPPS_NOTES_OWNER` | `""` | `OCNOTES_OWNER` | backfill de `user` en filas vacías (migración 002) |

(Notes no necesita más: su addr/dataDir/graph pasan a comunes.)

**Photos (`OCAPPS_PHOTOS_*`):**

| Nueva | Default | Legacy | Notas |
|---|---|---|---|
| `OCAPPS_PHOTOS_USERS` | `""` | — | **(H8)** `alice:apptoken1,bob:apptoken2` — usuarios con scan periódico en background (app-token por usuario, solo en memoria). Entrada malformada = error del módulo (D3). |
| `OCAPPS_PHOTOS_USER` | `""` | `OC_USER` | *(opcional desde H8)* usuario del app-token legacy. Sigue usándose para (a) la identidad del Bearer estático y (b) el backfill del índice single-tenant (§5.3); se pliega en `PHOTOS_USERS` con WARN de deprecación. |
| `OCAPPS_PHOTOS_APP_TOKEN` | `""` | `OC_APP_TOKEN` | *(opcional desde H8)* app-token de OpenCloud (idem). |
| `OCAPPS_PHOTOS_TOKEN` | `""` | `MEMORIES_TOKEN` | **DEPRECATED (H8)**: Bearer estático machine-to-machine. Solo válido si `OCAPPS_PHOTOS_USER` está configurado (mapea al oc_id legacy); configurarlo sin `USER` es error de config del módulo. Los clientes deben usar el Bearer OIDC de OpenCloud. |
| `OCAPPS_PHOTOS_SCAN_ROOT` | `Fotos` | `SCAN_ROOT` | aplica igual a todos los usuarios |
| `OCAPPS_PHOTOS_SCAN_EVERY` | `5m` | `SCAN_EVERY` | |
| — | — | `WEB_DIR` | **Eliminada** (PWA legacy retirada, Q6). Si está definida: warn "ignorada". |

### 3.3 Reglas de validación

- **Fatal (config común)**: `OCAPPS_OPENCLOUD_URL` vacía con auth mode `opencloud`; `OCAPPS_LOG_LEVEL` inválido; `OCAPPS_DATA_DIR` no creable/escribible.
- **Degrada el módulo, no el proceso**: valores inválidos de `OCAPPS_NEWS_*` (durations) → news failed; `OCAPPS_PHOTOS_TOKEN` sin `OCAPPS_PHOTOS_USER` u `OCAPPS_PHOTOS_USERS` malformado → photos failed (H8: `PHOTOS_USER/APP_TOKEN` vacíos ya NO degradan — photos es multi-tenant y no exige usuario). Justificación en §4.5/D3.

---

## 4. Enrutado

### 4.1 Mux único con namespacing estricto

`cmd/ocapps/main.go` construye un `http.ServeMux` (Go 1.22+, patrones con método). Cada módulo registra **solo** bajo su namespace:

| Prefijo / ruta | Módulo | Auth | Origen |
|---|---|---|---|
| `/index.php/apps/news/api/v1-3/` | news | Basic/Bearer (salvo excepciones públicas de abajo) | contrato News API v1.3 |
| `/index.php/apps/news/api/v1-3/img` · `/favicon/` · `/share/` · `/websub/` | news | **público** (firma HMAC / token share / hub) | igual que hoy |
| `GET\|PUT /api/me/settings`, `GET\|PUT /api/me/rules`, `/api/me`, `/api/users*` | news | Basic/Bearer | hoy registrado como `/api/` en raíz del mux de ocnews; la extensión llama `/api/me/*` a la raíz del host (verificado en `extension/src/api.ts:177-197`) → **el proxy ya enruta `/api/*` a news; se mantiene** pero con patrones exactos, no catch-all |
| `/index.php/apps/notes/api/v1/` y `/v1.4/` | notes | Basic/Bearer; `GET /v1/img` público firmado | contrato Notes API |
| `/ocs/v2.php/cloud/capabilities` · `/ocs/v2.php/cloud/user` | notes | Basic/Bearer | **colisión resuelta**: ver §4.2 |
| `/ocphotos-api/api/` | photos | Bearer sesión / token propio; `GET /ocphotos-api/api/video/{id}` público firmado | **cambio de proxy**: deja de strip-pear (§4.3) |
| `GET /healthz` | common | público | liveness + estado por módulo (§4.5) |
| `GET /readyz` | common | público | 200 solo si todos los módulos enabled están healthy; 503 si no |

### 4.2 Colisión `/ocs/v2.php/cloud/user` (news stub vs notes real)

Hoy **ambos** sirven `/ocs/v2.php/cloud/user`: ocnews como stub para news-android (`server.go:85`) y ocnotes como handler real (`handleUserInfo`). En el mux unificado solo puede quedar uno.

**Decisión**: lo sirve **notes**; news elimina su stub. **Tarea de verificación previa obligatoria** (H4): hacer diff de los payloads OCS de ambos handlers (envelope `ocs.meta` + `ocs.data`: id, displayname, email). Si el stub de news incluye campos que news-android necesita y el de notes no emite, se extiende el handler de notes (un solo handler OCS sirve a news-android, Iotas y la web). Riesgo bajo: ambos derivan del mismo shadow user Graph.

### 4.3 Colisión `/api/` (news vs photos) y cambio de proxy

Hoy el proxy strip-pea `/ocphotos-api/` → photos ve `/api/...` (`deploy/README.md`: "trailing slash strips the prefix"). En el binario unificado `/api/` ya pertenece a news (`/api/me/*`), así que:

**Decisión**: el proxy pasa a enrutar `/ocphotos-api/` → `http://127.0.0.1:8096` **sin strip**, y photos registra sus rutas como `/ocphotos-api/api/...`. Cero cambios en frontends (la extensión ya llama `/ocphotos-api/api/...`) y el prefijo hardcodeado en la firma de vídeo (`"/ocphotos-api/api/video/%d"`, `api.go:342`) sigue siendo correcto tal cual. El prefijo se extrae a constante `photos.PublicPrefix = "/ocphotos-api"` usada tanto en el registro de rutas como en la firma de vídeo (parametrizable con `OCAPPS_PHOTOS_PUBLIC_PREFIX` solo si algún día hiciera falta; no exponer si no hay demanda).

Alternativa considerada y rechazada: mantener strip y montar photos en `/api/` con news moviendo `/api/me` → rompería la extensión de news (llama `/api/me/*` sin prefijo de app) y mezclaría dos contratos en un mismo prefijo.

### 4.4 CORS unificado

Los tres servicios usan hoy `Access-Control-Allow-Origin: *` con auth por cabecera (Bearer/Basic, **sin cookies**). Decisión: **se mantiene `ACAO: *`**, justificado:

1. Sin cookies ni `Access-Control-Allow-Credentials`, un token Bearer no es credencial ambiental: el riesgo clásico de `*` (robo de respuesta con credenciales de sesión) no aplica.
2. Las extensiones web llaman **same-origin** a través del proxy del host (CORS ni siquiera interviene); los clientes nativos (Iotas, news-android) ignoran CORS. Estrechar a un origen único solo añadiría una variable de config que rompería despliegues con el backend en otro host/puerto (caso documentado en ocphotos) sin ganancia de seguridad.

Unificación real: un solo middleware `httpx.CORS` con la **unión** de métodos y cabeceras de los tres:
- `Allow-Methods: GET, POST, PUT, PATCH, DELETE, OPTIONS`
- `Allow-Headers: Authorization, Content-Type, If-Match, OCS-APIRequest`
- `Expose-Headers: ETag, Last-Modified, X-Notes-API-Versions, X-Notes-Chunk-Cursor`
- Preflight `OPTIONS` → 204 sin pasar por auth (registrar `OPTIONS <prefix>/` antes del handler autenticado, como ya hace ocnews).

### 4.5 Healthz único y política de fallos (D3)

```json
GET /healthz  → 200 siempre que el proceso viva
{"version":"0.1.0","modules":{"news":"ok","notes":"ok","photos":"failed: sqlite: disk I/O error"}}
GET /readyz   → 200 si todos los módulos enabled = ok; 503 si alguno falla (mismo body)
```

**Política de fallos — también en el arranque inicial** (decisión D3, justificada):

- Un módulo cuyo wiring falla (DB no abre, migración rota, Graph inalcanzable) queda en estado `failed`: sus rutas devuelven `503 {"error":"module <name> unavailable"}` (handler sustituto registrado en su namespace), se loguea el error con causa, y el proceso **sigue**. Servicio parcial > apagón total; el operador lo ve en `readyz` (503) y en logs; systemd no entra en bucle de reinicios.
- **Excepción fatal**: errores de la base común (config común inválida, `DATA_DIR` no escribible, puerto ocupado) → el proceso no arranca (`main` devuelve error, `os.Exit(1)`), porque ahí no hay nada útil que servir.
- Photos además gana una mejora: si `ListDrives`/Graph falla al arrancar, el módulo queda `failed` y **reintenta en background** (backoff 30 s→5 min) en vez del `os.Exit(1)` actual. **(H8: ya no aplica — photos es multi-tenant y NO hace init contra OpenCloud al arrancar; el módulo nace sano, `Healthy()` = ping SQLite, y la resolución de espacios personales es lazy por usuario.)**
- Loops en background (scheduler de news, scanner de photos) corren con `recover()` por goroutine: un pánico marca el módulo `failed` sin tumbar el proceso. No hay auto-restart de módulos en v0.1 (queda como trabajo futuro; systemd solo reinicia si muere el proceso entero).

Interfaz de módulo (en `internal/common/httpx` o un mini-paquete `common/module`):

```go
type Module interface {
    Name() string
    Enabled() bool
    // Register monta las rutas del módulo en el mux (o el handler 503 si failed).
    Register(mux *http.ServeMux)
    // Run ejecuta loops de background; debe retornar al cancelar ctx.
    Run(ctx context.Context) error
    // Healthy informa a /healthz y /readyz.
    Healthy() error
}
```

---

## 5. SQLite

### 5.1 Decisión: tres ficheros `.db` (D2)

```
$OCAPPS_DATA_DIR/
├── news/     ocnews.db  (+favicons/, imgcache/, imgsecret, feedsecret)
├── notes/    notes.db   (+attachments/, imgcache/, imgsecret)
└── photos/   memories.db(+thumbs/, hls/, mediasecret)
```

**Justificación frente a una BD única con prefijos de tabla:**

1. **`PRAGMA user_version` es por fichero.** Con tres ficheros, los esquemas de migración existentes de news (user_version=18) y notes (user_version=2) funcionan **sin tocar una línea**; con una BD única habría que namespacing-ear versiones (tabla `schema_migrations(module, version)`) y re-baselinar news igualmente.
2. **Aislamiento de fallos** (coherente con D3): corrupción/lock de una BD no afecta a las otras; `SetMaxOpenConns(1)` es por conexión y el patrón single-writer se mantiene por fichero.
3. **Backup/rollback por módulo** (copiar un fichero) y migración de datos trivial: copiar los `.db` existentes a su nuevo subdir (§5.4), sin transformación alguna.
4. Nombres de tabla genéricos de photos (`assets`) y de notes (`settings`) chocarían conceptualmente en una BD compartida.

El directorio de datos por módulo también recoge sus ficheros auxiliares (secretos, cachés, adjuntos), preservando los nombres actuales para que la copia sea 1:1.

### 5.2 Apertura unificada (`common/store`)

Un solo `Open` con la unión de pragmas de los tres (hoy difieren: news usa `busy_timeout(5000)` + WAL + `foreign_keys(1)`; photos `busy_timeout(10000)` sin WAL explícito en DSN — lo activa en el schema — y **sin foreign_keys en DSN**; notes similar a news):

```go
func Open(path string) (*sql.DB, error) {
    // mkdir 0700 del dir padre
    dsn := "file:" + url.PathEscape(path) +
        "?_pragma=busy_timeout(10000)" +
        "&_pragma=journal_mode(WAL)" +
        "&_pragma=synchronous(NORMAL)" +
        "&_pragma=foreign_keys(1)"
    // SetMaxOpenConns(1)
}
```

⚠️ Verificar en tests que activar `foreign_keys(1)` en la BD de photos no rompe nada (sus tablas tienen `REFERENCES ... ON DELETE CASCADE`; activarlo es semánticamente más correcto, pero hay que correr su suite).

### 5.3 Runner de migraciones unificado y adopción de BDs existentes

**Runner común** (port del de ocnews, que es el patrón más limpio):

```go
// Migrate aplica migrations/*.sql (embed) con número > PRAGMA user_version,
// cada una en su transacción, fijando user_version al final.
func Migrate(db *sql.DB, fsys fs.FS, dir string) error
// Baseline fija user_version sin ejecutar SQL (adopción de esquemas vivos).
func Baseline(db *sql.DB, version int) error
```

| Módulo | Estado actual | Adopción en ocapps |
|---|---|---|
| **news** | 18 migraciones, `user_version=18` | **Cero cambios.** Copiar `migrations/001..018` a `internal/news/store/migrations/` y usar `Migrate`. La BD viva llega con `user_version=18` y no se aplica nada. |
| **notes** | 2 migraciones + backup previo + backfill de owner | Portar `migrate.go` sobre el runner común **conservando** su backup pre-migración (`notes.db.<ts>.bak`) y el backfill con `OCAPPS_NOTES_OWNER`. BD viva: `user_version=2`, no-op. |
| **photos** | schema `CREATE TABLE IF NOT EXISTS` + 2 `ALTER` idempotentes tolerantes a "duplicate column", **sin versiones** | **Baselinar**: `001_baseline.sql` = el schema completo actual (CREATE IF NOT EXISTS + índices + las dos columnas `is_archived`/`phash` ya incluidas en el CREATE de `assets`). La función de adopción: (1) abre la BD viva; (2) verifica que existen las tablas esperadas (`assets`, `scan_state`, `albums`, `album_assets`, `asset_tags`, `geocode`) y las columnas `is_archived`, `phash` (`PRAGMA table_info(assets)`); (3) si todo está → `Baseline(db, 1)`; (4) si la BD está vacía/nueva → `Migrate` aplica `001_baseline.sql` y fija `user_version=1`. Futuras migraciones empiezan en `002_*.sql`. El patrón frágil de ALTERs en caliente desaparece. **(H8)** `002_multiowner.sql` lleva el esquema a multi-owner (rebuild de `assets` con `owner` + `UNIQUE(owner,path)`, `ALTER albums ADD owner`, índices con prefijo owner; `geocode` sigue global a propósito — caché de nombres públicos, no personal). El baseline 001 se actualizó para que las BDs nuevas **nazcan** multi-owner y el estado final converja a `user_version=2` (una BD nueva aplica 001+002; la 002 sobre tablas vacías es no-op efectivo; la BD viva adoptada en 1 recibe el owner vía 002). El runner ejecuta las migraciones con `foreign_keys` OFF (sin ello, el `DROP TABLE` del rebuild dispararía los `ON DELETE CASCADE` de `album_assets`/`asset_tags`). El **backfill** de las filas de la era single-tenant (`owner=''`) no puede ser SQL: el módulo resuelve el oc_id con `OCAPPS_PHOTOS_USER`+`APP_TOKEN` (`MeID`) y hace `UPDATE ... SET owner=?` al arrancar; sin esas credenciales loguea un WARN con el recuento y arranca igual (ver §6.2 y `deploy/README.md`). |

**Regla nueva para los tres**: toda migración futura es un fichero `NNN_descripcion.sql` inmutable en el `migrations/` de su módulo; prohibido ALTERs tolerantes a error fuera del runner.

### 5.4 Procedimiento de migración de datos (producción: 3 BDs vivas + systemd x3 → x1)

Principios: **copia, nunca mover en caliente**; verificación con `PRAGMA integrity_check`; rollback sin pérdida. Script `deploy/migrate.sh` (idempotente, con `--dry-run`):

```bash
# 0. Pre-vuelo
systemctl stop ocnews ocnotes ocphotos        # orden indiferente; esperar a inactive
# 1. Verificar origen (antes de copiar)
for db in /var/lib/ocnews/ocnews.db /var/lib/ocnotes/notes.db /var/lib/ocphotos/memories.db; do
  sqlite3 "$db" "PRAGMA integrity_check;" | grep -qx ok || abort
done
# 2. Copiar al nuevo layout (cp preserva; nunca mv)
install -d -m 700 -o ocapps /var/lib/ocapps/{news,notes,photos}
cp -a /var/lib/ocnews/ocnews.db*   /var/lib/ocapps/news/      # incluye -wal/-shm si existen
cp -a /var/lib/ocnotes/notes.db*   /var/lib/ocapps/notes/
cp -a /var/lib/ocphotos/memories.db* /var/lib/ocapps/photos/
# 3. Copiar datos auxiliares por módulo
cp -a /var/lib/ocnews/{favicons,imgcache,imgsecret,feedsecret}  /var/lib/ocapps/news/
cp -a /var/lib/ocnotes/{attachments,imgcache,imgsecret}         /var/lib/ocapps/notes/
cp -a /var/lib/ocphotos/{thumbs,hls,mediasecret}                /var/lib/ocapps/photos/
# 4. Verificar destino
for db in .../ocapps/*/*.db; do sqlite3 "$db" "PRAGMA integrity_check;" | grep -qx ok || abort; done
# 5. Registrar versiones esperadas (log para auditoría)
sqlite3 /var/lib/ocapps/news/ocnews.db "PRAGMA user_version;"   # esperado: 18
sqlite3 /var/lib/ocapps/notes/notes.db "PRAGMA user_version;"   # esperado: 2
# 6. systemd: disable viejos (NO borrar), enable+start unificado
systemctl disable ocnews ocnotes ocphotos
systemctl enable --now opencloud-apps
# 7. Smoke checks (§9.4)
```

Notas:
- Con los servicios parados no debe haber `-wal`/`-shm`; si existen, copiarlos junto al `.db` (o hacer `sqlite3 $db "PRAGMA wal_checkpoint(TRUNCATE);"` antes de parar/copiar).
- El primer arranque de ocapps aplica: news/notes no-op, photos baseline (§5.3). La migración 002 de notes (backfill owner) ya está aplicada en la BD viva; `OCAPPS_NOTES_OWNER` sigue siendo necesario solo si quedaran filas sin owner.
- **Rollback** (válido mientras no se hayan escrito datos nuevos que importen; documentar la ventana): `systemctl stop opencloud-apps` → `systemctl enable --now ocnews ocnotes ocphotos` (sus datos en `/var/lib/ocnews|ocnotes|ocphotos` están intactos — solo lectura/copia) → revertir cambio de proxy si se aplicó el de §4.3. Si ya se escribieron datos en las BDs nuevas y hay que volcarlos atrás: parar todo, copiar los `.db` de vuelta (misma verificación), arrancar los viejos.
- Tras N días de estabilidad (recomendado ≥7): archivar `/var/lib/ocnews*` y eliminar los units viejos.

---

## 6. Auth unificada

### 6.1 Un solo validador Graph en `common/auth`

Se toma como base el `OpenCloudValidator` de ocnews (el más completo: Basic+Bearer, shadow users, TTL) y se absorben las variantes de ocnotes (caché en memoria con evicción) y ocphotos (validación sin caché + chequeo single-tenant):

```go
type GraphValidator struct {
    graphURL string                 // <OCAPPS_OPENCLOUD_URL>/graph/v1.0/me
    client   *http.Client           // timeout 10 s
    cache    sync.Map               // key → cacheEntry{user *User, expiry, negative bool}
    sf       singleflight.Group     // una sola llamada a Graph por clave en vuelo
    policy   Policy
}
```

- **Claves de caché**: `basic:<username>:<sha256(password)>` y `bearer:<sha256(token)>` (ocnotes hoy trunca `token[:32]` en claro como clave — mejor hash completo; evita colisiones y no guarda el token). La clave Basic incluye la contraseña **solo como hash** (nunca en claro), como hacía el ocnews original (`sha256("basic\x00user\x00pass")`): sin ella, tras un login legítimo cualquier contraseña de ese usuario entraría por caché durante el TTL, y una entrada negativa cacheada bajo la misma clave permitiría un DoS del usuario legítimo con un solo intento fallido.
- **TTL**: positiva 5 min (igual que hoy en news/notes). **Negativa 30 s (nuevo)**: un 401/403 de Graph se cachea 30 s para que un cliente en bucle no martillee el IdP. **Solo se cachean rechazos explícitos (401/403)**: los fallos de red, 5xx o respuestas malformadas NO se cachean — un IdP caído no debe envenenar la caché y convertir un glitch transitorio en un outage de 30 s. Documentar el cambio: revocación de credenciales tarda ≤5 min en propagarse (ya era así en news/notes; en photos pasa de 0 a 5 min — ventana aceptable, alineada con el resto).
- **Singleflight**: en cache-miss concurrente con la misma clave, una sola petición a Graph; las demás esperan su resultado. Elimina la estampida al expirar entradas calientes (la extensión de photos hace ráfagas de peticiones de miniaturas).

### 6.2 Multiusuario en los tres módulos (photos multi-tenant desde H8)

Un mismo validador, **política inyectada**:

```go
type Policy interface { Admit(u *User) bool }
type multiTenant struct{}                    // los tres módulos (photos desde H8)
type singleTenant struct{ ocID string }      // ya sin uso en photos; queda en common/auth
```

- **News**: mantiene su tabla `users` (shadow users con `oc_id`, rol admin al primer usuario) — es su modelo de dominio; el validador común devuelve el `User` Graph y `internal/news` resuelve/crea su fila local como hace hoy `OpenCloudValidator` (esa parte queda en `internal/news/auth.go`, fina).
- **Notes**: mantiene su shadow user en memoria + escopado por graph ID en la columna `user` (su "persistencia" es la propia tabla `notes`).
- **Photos (H8)**: `MultiTenant()`. El `*auth.User` resuelto por `Validate` se inyecta en el request context (`api.OwnerFrom`) y TODOS los handlers scopean al owner (oc_id): el store filtra por `owner` en cada consulta (regla IDOR: un id ajeno → `sql.ErrNoRows` → 404, nunca 403). También entra Basic app-password. La extensión web ya manda el Bearer OIDC en cada llamada.
  - **Sesiones en memoria (registry)**: cada request autenticado hace `Touch(owner, cred)` que crea/actualiza `map[owner]*session{dav, webdavURL, isBasic, lastSeen}` con un cliente `webdav.NewBearer` (o Basic). **Nada se persiste a disco** (privacidad: los tokens OIDC solo viven en RAM). Las sesiones Bearer sin actividad >24h se purgan; las Basic y las sembradas por `OCAPPS_PHOTOS_USERS` no. El `webDavUrl` del espacio personal se resuelve lazy (primer request/scan del usuario) vía `ListDrives` con SU credencial.
  - **Indexado por actividad (scheduler)**: el primer uso de un usuario dispara su scan (índice progresivo); un ticker `SCAN_EVERY` escanea a los usuarios sembrados y a las sesiones Basic activas; cola con dedupe por owner + semáforo de 2 scans concurrentes + timeout 2h por scan. Ante 401/403 (`webdav.ErrUnauthorized`) el scan ABORTA sin soft-delete (el índice queda intacto) y se pospone a la próxima actividad del usuario.
  - **Bearer estático `OCAPPS_PHOTOS_TOKEN` (DEPRECATED)**: comprobado ANTES de Graph como siempre, pero solo válido si `OCAPPS_PHOTOS_USER` está configurado — mapea al oc_id legacy resuelto con el app-token (WARN de deprecación en arranque). Sin `USER` es error de config (el token ya no tiene identidad).
  - **Vídeo firmado (`/api/video/{id}`)**: capability URL sin sesión, como antes; la firma HMAC solo se genera tras verificar ownership en `video-url`, y el stream resuelve el owner desde el id (los ids son globales) para servir con la sesión DAV de ese owner.
  - **Thumbs**: caché en disco compartida sin colisiones (la clave incluye el href, que contiene el space-UUID del usuario).
- Resultado: una sola caché Graph compartida por los tres módulos (un usuario de la web que abre news y photos valida una vez contra Graph), con los tres módulos multi-tenant.

### 6.3 Middleware

`auth.Middleware(validator, next)` con la semántica actual de ocnews (acepta `Authorization: Basic` y `Bearer`, 401 con `WWW-Authenticate`). Photos deja de tener su `validOpenCloudSession` propio: su `withAuth` pasa a usar el middleware común + exemptions (`/ocphotos-api/api/video/` firmado) + chequeo de token propio.

---

## 7. Quick-wins a incorporar durante la fusión

| # | Tarea | Fichero origen | Detalle |
|---|---|---|---|
| Q1 | Typo `OCNEWS_OPENCOLOUD_URL` | ocnews `config.go:29,50` | Desaparece en favor de `OCAPPS_OPENCLOUD_URL`; la legacy (con typo) se sigue leyendo con warning (§3.2). |
| Q2 | Data race `geo.Geocoder.last` | ocphotos `geo.go:22,46-53` | Añadir `mu sync.Mutex` alrededor del throttle (leer/actualizar `last` bajo lock). Detectable con `go test -race` + test nuevo con dos `Name()` concurrentes. |
| Q3 | Fallback hardcodeado del media secret | ocphotos `api.go:318-329` | `mediaSecret()` pasa a `common/cred.LoadOrCreateSecret(<photos>/mediasecret)` que **devuelve error si `rand.Read` falla** → el módulo photos arranca `failed` (503) en vez de firmar URLs predecibles con `"ocphotos-fallback-secret"`. |
| Q4 | ffmpeg | `app/Dockerfile`, deploy | (a) Dockerfile nuevo de ocapps incluye `ffmpeg` en la etapa runtime; (b) al arrancar photos: `exec.LookPath("ffmpeg")` → si falta, `log.Warn` "ffmpeg no encontrado: pósters de vídeo y HLS degradados" (no fatal: el resto sirve; hoy falla silencioso con redirect/502); (c) documentar en `deploy/README.md` como **única dependencia de sistema**. |
| Q5 | `rescanCh` con buffer 1 | ocphotos `api.go:750-756`, `main.go` | Subir buffer a 4 y loguear drops (`"rescan ignorado: cola llena"`); el throttle de 20 s se mantiene en el consumidor. |
| Q6 | Retirada PWA legacy | ocphotos `app/src`, `app/info.md`, `app/Dockerfile`, `docker-compose.yml`, `main.go:177-182,200-210` (`spaHandler`, `WEB_DIR`) | Eliminar del repo ocphotos; en ocapps no se porta nada de eso. `WEB_DIR` definida → warn "ignorada". |
| Q7 | Endpoints muertos de ocphotos | `api.go:62-93` | Candidatos: `GET /api/thumb` (por path), `GET /api/assets/{id}/hls/{file}`, `GET /api/assets/{id}`. **Antes de borrar**: re-grep sobre `ocphotos/ocphotos/src` (extensión) y `app/src` (que desaparece en Q6) en el commit actual — la auditoría los marca sin consumidor, pero hay que revalidar en el momento del port. Si se confirma: se borran handler + rutas. Ojo: `/api/video/{id}` (stream firmado) **sí se usa** y se queda. Si se borra HLS, `internal/video` queda solo para... nada → evaluar borrar `video.go` y la caché `hls/` (documentar decisión; ffmpeg seguiría usándose para pósters en `thumb.VideoPoster`). |
| Q8 | CORS | los 3 | Unificado en `httpx.CORS` (§4.4). |
| Q9 | `slog.SetDefault` único | ocnews `main.go:50` | Solo en `cmd/ocapps/main.go`; los paquetes reciben logger por constructor (patrón ya dominante). |
| Q10 | Dockerfile/toolchain Go | ocphotos `app/Dockerfile` (`golang:1.24-alpine` vs `go 1.26.4`) | Fijar `golang:1.26-alpine` en el Dockerfile nuevo de ocapps. |

---

## 8. Plan de implementación por hitos

Dependencias: **H0 → H1 → {H2, H3, H4} → H5 → H6**. H2/H3/H4 son paralelizables entre subagentes (directorios disjuntos; el único punto de contención es `go.mod`, que H0 ya deja completo). Orden recomendado si hay menos paralelismo: H2 (notes, el más pequeño — valida `common` pronto) → H4 (news, el más largo) → H3 (photos, con quick-wins).

| Hito | Contenido | Criterios de aceptación | Riesgos |
|---|---|---|---|
| **H0 — Esqueleto** | Repo `ocapps`, `go.mod` unificado (deps §2.3, `third_party/goexif` + replace), `cmd/ocapps/main.go` mínimo (solo healthz), CI (vet, `test -race -shuffle=on`, mod tidy check, golangci-lint, govulncheck — heredar workflow de ocnews ampliado a todo el módulo), `.goreleaser.yaml` (`CGO_ENABLED=0`, ldflags version/commit/date únicos), Dockerfile (`golang:1.26-alpine` + `ffmpeg` runtime). | `CGO_ENABLED=0 go build ./...` OK; CI verde; binario arranca y responde `/healthz`. | Bajo. |
| **H1 — common** | Todos los paquetes de §2.1 con sus tests nuevos (§10.2): config (tabla legacy completa), auth (validator+middleware+policies), store (Open+Migrate+Baseline), httpx, log, cred, netguard, imgproxy (merge), webdav (port generalizado de `internal/dav`). | `go test -race ./internal/common/...` verde; cobertura de caminos legacy de config. | Merge de los dos imgproxy/netguard: verificar que firmas HMAC generadas con el secret viejo siguen validando (el algoritmo no cambia, solo el paquete) — test con vector fijo. |
| **H2 — Port notes** | Copiar `internal/{api,store,attachments}` → `internal/notes/...`; reescribir imports; adoptar `common/{config,auth,store,httpx,imgproxy,cred}`; `module.go` con `Run/Register/Healthy`. Tests viajan. | `go test -race ./internal/notes/...` verde (los 6 ficheros de test existentes, 963 l.); smoke manual: CRUD de nota + capabilities OCS contra instancia real. | `store.Open(dataDir, owner)` cambia de firma (recibe `*sql.DB` común); conservar backup pre-migración y backfill owner. |
| **H3 — Port photos** | Copiar `internal/{api,store,index,exif,thumb,video,geo,phash}` → `internal/photos/...`; loop de scan (hoy closures de main.go) → `module.go`; rutas bajo `/ocphotos-api/`; auth → middleware común + SingleTenant + token propio; quick-wins Q2,Q3,Q4,Q5,Q7 (re-grep previo); baseline de migraciones (§5.3). | Suite photos verde con `-race` (incl. test nuevo del race de geo); test de baseline sobre fixture de `memories.db` real; tests de ffmpeg siguen auto-skipeando sin ffmpeg. | Subida modernc/sqlite 1.38→1.56; activar `foreign_keys(1)`; Q7 (borrado de endpoints) requiere verificación fresca contra la extensión. |
| **H4 — Port news** | Copiar los 14 paquetes internos → `internal/news/...`; shadow-user glue en `internal/news/auth.go`; scheduler/websub/notify intactos; verificación del stub OCS (§4.2) y retirada; rutas `/api/me|users` con patrones exactos. | `go test -race ./internal/news/...` verde (31 ficheros de test); `check:types`/build de la extensión news sin cambios; diff de payloads OCS documentado. | Es el módulo mayor (7.657 l.); riesgo de deriva si H1 cambió APIs — mitigar fijando las firmas de common en H1. |
| **H5 — Wiring unificado** | `cmd/ocapps/main.go` completo: config común → init por módulo con degradación (§4.5) → mux único → supervisor de `Run(ctx)` con recover → shutdown gracioso (10 s) → healthz/readyz agregados. | Tests de integración de arranque (módulo forzado a fallar → 503 en su namespace, 200 en los demás, readyz 503); `go test -race ./...` global verde. | Comportamiento en degradación es código nuevo sin precedente en los repos — cubrir con tests. |
| **H6 — Despliegue y migración** | `deploy/`: unit systemd, `env.example`, snippets de proxy, `migrate.sh` (§5.4) con `--dry-run`, README de despliegue + rollback; avisos de deprecación en los README de los 3 repos de app. | Ensayo en instancia de staging con copias de las 3 BDs reales: checklist §9.4 completo; rollback ensayado. | El cambio de proxy (§4.3) es el único paso no revertible por el propio binario — coordinar con el corte de systemd. |

**Gates transversales por hito**: build `CGO_ENABLED=0`, `go vet ./...`, `go test -race -shuffle=on ./...`, y para los que tocan contrato (H2-H5): build + `check:types` de la extensión correspondiente en su repo (sin modificarla) como garantía de contrato intacto.

---

## 9. systemd y despliegue

### 9.1 Unit único `/etc/systemd/system/opencloud-apps.service`

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

Nota: `Restart=on-failure` solo actúa si muere el proceso entero; un módulo caído no reinicia nada (por diseño, §4.5) — la alerta es `readyz` 503.

### 9.2 `/etc/ocapps/env` (ejemplo; 640 root:ocapps)

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
OCAPPS_PHOTOS_TOKEN=<token-propio-opcional>
OCAPPS_PHOTOS_SCAN_ROOT=Fotos
OCAPPS_PHOTOS_SCAN_EVERY=30m
```

### 9.3 Cambios en el proxy

Hoy (inferido de auditorías y deploy README): `/index.php/apps/news/*` y `/api/*` → ocnews :8094; `/index.php/apps/notes/*` y `/ocs/*` → ocnotes :8100; `/ocphotos-api/` → ocphotos :8097 **con strip**. Objetivo: **todo al mismo upstream sin strip**:

```nginx
# /index.php/apps/news/ y /index.php/apps/notes/ y /ocs/ igual que hoy (sin strip),
# solo cambia el upstream:
location /index.php/apps/news/  { proxy_pass http://127.0.0.1:8096; }
location /index.php/apps/notes/ { proxy_pass http://127.0.0.1:8096; }
location /ocs/                  { proxy_pass http://127.0.0.1:8096; }
location /api/me                { proxy_pass http://127.0.0.1:8096; }   # contrato extensión news
# photos: QUITAR el trailing slash que strip-peaba el prefijo:
location /ocphotos-api/         { proxy_pass http://127.0.0.1:8096; }   # antes: http://...:8097/
```

(Replicar cabeceras `Authorization`, `Host`, timeouts largos para `/ocphotos-api/api/video/` por Range/streaming — igual que hoy.) **Decisión**: rutado directo por prefijos públicos; no se introducen prefijos nuevos `/ocnews-api/` ni `/ocnotes-api/` (romperían el contrato de los clientes Nextcloud News/Iotas).

### 9.4 Checklist de verificación en instancia

```bash
systemctl is-active opencloud-apps                       # active
systemctl is-enabled ocnews ocnotes ocphotos             # disabled (x3)
journalctl -u opencloud-apps -n 50 --no-pager            # sin "env legacy" inesperados ni módulos failed
curl -s localhost:8096/healthz | jq .                    # los 3 módulos "ok"
curl -s -o /dev/null -w '%{http_code}\n' localhost:8096/readyz   # 200
# Contratos (interno, por localhost, con credenciales válidas):
curl -su user:app-token localhost:8096/index.php/apps/news/api/v1-3/version
curl -su user:app-token localhost:8096/index.php/apps/notes/api/v1/notes
curl -s  -H "Authorization: Bearer $TOK" localhost:8096/ocs/v2.php/cloud/capabilities
curl -s  -H "Authorization: Bearer $TOK" localhost:8096/ocphotos-api/api/stats
# Integridad de BDs:
for f in /var/lib/ocapps/*/*.db; do sqlite3 "$f" 'PRAGMA integrity_check;'; done   # ok x3
sqlite3 /var/lib/ocapps/news/ocnews.db 'PRAGMA user_version;'    # 18
sqlite3 /var/lib/ocapps/notes/notes.db 'PRAGMA user_version;'    # 2
sqlite3 /var/lib/ocapps/photos/memories.db 'PRAGMA user_version;' # 1 (baseline aplicado)
# Desde el exterior (vía proxy, mismo origen que la web):
curl -s -H "Authorization: Bearer $TOK" https://<host>/ocphotos-api/api/stats
curl -su user:app-token https://<host>/index.php/apps/news/api/v1-3/feeds
```

Verificación funcional final: abrir las tres apps en la web de OpenCloud, y sincronizar un cliente externo (news-android o Iotas).

---

## 10. Tests

### 10.1 Port de la suite existente

- Los `_test.go` viajan **con sus paquetes** (news: 31 ficheros; notes: 6; photos: 8) y solo cambian los imports. No se reescriben aserciones: son la red de seguridad del port.
- Gate global: `go test -race -shuffle=on ./...` en CI (heredado de ocnews). `-race` es ahora especialmente valioso: tres schedulers/loops en un proceso.
- Tests que usan SQLite real (store de notes/photos con tmp/memoria) funcionan igual vía `common/store.Open`.
- Tests de photos que se saltan sin ffmpeg (`video_test.go`, `thumb/video_test.go`) conservan ese comportamiento; en CI del Dockerfile se puede añadir un job con ffmpeg que los ejecute de verdad.
- Extensión: sin cambios, pero su `build` + `check:types` (donde exista; añadir script en ocnotes — quick-win de repo de app) se usa como gate de contrato en H2-H5.

### 10.2 Tests nuevos mínimos para `common`

| Paquete | Test |
|---|---|
| `config` | Tabla-driven: para cada fila de §3.2, valor nuevo > legacy > default; warning de deprecación registrado al usar legacy; mapeo `OCNOTES_GRAPH_URL` → raíz (strip `/graph/v1.0/me`); rechazo de durations inválidas. |
| `auth` | `httptest.Server` simulando Graph `/me`: (1) cache hit no re-emite petición dentro del TTL; (2) singleflight: N validaciones concurrentes con el mismo token → 1 sola petición a Graph; (3) 401 cacheado 30 s; (4) políticas MultiTenant vs SingleTenant (id distinto → rechazo); (5) Basic y Bearer resuelven al mismo usuario por `oc_id`. |
| `store` | `Migrate` aplica en orden y fija `user_version`; re-apertura no-op; `Baseline` sobre una copia del fixture `memories.db` real verifica tablas/columnas y fija 1; migración defectuosa hace rollback y deja `user_version` intacto. |
| `httpx` | CORS: preflight 204 sin auth; cabeceras unión exactas; `WriteJSON` fija Content-Type y código. |
| `cred` | `LoadOrCreateSecret` crea 0600, reutiliza, **falla** si el fichero tiene longitud inválida; vector AES-GCM fijo. |
| `imgproxy` | Vector HMAC fijo: una URL firmada con el algoritmo/secret de ocnews viejo valida con el proxy unificado (garantía de compat de firmas). |

### 10.3 Tests de integración nuevos (H5)

- Arranque con un módulo forzado a fallar (p. ej. `OCAPPS_DATA_DIR` apuntando a un subdir de photos de solo lectura): sus rutas → 503, las de los demás → 200, `/readyz` → 503, proceso vivo.
- Registro de rutas: tabla de todos los paths públicos del §4.1 contra el mux real (garantiza que ningún namespace pisa a otro — incluida la unicidad de `/ocs/v2.php/cloud/user`).

---

## Anexo A. Ficheros fuente de referencia (para los coders)

| Tema | Fichero |
|---|---|
| Wiring news (referencia de orden de arranque) | `ocnews/backend/cmd/ocnews/main.go` |
| Wiring photos (closures de scan → module.go) | `ocphotos/app/server-go/cmd/photos-service/main.go` |
| Wiring notes | `ocnotes/backend/main.go` |
| Auth base a portar | `ocnews/backend/internal/auth/{validator,auth}.go` |
| Runner de migraciones base | `ocnews/backend/internal/store/store.go` (`Open`+`migrate`) |
| Backup+backfill notes | `ocnotes/backend/internal/store/migrate.go` |
| Baseline photos (ALTERs actuales) | `ocphotos/app/server-go/internal/store/sqlite.go:113-131` |
| CORS/rutas news | `ocnews/backend/internal/api/server.go` |
| CORS/sesión/secret photos | `ocphotos/app/server-go/internal/api/api.go` |
| Cliente WebDAV a generalizar | `ocphotos/app/server-go/internal/dav/client.go` |
| Env legacy completas | `ocnews/backend/internal/config/config.go`, `ocnotes/backend/internal/config/config.go`, `ocphotos .../main.go:46-59` |

## Anexo B. Supuestos a confirmar antes de H6

1. **Configuración real del proxy de producción**: este diseño asume (de `deploy/README.md` de ocphotos y del hardcodeo de rutas en news) que hoy el proxy no strip-pea `/index.php/...` ni `/ocs/` y sí strip-pea `/ocphotos-api/`. Confirmar leyendo la config real antes de escribir los snippets definitivos.
2. **Puertos/data dirs reales en producción** (el deploy README de ocphotos menciona `:8097` y `/var/lib/ocphotos`; news/notes se asumen `/var/lib/ocnews`, `/var/lib/ocnotes`) — ajustar `migrate.sh` con los paths reales del usuario.
3. **Payloads OCS** de los dos handlers `/ocs/v2.php/cloud/user` (§4.2): diff obligatorio en H4.
4. **Endpoints muertos de photos** (Q7): re-grep contra la extensión en el commit del port antes de borrar.
5. Modernc.org/sqlite 1.56 + `foreign_keys(1)` sobre la BD viva de photos: validado por su suite, pero el primer arranque en staging debe ir seguido del checklist §9.4.
