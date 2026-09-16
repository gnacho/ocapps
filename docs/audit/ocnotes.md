# Informe de auditoría: ocnotes (OpenCloud Notes)

Repo auditado: `/mnt/agents/work/opencloud/ocnotes` (HEAD `2c0ff6b`, v0.1.4). Solo lectura; no se ha modificado nada.

---

## 1. Métricas reales vs. plan

| Métrica | Plan | Real | Veredicto |
|---|---|---|---|
| Líneas Go | 2.905 | **2.905** | ✔ exacto |
| Ficheros Go | 18 | **18** | ✔ exacto |
| Líneas TS+Vue | 1.790 | **1.785** en `extension/src` (12 ficheros: 6 `.ts` + 6 `.vue`) | discrepancia de 5 líneas = `extension/vite.config.ts` (5 líneas). El plan contaba TS también fuera de `src/`. Con él: 1.790 exactos. |

Matices que el plan no desglosa:
- De las 2.905 líneas Go, **963 son tests** (6 ficheros `_test.go`): `server_test.go` 249, `attachments_test.go` 240, `migrate_test.go` 198, `store_test.go` 139, `imgproxy_test.go` 83, `netguard_test.go` 54. Código Go de producción: **1.942 líneas en 12 ficheros**.
- El TS/Vue excluye `styles.css` (756 líneas) y `l10n/translations.json` (148).
- **No existe `deploy/`** en el repo (el contexto del plan lo menciona; está ausente). Tampoco hay `manifest.json`, `Dockerfile`, `Makefile` ni CI.

## 2. Inventario del backend

### Endpoints exactos (`backend/internal/api/server.go`, router en líneas 87–128)

Base `v1` = `/index.php/apps/notes/api/v1`, `v1.4` = `/index.php/apps/notes/api/v1.4` (constantes en líneas 25–26):

| Método | Path | Handler | Uso |
|---|---|---|---|
| GET | `/v1/notes` | `handleGetNotes` (l.340) | Lista con `category`, `exclude`, `pruneBefore`, `chunkSize` (protocolo de sync incremental de la API Notes) |
| POST | `/v1/notes` | `handleCreateNote` (l.451) | Crear |
| GET | `/v1/notes/{id}` | `handleGetNote` (l.477) | Leer (devuelve ETag) |
| PUT | `/v1/notes/{id}` | `handleUpdateNote` (l.494) | Actualizar título/contenido/categoría/favorito, con `If-Match` → 412 (l.506–512) |
| DELETE | `/v1/notes/{id}` | `handleDeleteNote` (l.541) | Borrar + cascada de adjuntos (l.553–557) |
| GET/PUT | `/v1/settings` | `handleGetSettings`/`handlePutSettings` (l.563, 575) | Settings por usuario |
| POST | `/v1/img/sign` | `handleImgSign` (l.132) | Firma HMAC de URLs de imagen externas |
| GET | `/v1/img?u=&t=` | `imgproxy.Serve` (pública, sin auth, firmada) | Proxy de imágenes con caché en disco |
| POST/GET | `/v1.4/attachment/{id}` | `handleAttachment` (l.165) | Subir (multipart, ≤32 MiB) / descargar adjuntos |
| GET | `/ocs/v2.php/cloud/capabilities` | `handleCapabilities` (l.623) | OCS XML/JSON, anuncia `api_version ["0.2","1.4"]`, version 6.0.0 |
| GET | `/ocs/v2.php/cloud/user` | `handleUserInfo` (l.667) | Info de usuario OCS |

Todas las respuestas llevan `X-Notes-API-Versions: 0.2, 1.4` (l.27–28).

### Esquema SQLite (`backend/internal/store/migrations/`)

- **Tabla `notes`** (001_initial.sql l.4–12 + 002_multiuser.sql l.4): `id INTEGER PK AUTOINCREMENT, title, content, category, favorite INTEGER, modified INTEGER, etag TEXT, user TEXT`. Índices por category, favorite, modified, user, (user,category), (user,modified).
- **Tabla `settings`** (reconstruida en 002 l.10–22): `(user, key, value)` con PK compuesta. Defaults sembrados en código: `notesPath="Notes"`, `fileSuffix=".md"` (`store/notes.go` l.237).
- Los adjuntos **no** están en SQLite: van a disco en `<dataDir>/attachments/<noteID>/<nombre>` (`attachments/attachments.go`).

