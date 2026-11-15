# Audit report: ocnotes (OpenCloud Notes)

Audited repo: `/mnt/agents/work/opencloud/ocnotes` (HEAD `2c0ff6b`, v0.1.4). Read-only, nothing modified.

---

## 1. Real metrics vs. plan

| Metric | Plan | Real | Verdict |
|---|---|---|---|
| Go lines | 2,905 | **2,905** | exact |
| Go files | 18 | **18** | exact |
| TS+Vue lines | 1,790 | **1,785** in `extension/src` (12 files: 6 `.ts` + 6 `.vue`) | discrepancy of 5 lines = `extension/vite.config.ts` (5 lines). The plan counted TS outside `src/` too. With it: exactly 1,790. |

Nuances the plan does not break down:
- Of the 2,905 Go lines, **963 are tests** (6 `_test.go` files): `server_test.go` 249, `attachments_test.go` 240, `migrate_test.go` 198, `store_test.go` 139, `imgproxy_test.go` 83, `netguard_test.go` 54. Production Go code: **1,942 lines in 12 files**.
- The TS/Vue figure excludes `styles.css` (756 lines) and `l10n/translations.json` (148).
- **There is no `deploy/`** in the repo (the plan context mentions one, but it is absent). There is also no `manifest.json`, `Dockerfile`, `Makefile` or CI.

## 2. Backend inventory

### Exact endpoints (`backend/internal/api/server.go`, router on lines 87-128)

Base `v1` = `/index.php/apps/notes/api/v1` and `v1.4` = `/index.php/apps/notes/api/v1.4` (constants on lines 25-26):

| Method | Path | Handler | Use |
|---|---|---|---|
| GET | `/v1/notes` | `handleGetNotes` (l.340) | List with `category`, `exclude`, `pruneBefore`, `chunkSize` (incremental sync protocol of the Notes API) |
| POST | `/v1/notes` | `handleCreateNote` (l.451) | Create |
| GET | `/v1/notes/{id}` | `handleGetNote` (l.477) | Read (returns the ETag) |
| PUT | `/v1/notes/{id}` | `handleUpdateNote` (l.494) | Update title, content, category or favourite, with `If-Match` -> 412 (l.506-512) |
| DELETE | `/v1/notes/{id}` | `handleDeleteNote` (l.541) | Delete plus attachment cascade (l.553-557) |
| GET/PUT | `/v1/settings` | `handleGetSettings`/`handlePutSettings` (l.563, 575) | Per-user settings |
| POST | `/v1/img/sign` | `handleImgSign` (l.132) | HMAC signature of external image URLs |
| GET | `/v1/img?u=&t=` | `imgproxy.Serve` (public, no auth, signed) | Image proxy with an on-disk cache |
| POST/GET | `/v1.4/attachment/{id}` | `handleAttachment` (l.165) | Upload (multipart, <= 32 MiB) or download attachments |
| GET | `/ocs/v2.php/cloud/capabilities` | `handleCapabilities` (l.623) | OCS XML/JSON, announces `api_version ["0.2","1.4"]`, version 6.0.0 |
| GET | `/ocs/v2.php/cloud/user` | `handleUserInfo` (l.667) | OCS user info |

All responses carry `X-Notes-API-Versions: 0.2, 1.4` (l.27-28).

### SQLite schema (`backend/internal/store/migrations/`)

- **`notes` table** (001_initial.sql l.4-12 + 002_multiuser.sql l.4): `id INTEGER PK AUTOINCREMENT, title, content, category, favorite INTEGER, modified INTEGER, etag TEXT, user TEXT`. Indexes on category, favorite, modified, user, (user,category) and (user,modified).
- **`settings` table** (rebuilt in 002 l.10-22): `(user, key, value)` with a composite PK. Defaults seeded in code: `notesPath="Notes"`, `fileSuffix=".md"` (`store/notes.go` l.237).
- Attachments are **not** in SQLite, they go to disk at `<dataDir>/attachments/<noteID>/<name>` (`attachments/attachments.go`).

### Logic that goes BEYOND a trivial CRUD over `.md` (risks for F1)

