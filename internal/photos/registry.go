package photos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/webdav"
)

// sessionTTL es la inactividad tras la que se purga una sesión Bearer (H8
// §6.2). Las sesiones Basic (app-password duradero) y las sembradas por
// config (OCAPPS_PHOTOS_USERS) NO se purgan.
const sessionTTL = 24 * time.Hour

// session es la sesión EN MEMORIA de un owner (H8 §6.2): su cliente DAV con
// la credencial del request y el webDavUrl de su espacio personal cacheado.
// NUNCA se persiste a disco.
type session struct {
	dav       *webdav.Client
	webdavURL string // espacio personal resuelto (lazy); "" si pendiente
	isBasic   bool   // credencial Basic app-password (duradera)
	seeded    bool   // sembrada por config (background scan)
	expired   bool   // un scan detectó 401/403 (Bearer caducado)
	lastSeen  time.Time
}

// registry es el mapa ownerID → sesión del módulo (H8 §6.2). Implementa
// api.SessionProvider: la API toca el registry en cada request autenticado
// con la credencial del request.
type registry struct {
	baseURL string
	log     *slog.Logger

	// hooks al scheduler (se cablean en New tras construir ambos; sin lock
	// porque se fijan antes de servir el primer request)
	touchHook func(owner string)
	scanHook  func(owner string)

	mu       sync.Mutex
	sessions map[string]*session

	// identidad del Bearer estático DEPRECATED (OCAPPS_PHOTOS_TOKEN): el
	// oc_id del usuario legacy OCAPPS_PHOTOS_USER, resuelto lazy con su
	// app-token y cacheado en memoria.
	legacyUser  string
	legacyToken string
	legacyOwner string
}

func newRegistry(baseURL, legacyUser, legacyToken string, log *slog.Logger) *registry {
	return &registry{
		baseURL: baseURL, log: log,
		sessions:    map[string]*session{},
		legacyUser:  legacyUser,
		legacyToken: legacyToken,
	}
}

// Touch registra actividad del owner con la credencial del request: crea o
// actualiza su sesión (el cliente DAV se reconstruye con la credencial
// FRESCA — un Bearer caducado se reemplaza por el nuevo token) y avisa al
// scheduler para que encole un scan si toca.
//
// EXCEPCIÓN (H8 review): una sesión SEMBRADA (OCAPPS_PHOTOS_USERS) conserva
// siempre su cliente Basic con app-token: si Touch lo reemplazara por el
// Bearer OIDC efímero del request, el background scan del ticker moriría al
// caducar ese token. Los requests del usuario usan entonces el app-token de
// su sesión (misma identidad) — es la variante más simple y no degrada el
// background scan.
func (r *registry) Touch(_ context.Context, u *auth.User, cred auth.Credential) {
	if u == nil || u.ID == "" {
		return
	}
	r.mu.Lock()
	sess, ok := r.sessions[u.ID]
	if !ok {
		sess = &session{}
		r.sessions[u.ID] = sess
	}
	if !(sess.seeded && sess.isBasic) {
		if cred.Bearer != "" {
			sess.dav = webdav.NewBearer(r.baseURL, cred.Bearer)
			sess.isBasic = false
		} else {
			sess.dav = webdav.New(r.baseURL, cred.Username, cred.Password)
			sess.isBasic = true
		}
	}
	sess.expired = false
	sess.lastSeen = time.Now()
	r.mu.Unlock()
	if r.touchHook != nil {
		r.touchHook(u.ID)
	}
}

// get devuelve una COPIA de la sesión del owner (o nil), tomada bajo lock:
// los lectores (scheduler, API) nunca tocan los campos mutables del struct
// almacenado fuera del mutex (race Touch-vs-DavFor/scanOne, H8 review).
func (r *registry) get(owner string) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sess, ok := r.sessions[owner]; ok {
		cp := *sess
		return &cp
	}
	return nil
}

// DavFor devuelve el cliente DAV de la sesión del owner: nil si no hay
// sesión o si está marcada como caducada (un scan detectó 401/403; la API
// responde 503 hasta que el próximo request del usuario la refresque vía
// Touch con su token nuevo).
func (r *registry) DavFor(_ context.Context, owner string) *webdav.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sess, ok := r.sessions[owner]; ok && !sess.expired {
		return sess.dav
	}
	return nil
}

