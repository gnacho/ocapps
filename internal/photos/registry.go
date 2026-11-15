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

// sessionTTL is the inactivity after which a Bearer session is purged (H8
// §6.2). Basic sessions (durable app password) and those seeded by config
// (OCAPPS_PHOTOS_USERS) are NOT purged.
const sessionTTL = 24 * time.Hour

// session is the IN-MEMORY session of an owner (H8 §6.2): its DAV client with
// the credential of the request and the cached webDavUrl of its personal space.
// It is NEVER persisted to disk.
type session struct {
	dav       *webdav.Client
	webdavURL string // resolved personal space (lazy), "" if pending
	isBasic   bool   // Basic app-password credential (durable)
	seeded    bool   // seeded by config (background scan)
	expired   bool   // a scan detected 401/403 (expired Bearer)
	lastSeen  time.Time
}

// registry is the ownerID -> session map of the module (H8 §6.2). It implements
// api.SessionProvider: the API touches the registry on every authenticated
// request with the credential of the request.
type registry struct {
	baseURL string
	log     *slog.Logger
	// space: drive the scan is limited to ("" = personal). It comes from
	// OCAPPS_PHOTOS_SCAN_SPACE, see resolveSpace.
	space string

	// hooks to the scheduler (wired in New after building both, no lock
	// because they are set before serving the first request)
	touchHook func(owner string)
	scanHook  func(owner string)

	mu       sync.Mutex
	sessions map[string]*session

	// identity of the DEPRECATED static Bearer (OCAPPS_PHOTOS_TOKEN): the
	// oc_id of the legacy user OCAPPS_PHOTOS_USER, resolved lazily with its
	// app token and cached in memory.
	legacyUser  string
	legacyToken string
	legacyOwner string
}

func newRegistry(baseURL, legacyUser, legacyToken, space string, log *slog.Logger) *registry {
	return &registry{
		baseURL: baseURL, log: log, space: space,
		sessions:    map[string]*session{},
		legacyUser:  legacyUser,
		legacyToken: legacyToken,
	}
}

// Touch registers owner activity with the credential of the request: it creates
// or updates its session (the DAV client is rebuilt with the FRESH credential,
// an expired Bearer is replaced by the new token) and notifies the scheduler so
// that it enqueues a scan when due.
//
// EXCEPTION (H8 review): a SEEDED session (OCAPPS_PHOTOS_USERS) always keeps its
// Basic client with the app token: if Touch replaced it with the ephemeral OIDC
// Bearer of the request, the background scan of the ticker would die when that
// token expires. The user requests then use the app token of its session (same
// identity), it is the simplest variant and does not degrade the background
// scan.
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
	if !sess.seeded || !sess.isBasic {
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

// get returns a COPY of the owner session (or nil), taken under the lock: the
// readers (scheduler, API) never touch the mutable fields of the stored struct
// outside the mutex (Touch-vs-DavFor/scanOne race, H8 review).
func (r *registry) get(owner string) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sess, ok := r.sessions[owner]; ok {
		cp := *sess
		return &cp
	}
	return nil
}

// DavFor returns the DAV client of the owner session: nil if there is no
// session or if it is marked as expired (a scan detected 401/403, the API
// responds 503 until the next user request refreshes it through Touch with the
// new token).
func (r *registry) DavFor(_ context.Context, owner string) *webdav.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sess, ok := r.sessions[owner]; ok && !sess.expired {
		return sess.dav
	}
	return nil
}