1. **It is a Nextcloud Notes API server for external clients, not just for the extension.** The comments cite *Iotas* (the Notes Android client) in `server.go` l.52-55 and l.594, and the OCS endpoints (capabilities and user) only exist for external clients. The commits `e7704cf fix(api): close the Notes API gaps that break Iotas` and `9f1de3c feat(api): implement the Notes API v1.4 attachment endpoints` confirm it. **Removing the backend breaks synchronisation with mobile and desktop clients**, not just the web app.
2. **Multi-user authentication against Graph** (`auth/auth.go`): it validates Basic and Bearer against `OCNOTES_GRAPH_URL`, caches "shadow users" for 5 min, and **scopes every note by graph ID** (migration 002_multiuser). The host WebDAV would give this for free for the web app, but this is what lets Iotas authenticate.
3. **Persisted favourites** (the `favorite` column, ordered `favorite DESC, modified DESC` in `store/notes.go` l.98). It is not derivable from the `.md` content.
4. **Categories as per-note metadata** (the `category` column, filterable by query param). They are not real folders.
5. **Etags and conflict control**: `If-Match` -> 412 with the current note in the body (`server.go` l.506-512), an aggregate SHA-256 ETag in the list (l.370), and a `Last-Modified` header.
6. **Incremental sync protocol**: `pruneBefore` (notes pruned down to `{id}`), `chunkSize` plus `X-Notes-Chunk-Cursor`, `exclude=content` (l.340-449). Designed for clients with a local cache.
7. **HMAC-SHA256 signed image proxy** (`imgproxy/imgproxy.go`): it exists because the host CSP is `img-src 'self'` and the preview cannot load external images. It includes SSRF mitigation (`netguard`: rejects private, loopback and link-local IPs), an on-disk cache, an 8 MB limit and a persistent secret. **It has no WebDAV substitute.**
8. **Attachments with Nextcloud 6.1+ style name deduplication** (`name (1).ext`), atomic writes, a 32 MiB limit, `.attachments.<id>/` paths and cascading delete.
9. **Server-side per-user settings** (source, size, notesPath, fileSuffix).
10. **Relevant dead code**: `Store.SearchNotes` (`store/notes.go` l.194, LIKE over title and content) **is not exposed by any endpoint**, because the search is done by the client in memory. `crypto/secrets.go` is not used from `main.go` either (imgproxy manages its own secret).

## 3. Usage from the extension (`extension/src/composables/api.ts`)

| Endpoint | Consumed in | Does it depend on non-WebDAV logic? |
|---|---|---|
| `GET /notes` | `NoteList.vue` l.40 (`loadNotes`) | List ordered by favourite and date. The client receives the **full content** of every note and uses it for previews (l.167) and in-memory search (l.77-80). With WebDAV it would have to GET every `.md` after the PROPFIND. |
| `GET /notes/{id}` | - | **Defined in `api.ts` l.42 but never called.** Dead code on the client. |
| `POST /notes` | `NotesApp.vue` l.81 | Numeric ID generated by the server. With WebDAV the "id" would become the file path. |
| `PUT /notes/{id}` (title/content) | `NoteEditor.vue` l.342 (`save`) | ETag plus If-Match, which WebDAV gives for free (`getetag`/`If-Match` on PUT). |
| `PUT /notes/{id}` (favorite) | `NoteEditor.vue` l.375 (`toggleFavorite`) | **No `.md` equivalent.** Alternatives: PROPPATCH of a custom property (uncertain support in the OpenCloud WebDAV), a client-side sidecar, or losing it. |
| `PUT /notes/{id}` (category) | `NoteEditor.vue` l.387, `useDragNote.ts` l.55 (drag and drop to the sidebar) | Category as metadata. With WebDAV: map to subfolders (MOVE) or PROPPATCH, which requires rewriting the data model. |
| `DELETE /notes/{id}` | `NoteEditor.vue` l.358 | Direct WebDAV DELETE, and the attachment cascade would have to be redone on the client. |
| `GET/PUT /settings` | `NotesApp.vue` l.135, `SidebarNav.vue` l.92, l.110 | Replaceable by `localStorage` (already used for `listWidth`, `NotesApp.vue` l.93). **Note:** `notesPath`/`fileSuffix` are stored on the server but **no code consumes them**, as they are zombie settings inherited from the Notes API. |
| `POST /img/sign` | `NoteEditor.vue` l.492 (`buildPreview`) | **No substitute without a backend**: the `img-src 'self'` CSP blocks external images in the preview. |
| `POST /v1.4/attachment/{id}` | `NoteEditor.vue` l.294 (`onAttachmentSelected`) | A WebDAV PUT to an attachments subfolder handles it well (even more simply). |
| `GET /v1.4/attachment/{id}?path=` | `NoteEditor.vue` l.473 (`resolveAttachments`) | WebDAV GET plus a blob URL. Solvable. |
| OCS capabilities and user | - | **The extension does not use them.** They are for Iotas and other Nextcloud Notes clients. |

UI features that depend on backend logic WebDAV does not give for free:
1. **Persisted favourites** (the "Favorites" group in `NoteList.vue` l.84-85, the star in the editor).
2. **External images in the preview** (signed proxy, because of the CSP).
3. **Aggregated categories**: they are computed on the client (`SidebarNav.vue` l.49-56) but from a per-note `category` field that lives in SQLite today. With WebDAV it has to be redefined where it lives (folders vs. a property).
4. **Stable numeric ID** used as the key for attachments and as the `key` of lists: it disappears, and the path would replace it.
5. **External clients (Iotas)** through the Notes API plus OCS: entirely lost.

## 4. Feasibility of Phase 1