### Lógica que va MÁS ALLÁ de un CRUD trivial sobre `.md` (riesgos para F1)

1. **Servidor de la API Nextcloud Notes para clientes externos, no solo para la extensión.** Los comentarios citan a *Iotas* (cliente Android de Notes) en `server.go` l.52–55 y l.594, y los endpoints OCS (capabilities/user) solo existen para clientes externos. Los commits `e7704cf fix(api): close the Notes API gaps that break Iotas` y `9f1de3c feat(api): implement the Notes API v1.4 attachment endpoints` lo confirman. **Eliminar el backend rompe la sincronización con clientes móviles/escritorio**, no solo la web.
2. **Autenticación multiusuario contra Graph** (`auth/auth.go`): valida Basic y Bearer contra `OCNOTES_GRAPH_URL`, cachea "shadow users" 5 min, y **escopa todas las notas por graph ID** (migración 002_multiuser). WebDAV del host daría esto gratis para la web, pero es lo que permite a Iotas autenticarse.
3. **Favoritos persistidos** (columna `favorite`, orden `favorite DESC, modified DESC` en `store/notes.go` l.98). No es derivable del contenido del `.md`.
4. **Categorías como metadato por nota** (columna `category`, filtrable por query param). No son carpetas reales.
5. **Etags y control de conflictos**: `If-Match` → 412 con la nota actual en el cuerpo (`server.go` l.506–512); ETag agregado SHA-256 en la lista (l.370); cabecera `Last-Modified`.
6. **Protocolo de sync incremental**: `pruneBefore` (notas podadas a solo `{id}`), `chunkSize` + `X-Notes-Chunk-Cursor`, `exclude=content` (l.340–449). Pensado para clientes con caché local.
7. **Proxy de imágenes firmado HMAC-SHA256** (`imgproxy/imgproxy.go`): existe porque la CSP del host es `img-src 'self'` y el preview no puede cargar imágenes externas. Incluye mitigación SSRF (`netguard`: rechaza IPs privadas/loopback/link-local), caché en disco, límite 8 MB, secret persistente. **Sin sustituto WebDAV.**
8. **Adjuntos con dedup de nombres** estilo Nextcloud 6.1+ (`nombre (1).ext`), escritura atómica, límite 32 MiB, rutas `.attachments.<id>/`, borrado en cascada.
9. **Settings por usuario servidor-side** (fuente, tamaño, notesPath, fileSuffix).
10. **Código muerto relevante**: `Store.SearchNotes` (`store/notes.go` l.194, LIKE sobre título+contenido) **no está expuesto en ningún endpoint** — la búsqueda la hace el cliente en memoria. `crypto/secrets.go` tampoco se usa desde `main.go` (imgproxy gestiona su propio secret).

## 3. Uso desde la extensión (`extension/src/composables/api.ts`)