// resolveSpace resuelve (y cachea) el webDavUrl del espacio personal del
// owner con SU credencial (driveType == "personal", fallback: el primero).
// sess es la copia snapshot de get(): la caché se escribe en la sesión
// ALMACENADA bajo lock.
func (r *registry) resolveSpace(ctx context.Context, sess *session, owner string) (string, error) {
	if sess.webdavURL != "" {
		return sess.webdavURL, nil
	}
	drives, err := sess.dav.ListDrives(ctx)
	if err != nil {
		return "", err
	}
	webdavURL := ""
	for _, d := range drives {
		if d.DriveType == "personal" {
			webdavURL = d.WebDAVURL
			break
		}
	}
	if webdavURL == "" && len(drives) > 0 {
		webdavURL = drives[0].WebDAVURL
	}
	if webdavURL == "" {
		return "", errors.New("ningún espacio con webDavUrl disponible")
	}
	r.mu.Lock()
	if cur, ok := r.sessions[owner]; ok {
		cur.webdavURL = webdavURL
	}
	r.mu.Unlock()
	r.log.Info("espacio OpenCloud localizado", "owner", owner, "webdav", webdavURL)
	return webdavURL, nil
}

// SpacePrefix devuelve el path del webDavUrl del espacio personal del owner
// (handler de carpetas), resolviéndolo on-demand si hace falta. "" si no se
// puede (sin sesión o espacio no resoluble: la API responde vacío coherente).
func (r *registry) SpacePrefix(ctx context.Context, owner string) string {
	sess := r.get(owner)
	if sess == nil {
		return ""
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	webdavURL, err := r.resolveSpace(sctx, sess, owner)
	if err != nil {
		r.log.Warn("espacio del usuario no resoluble", "owner", owner, "err", err)
		return ""
	}
	if u, err := url.Parse(webdavURL); err == nil {
		return strings.TrimRight(u.Path, "/")
	}
	return ""
}

// LegacyOwner resuelve el oc_id del usuario legacy OCAPPS_PHOTOS_USER con su
// app-token (identidad del Bearer estático DEPRECATED y del backfill). Se
// cachea en memoria; si falla se reintenta en la próxima llamada.
func (r *registry) LegacyOwner(ctx context.Context) (string, error) {
	r.mu.Lock()
	cached := r.legacyOwner
	r.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	if r.legacyUser == "" || r.legacyToken == "" {
		return "", errors.New("OCAPPS_PHOTOS_USER/APP_TOKEN no configurados")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	id, err := webdav.New(r.baseURL, r.legacyUser, r.legacyToken).MeID(ctx)
	if err != nil {
		return "", fmt.Errorf("resolver oc_id del usuario legacy: %w", err)
	}
	r.mu.Lock()
	r.legacyOwner = id
	r.mu.Unlock()
	return id, nil
}

// RequestScan encola un scan solo del owner (POST /api/admin/rescan).
func (r *registry) RequestScan(owner string) {
	if r.scanHook != nil {
		r.scanHook(owner)
	}
}

// seed crea la sesión de un usuario con app-token de OCAPPS_PHOTOS_USERS
// (background scan): cliente Basic + resolución de MeID y espacio personal.
// Devuelve el oc_id del usuario.
func (r *registry) seed(ctx context.Context, user, appToken string) (string, error) {
	dav := webdav.New(r.baseURL, user, appToken)
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	owner, err := dav.MeID(sctx)
	if err != nil {
		return "", fmt.Errorf("MeID de %q: %w", user, err)
	}
	sess := &session{dav: dav, isBasic: true, seeded: true, lastSeen: time.Now()}
	r.mu.Lock()
	r.sessions[owner] = sess
	r.mu.Unlock()
	if _, err := r.resolveSpace(sctx, r.get(owner), owner); err != nil {
		return owner, fmt.Errorf("espacio de %q: %w", user, err)
	}
	return owner, nil
}

// markExpired invalida el espacio cacheado de una sesión Bearer cuya
// credencial resultó caducada (401/403 en un scan): el próximo Touch del
// usuario la refresca con su token nuevo. Las sesiones Basic no se marcan
// (un app-token no caduca; un 401 ahí es un cambio de credencial real).
func (r *registry) markExpired(owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sess, ok := r.sessions[owner]; ok && !sess.isBasic {
		sess.webdavURL = ""
		sess.expired = true
	}
}

// purge elimina las sesiones Bearer sin actividad > sessionTTL (24h, H8
// §6.2). Las Basic y las sembradas por config no se purgan. Devuelve cuántas
// purgó.
func (r *registry) purge(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for owner, sess := range r.sessions {
		if sess.isBasic || sess.seeded {
			continue
		}
		if now.Sub(sess.lastSeen) > sessionTTL {
			delete(r.sessions, owner)
			n++
		}
	}
	return n
}

// activeBasicOwners devuelve los owners con sesión Basic o sembrada activa
// (a los que el ticker programado encola scans, H8 §7).
func (r *registry) activeBasicOwners() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for owner, sess := range r.sessions {
		if sess.isBasic || sess.seeded {
			out = append(out, owner)
		}
	}
	return out
}