| Capability | (a) Pure WebDAV | (b) JS client | (c) No substitute |
|---|---|---|---|
| List notes (PROPFIND on the folder) | yes | | |
| Read/create/edit/delete `.md` | yes GET/PUT/DELETE | | |
| Etag plus If-Match (conflicts) | yes `getetag` + `If-Match` | | |
| Title (from the file name or the first line) | yes | yes | |
| Search by title and content | partial (it is client-side today) | yes (requires downloading every content, N GETs of cost) | |
| Categories | yes if mapped to subfolders (MOVE to recategorise) | yes, aggregated on the client as today | |
| Favourites | no, not standard | local only (localStorage, no sync) | no multi-device persistence without a reliable PROPPATCH |
| Attachments | yes PUT/GET in a subfolder | | |
| Settings (source, size, view) | | yes, localStorage (a pattern already in use) | |
| Markdown preview | | yes, it is already 100% client-side (markdown-it + DOMPurify) | |
| External images in the preview (CSP) | | no (fetching from the client hits the CSP and origin too) | **needs a server proxy** |
| Nextcloud Notes API plus OCS for Iotas and mobiles | | | **entirely lost** |
| Multi-user | yes, the host session provides it | | |

**Verdict: F1 is feasible WITH LOSSES**, and one of them is structural, not cosmetic:

1. **Loss 1 (big): the Nextcloud Notes API for external clients goes away.** The backend is not a mere file server for the web app, it is a full Notes v1.4 server with OCS capabilities designed for Iotas (cited in code and commits). If the product goal includes mobile sync, a straight F1 is **not viable**. If the scope is only the web app, it holds.
2. **Loss 2 (medium): external images in the preview.** Without the signed proxy there is no workaround on a pure client because of the host CSP. It can be degraded to "do not load external images".
3. **Loss 3 (medium): synced favourites.** They only persist locally unless the OpenCloud WebDAV accepts PROPPATCH of custom properties (to be verified).
4. **Redesign (not a loss):** categories become folders, the numeric id becomes a path, and content loading becomes lazy (today the UI assumes it has every content in memory to search and preview, and with WebDAV the loading strategy has to change).

On top of that, **data migration**: the current notes live in SQLite and the attachments in `<dataDir>/attachments/`, so F1 needs to export them to the user's WebDAV folder (decision: category to subfolder, favourites to frontmatter?).

## 5. Extension structure

- **Type:** `defineWebApplication` from `@opencloud-eu/web-pkg` (`extension/src/index.ts` l.30), which makes it a **full web app with its own route** (`/` mounted under `/notes`, `authContext: 'user'`), **not** a `folderView`/`customComponent`/`sidebarPanel` extension. The only declared extension point is `appMenuItem` (l.56-65). There is no reference to `folderView` anywhere in the code (empty grep).
- **Packaging:** Vite plus `@opencloud-eu/extension-sdk` v7 (`extension/vite.config.ts`: `defineConfig({ name: 'web-app-notes' })`). The SDK generates the manifest at build time, so **there is no `manifest.json` in the repo** and no committed `dist/` (`.gitignore`). Styles are injected at runtime through `<style>` (`index.ts` l.11-22).
- **`deploy/`: it does not exist.** If the plan assumes deployment manifests or Docker files, they have to be created or they lived outside the repo.
- Structure: `views/NotesApp.vue` (3-column layout), `components/` (SidebarNav, NoteList, NoteEditor at 713 lines, NewNoteDialog, ModalDialog), `composables/` (api, theme, useDragNote), `stores/notes.ts` (global reactive state, no Pinia), and i18n with vue3-gettext (`gettext.config.cjs`, es/en locales).

## 6. Tests and build

- **Tests:** Go only (6 files, 963 lines): API (`server_test.go`), attachments, migrations, store, imgproxy, netguard. `go test ./...` should cover them. **Zero frontend tests.**
- **npm scripts** (`extension/package.json` l.6-9): only `dev` and `build`. **There is no `check:types`** (even though `vue-tsc` and `@opencloud-eu/tsconfig` are in devDependencies, so it is easy to add), nor `lint`, nor `test`.
- **CI: none.** There is no `.github/`, no `.woodpecker/`, and no pipeline YAML of any kind in the repo.
- **Go:** `go 1.25.0`, with `modernc.org/sqlite v1.56.0` as the only direct dependency (no web framework, just the `net/http` stdlib).
- Backend config through env: `OCNOTES_ADDR`, `OCNOTES_DATA_DIR`, `OCNOTES_GRAPH_URL`, `OCNOTES_AUTH_MODE`, `OCNOTES_OWNER` (`config/config.go`).

### Executive summary
The plan metrics are exact (2,905 Go lines and 1,790 TS+Vue lines counting `vite.config.ts`). But the plan underestimates the backend: it is not "a CRUD over SQLite", it is a **multi-user Nextcloud Notes v1.4 server with external clients (Iotas), an anti-CSP image proxy, favourites, incremental sync and attachments**. F1 (pure WebDAV) is viable for the web app with concrete losses: the external API and OCS, external images in the preview, and synced favourites. It also demands a redesign of categories, ids and loading strategy, plus a SQLite to WebDAV migration that the plan must budget for.