| Endpoint | Consumido en | ¿Depende de lógica no-WebDAV? |
|---|---|---|
| `GET /notes` | `NoteList.vue` l.40 (`loadNotes`) | Lista ordenada fav+fecha; el cliente recibe **contenido completo** de todas las notas y lo usa para preview (l.167) y búsqueda en memoria (l.77–80). Con WebDAV habría que GET de cada `.md` tras el PROPFIND. |
| `GET /notes/{id}` | — | **Definido en `api.ts` l.42 pero nunca llamado.** Código muerto en cliente. |
| `POST /notes` | `NotesApp.vue` l.81 | ID numérico generado por servidor. WebDAV: el "id" pasaría a ser la ruta del fichero. |
| `PUT /notes/{id}` (título/contenido) | `NoteEditor.vue` l.342 (`save`) | ETag + If-Match: WebDAV lo da gratis (`getetag`/`If-Match` en PUT). |
| `PUT /notes/{id}` (favorite) | `NoteEditor.vue` l.375 (`toggleFavorite`) | **Sin equivalente `.md`.** Alternativas: PROPPATCH de propiedad custom (soporte incierto en el WebDAV de OpenCloud), sidecar en cliente, o perderlo. |
| `PUT /notes/{id}` (category) | `NoteEditor.vue` l.387, `useDragNote.ts` l.55 (drag&drop a sidebar) | Categoría como metadato. Con WebDAV: mapear a subcarpetas (MOVE) o PROPPATCH; requiere reescribir el modelo de datos. |
| `DELETE /notes/{id}` | `NoteEditor.vue` l.358 | WebDAV DELETE directo; la cascada de adjuntos habría que rehacerla en cliente. |
| `GET/PUT /settings` | `NotesApp.vue` l.135, `SidebarNav.vue` l.92, l.110 | Sustituible por `localStorage` (ya usado para `listWidth`, `NotesApp.vue` l.93). **Ojo:** `notesPath`/`fileSuffix` se guardan en el servidor pero **ningún código los consume** — son settings zombie heredados del API Notes. |
| `POST /img/sign` | `NoteEditor.vue` l.492 (`buildPreview`) | **Sin sustituto sin backend**: la CSP `img-src 'self'` bloquea imágenes externas en el preview. |
| `POST /v1.4/attachment/{id}` | `NoteEditor.vue` l.294 (`onAttachmentSelected`) | WebDAV PUT a una subcarpeta de adjuntos lo resuelve bien (incluso más simple). |
| `GET /v1.4/attachment/{id}?path=` | `NoteEditor.vue` l.473 (`resolveAttachments`) | WebDAV GET + blob URL. Resoluble. |
| OCS capabilities/user | — | **No los usa la extensión.** Son para Iotas/otros clientes Nextcloud Notes. |

Funciones UI que dependen de lógica del backend que WebDAV no da gratis:
1. **Favoritos persistidos** (grupo "Favorites" en `NoteList.vue` l.84–85, estrella en editor).
2. **Imágenes externas en el preview** (proxy firmado, por CSP).
3. **Categorías agregadas**: se computan en cliente (`SidebarNav.vue` l.49–56) pero desde un campo `category` por nota que hoy vive en SQLite; con WebDAV hay que redefinir dónde vive (carpetas vs. propiedad).
4. **ID estable numérico** usado como clave de adjuntos y `key` de listas; desaparece (la ruta lo sustituiría).
5. **Clientes externos (Iotas)** vía API Notes + OCS: se pierden por completo.

## 4. Viabilidad de la Fase 1

| Capacidad | (a) WebDAV puro | (b) Cliente JS | (c) Sin sustituto |
|---|---|---|---|
| Listar notas (PROPFIND carpeta) | ✔ | | |
| Leer/crear/editar/borrar `.md` | ✔ GET/PUT/DELETE | | |
| Etag + If-Match (conflictos) | ✔ `getetag` + `If-Match` | | |
| Título (del nombre de fichero o 1ª línea) | ✔ | ✔ | |
| Búsqueda por título+contenido | parcial (hoy es en cliente) | ✔ (exige descargar todos los contenidos — coste N GETs) | |
| Categorías | ✔ si se mapean a subcarpetas (MOVE para recategorizar) | ✔ agregación en cliente como hoy | |
| Favoritos | ✖ estándar | solo local (localStorage, no sincroniza) | ✖ persistencia multi-dispositivo sin PROPPATCH fiable |
| Adjuntos | ✔ PUT/GET en subcarpeta | | |
| Settings (fuente, tamaño, vista) | | ✔ localStorage (patrón ya usado) | |
| Preview markdown | | ✔ ya es 100% cliente (markdown-it + DOMPurify) | |
| Imágenes externas en preview (CSP) | | ✖ (fetch desde cliente también choca con CSP/origen) | ✖ **necesita proxy servidor** |
| API Nextcloud Notes + OCS para Iotas/móviles | | | ✖ **se pierde entera** |
| Multiusuario | ✔ lo da la sesión del host | | |

**Veredicto: F1 es viable CON PÉRDIDAS**, y una de ellas es estructural, no cosmética:

1. **Pérdida 1 (grande): se apaga la API Nextcloud Notes para clientes externos.** El backend no es un mero sirve-ficheros para la web: es un servidor Notes v1.4 completo con OCS capabilities pensado para Iotas (citado en código y commits). Si el objetivo del producto incluye sincronización móvil, F1 directa es **no viable**; si el alcance es solo la app web, sigue.
2. **Pérdida 2 (media): imágenes externas en el preview.** Sin el proxy firmado no hay workaround en cliente puro por la CSP del host. Se puede degradar a "no cargar imágenes externas".
3. **Pérdida 3 (media): favoritos sincronizados.** Solo persisten localmente salvo que el WebDAV de OpenCloud acepte PROPPATCH de propiedades custom (a verificar).
4. **Rediseño (no pérdida):** categorías → carpetas, id numérico → path, carga perezosa de contenidos (hoy la UI asume tener todos los contenidos en memoria para buscar y previsualizar; con WebDAV hay que cambiar la estrategia de carga).

Además, **migración de datos**: las notas actuales viven en SQLite y los adjuntos en `<dataDir>/attachments/`; F1 necesita exportar a la carpeta WebDAV del usuario (decisión: categoría → subcarpeta; favoritos → ¿frontmatter?).

## 5. Estructura de la extensión

- **Tipo:** `defineWebApplication` de `@opencloud-eu/web-pkg` (`extension/src/index.ts` l.30) — es una **app web completa con ruta propia** (`/` montada bajo `/notes`, `authContext: 'user'`), **no** una extensión `folderView`/`customComponent`/`sidebarPanel`. El único punto de extensión declarado es `appMenuItem` (l.56–65). No hay ninguna referencia a `folderView` en el código (grep vacío).
- **Empaquetado:** Vite + `@opencloud-eu/extension-sdk` v7 (`extension/vite.config.ts`: `defineConfig({ name: 'web-app-notes' })`). El SDK genera el manifest en build; **no hay `manifest.json` en el repo** ni `dist/` commiteado (`.gitignore`). Estilos inyectados en runtime vía `<style>` (`index.ts` l.11–22).
- **`deploy/`: no existe.** Si el plan asume manifests/docker de despliegue, hay que crearlos o vivían fuera del repo.
- Estructura: `views/NotesApp.vue` (layout 3 columnas), `components/` (SidebarNav, NoteList, NoteEditor 713 líneas, NewNoteDialog, ModalDialog), `composables/` (api, theme, useDragNote), `stores/notes.ts` (estado reactivo global, sin Pinia), i18n con vue3-gettext (`gettext.config.cjs`, locales es/en).

## 6. Tests y build

- **Tests:** solo Go (6 ficheros, 963 líneas): API (`server_test.go`), adjuntos, migraciones, store, imgproxy, netguard. `go test ./...` debería cubrirlos. **Cero tests de frontend.**
- **Scripts npm** (`extension/package.json` l.6–9): solo `dev` y `build`. **No hay `check:types`** (aunque `vue-tsc` y `@opencloud-eu/tsconfig` están en devDependencies — fácil de añadir), ni `lint`, ni `test`.
- **CI: ninguna.** No hay `.github/`, `.woodpecker/`, ni ningún YAML de pipeline en el repo.
- **Go:** `go 1.25.0`, única dependencia directa `modernc.org/sqlite v1.56.0` (sin framework web; `net/http` stdlib).
- Config del backend por env: `OCNOTES_ADDR`, `OCNOTES_DATA_DIR`, `OCNOTES_GRAPH_URL`, `OCNOTES_AUTH_MODE`, `OCNOTES_OWNER` (`config/config.go`).

### Resumen ejecutivo
Las métricas del plan son exactas (2.905 Go / 1.790 TS+Vue contando `vite.config.ts`). Pero el plan subestima el backend: no es "un CRUD sobre SQLite", es un **servidor Nextcloud Notes v1.4 multiusuario con clientes externos (Iotas), proxy de imágenes anti-CSP, favoritos, sync incremental y adjuntos**. F1 (WebDAV puro) es viable para la app web con pérdidas concretas: API externa/OCS, imágenes externas en preview y favoritos sincronizados; y exige rediseño de categorías, ids y estrategia de carga, más una migración SQLite → WebDAV que el plan debe presupuestar.