// resolveSpace resolves (and caches) the webDavUrl of the personal space of the
// owner with ITS credential (driveType == "personal", fallback: the first one).
// sess is the snapshot copy from get(): the cache is written to the STORED
// session under the lock.
// space: id of the drive to use ("" = the personal one). An explicit space is
// always resolved against /me/drives, without trusting the cache: the cached
// personal one is useless when the owner has the photos in another drive.
func (r *registry) resolveSpace(ctx context.Context, sess *session, owner string) (string, error) {
	space := r.space
	if space == "" && sess.webdavURL != "" {
		return sess.webdavURL, nil
	}
	drives, err := sess.dav.ListDrives(ctx)
	if err != nil {
		return "", err
	}
	webdavURL := ""
	if space != "" {
		for _, d := range drives {
			if d.ID == space || strings.Contains(d.WebDAVURL, space) {
				webdavURL = d.WebDAVURL
				break
			}
		}
		if webdavURL == "" {
			return "", fmt.Errorf("space %q not available for the owner (%d visible drives)", space, len(drives))
		}
	} else {
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
			return "", errors.New("no space with a webDavUrl available")
		}
	}
	r.mu.Lock()
	if cur, ok := r.sessions[owner]; ok {
		cur.webdavURL = webdavURL
	}
	r.mu.Unlock()
	r.log.Info("OpenCloud space located", "owner", owner, "webdav", webdavURL, "configured", space)
	return webdavURL, nil
}

// SpacePrefix returns the path of the webDavUrl of the personal space of the
// owner (folders handler), resolving it on demand when needed. "" if it cannot
// (no session or unresolvable space: the API responds with a coherent empty
// value).
func (r *registry) SpacePrefix(ctx context.Context, owner string) string {
	sess := r.get(owner)
	if sess == nil {
		return ""
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	webdavURL, err := r.resolveSpace(sctx, sess, owner)
	if err != nil {
		r.log.Warn("user space not resolvable", "owner", owner, "err", err)
		return ""
	}
	if u, err := url.Parse(webdavURL); err == nil {
		return strings.TrimRight(u.Path, "/")
	}
	return ""
}

// LegacyOwner resolves the oc_id of the legacy user OCAPPS_PHOTOS_USER with its
// app token (identity of the DEPRECATED static Bearer and of the backfill). It
// is cached in memory, on failure it is retried on the next call.
func (r *registry) LegacyOwner(ctx context.Context) (string, error) {
	r.mu.Lock()
	cached := r.legacyOwner
	r.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	if r.legacyUser == "" || r.legacyToken == "" {
		return "", errors.New("OCAPPS_PHOTOS_USER/APP_TOKEN not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	id, err := webdav.New(r.baseURL, r.legacyUser, r.legacyToken).MeID(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve oc_id of the legacy user: %w", err)
	}
	r.mu.Lock()
	r.legacyOwner = id
	r.mu.Unlock()
	return id, nil
}

// RequestScan enqueues a scan of the owner only (POST /api/admin/rescan).
func (r *registry) RequestScan(owner string) {
	if r.scanHook != nil {
		r.scanHook(owner)
	}
}

// seed creates the session of a user with an app token from OCAPPS_PHOTOS_USERS
// (background scan): Basic client plus resolution of MeID and personal space.
// It returns the oc_id of the user.
func (r *registry) seed(ctx context.Context, user, appToken string) (string, error) {
	dav := webdav.New(r.baseURL, user, appToken)
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	owner, err := dav.MeID(sctx)
	if err != nil {
		return "", fmt.Errorf("MeID of %q: %w", user, err)
	}
	sess := &session{dav: dav, isBasic: true, seeded: true, lastSeen: time.Now()}
	r.mu.Lock()
	r.sessions[owner] = sess
	r.mu.Unlock()
	if _, err := r.resolveSpace(sctx, r.get(owner), owner); err != nil {
		return owner, fmt.Errorf("space of %q: %w", user, err)
	}
	return owner, nil
}

// markExpired invalidates the cached space of a Bearer session whose credential
// turned out to be expired (401/403 in a scan): the next Touch of the user
// refreshes it with the new token. Basic sessions are not marked (an app token
// does not expire, a 401 there is a real credential change).
func (r *registry) markExpired(owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sess, ok := r.sessions[owner]; ok && !sess.isBasic {
		sess.webdavURL = ""
		sess.expired = true
	}
}

// purge removes the Bearer sessions without activity for more than sessionTTL
// (24h, H8 §6.2). Basic and config-seeded sessions are not purged. It returns
// how many were purged.
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

// activeBasicOwners returns the owners with an active Basic or seeded session
// (the ones the scheduled ticker enqueues scans for, H8 §7).
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
