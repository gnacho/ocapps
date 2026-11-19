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
	// drives is the last listing of /me/drives (every space visible to the
	// owner: personal, project spaces and shares). It is refreshed by each
	// scan and read by the API to resolve the prefix of a space; it is empty
	// until the first resolution.
	drives []webdav.Drive
}

// registry is the ownerID -> session map of the module (H8 §6.2). It implements
// api.SessionProvider: the API touches the registry on every authenticated
// request with the credential of the request.
type registry struct {
	baseURL string
	log     *slog.Logger
	// spaces: drives the scan is limited to ("" = every visible drive, or the
	// keyword "personal"). It comes from OCAPPS_PHOTOS_SCAN_SPACES, see
	// resolveSpaces.
	spaces []string

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

func newRegistry(baseURL, legacyUser, legacyToken string, spaces []string, log *slog.Logger) *registry {
	return &registry{
		baseURL: baseURL, log: log, spaces: spaces,
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

// listDrives lists the spaces visible to the owner with ITS credential and
// refreshes the session cache. The cache is what the API uses to resolve the
// prefix of a space without a Graph call per request; a scan refreshes it, so a
// space created after the last scan is picked up by the next one.
func (r *registry) listDrives(ctx context.Context, sess *session, owner string) ([]webdav.Drive, error) {
	drives, err := sess.dav.ListDrives(ctx)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if cur, ok := r.sessions[owner]; ok {
		cur.drives = drives
	}
	r.mu.Unlock()
	return drives, nil
}

// selectDrives applies the configured filter (OCAPPS_PHOTOS_SCAN_SPACES) to the
// listing of drives: empty list = every drive with a webDavUrl (personal,
// project spaces and shares), "personal" = the personal drive, anything else is
// a drive id (the one after "$" in /dav/spaces/<storage>$<drive>) and an id
// that is not visible is an error, not a silent fallback (fail loud: a typo in
// the configuration must not leave the owner index at zero).
func (r *registry) selectDrives(drives []webdav.Drive) ([]webdav.Drive, error) {
	if len(r.spaces) == 0 {
		out := make([]webdav.Drive, 0, len(drives))
		for _, d := range drives {
			if d.WebDAVURL != "" {
				out = append(out, d)
			}
		}
		if len(out) == 0 {
			return nil, errors.New("no space with a webDavUrl available")
		}
		return out, nil
	}
	var out []webdav.Drive
	for _, want := range r.spaces {
		matched := false
		for _, d := range drives {
			if d.WebDAVURL == "" {
				continue
			}
			if want == "personal" {
				if d.DriveType != "personal" {
					continue
				}
			} else if d.ID != want && !strings.Contains(d.WebDAVURL, want) {
				continue
			}
			out = append(out, d)
			matched = true
			if want != "personal" {
				break // an id identifies one drive, the keyword may match several
			}
		}
		if !matched {
			return nil, fmt.Errorf("space %q not available for the owner (%d visible drives)", want, len(drives))
		}
	}
	return out, nil
}

// VisibleDrives returns the drives cached by the last listing of /me/drives
// (every space the owner can see, whether or not the scan is configured for
// it). nil when nothing has been listed yet. It is the "keep" set of the prune
// of spaces that disappeared (issue #70).
func (r *registry) VisibleDrives(owner string) []webdav.Drive {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sess, ok := r.sessions[owner]; ok {
		return sess.drives
	}
	return nil
}

// resolveSpaces returns the drives a scan of the owner must walk, with the
// listing just refreshed from /me/drives with the credential of the session.
func (r *registry) resolveSpaces(ctx context.Context, sess *session, owner string) ([]webdav.Drive, error) {
	drives, err := r.listDrives(ctx, sess, owner)
	if err != nil {
		return nil, err
	}
	selected, err := r.selectDrives(drives)
	if err != nil {
		return nil, err
	}
	// The cached single webDavUrl is the personal one (or the first selected):
	// the folders handler and the legacy single-space paths keep working.
	personal := ""
	for _, d := range selected {
		if d.DriveType == "personal" {
			personal = d.WebDAVURL
			break
		}
	}
	if personal == "" {
		personal = selected[0].WebDAVURL
	}
	r.mu.Lock()
	if cur, ok := r.sessions[owner]; ok {
		cur.webdavURL = personal
	}
	r.mu.Unlock()
	names := make([]string, 0, len(selected))
	for _, d := range selected {
		names = append(names, d.Name)
	}
	r.log.Info("OpenCloud spaces located", "owner", owner, "spaces", names, "configured", r.spaces)
	return selected, nil
}

// resolveSpace returns the webDavUrl of the personal space of the owner (the
// fallback is the first selected one) for the single-space callers (folders,
// resolve of "open with"). "" and an error when it cannot be resolved.
func (r *registry) resolveSpace(ctx context.Context, sess *session, owner string) (string, error) {
	spaces, err := r.resolveSpaces(ctx, sess, owner)
	if err != nil {
		return "", err
	}
	return spaces[0].WebDAVURL, nil
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

// SpacePrefixOf returns the path of the webDavUrl of a space of the owner (the
// folders handler of that space). An unknown or not visible space returns ""
// (the API answers with an empty listing, never a 500). It uses the drives
// cached by the last scan and lists them when the cache is empty (first request
// after a restart).
func (r *registry) SpacePrefixOf(ctx context.Context, owner, spaceID string) string {
	sess := r.get(owner)
	if sess == nil {
		return ""
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	drives := sess.drives
	if len(drives) == 0 {
		var err error
		if drives, err = r.listDrives(sctx, sess, owner); err != nil {
			r.log.Warn("spaces not listable", "owner", owner, "err", err)
			return ""
		}
	}
	for _, d := range drives {
		if d.ID != spaceID {
			continue
		}
		if u, err := url.Parse(d.WebDAVURL); err == nil {
			return strings.TrimRight(u.Path, "/")
		}
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
