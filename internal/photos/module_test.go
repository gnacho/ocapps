package photos

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/config"
)

// fakeOpenCloud simulates Graph (/me, /me/drives) and WebDAV (PROPFIND) for the
// multi-tenant wiring. propfind decides the answer of each PROPFIND path (nil ->
// 404 for everything).
func fakeOpenCloud(t *testing.T, propfind func(path string) (int, string)) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/graph/v1.0/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "user-1", "onPremisesSamAccountName": "admin"})
		case r.URL.Path == "/graph/v1.0/me/drives":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{
				"id": "d1", "name": "Personal", "driveType": "personal",
				"root": map[string]any{"webDavUrl": srv.URL + "/dav/spaces/1"},
			}}})
		case r.Method == "PROPFIND":
			if propfind == nil {
				http.NotFound(w, r)
				return
			}
			code, body := propfind(r.URL.Path)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(code)
			_, _ = fmt.Fprint(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeOpenCloudReq is fakeOpenCloud with access to the *http.Request (for
// example to demand Basic auth on the PROPFINDs).
func fakeOpenCloudReq(t *testing.T, propfind func(r *http.Request) (int, string)) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/graph/v1.0/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "user-1", "onPremisesSamAccountName": "admin"})
		case r.URL.Path == "/graph/v1.0/me/drives":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{
				"id": "d1", "name": "Personal", "driveType": "personal",
				"root": map[string]any{"webDavUrl": srv.URL + "/dav/spaces/1"},
			}}})
		case r.Method == "PROPFIND":
			code, body := propfind(r)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(code)
			_, _ = fmt.Fprint(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const onePhotoXML = `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">
<d:response><d:href>/dav/spaces/1/Fotos/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
<d:response><d:href>/dav/spaces/1/Fotos/IMG.jpg</d:href><d:propstat><d:prop><d:resourcetype/><d:getetag>"e1"</d:getetag><d:getlastmodified>Mon, 02 Jan 2006 15:04:05 GMT</d:getlastmodified><d:getcontentlength>100</d:getcontentlength><d:getcontenttype>image/jpeg</d:getcontenttype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
</d:multistatus>`

func propfindOnePhoto(path string) (int, string) {
	if path == "/dav/spaces/1/Fotos/" {
		return http.StatusMultiStatus, onePhotoXML
	}
	return http.StatusNotFound, ""
}

func photosCfg() config.PhotosConfig {
	return config.PhotosConfig{
		User: "admin", AppToken: "app-token", Token: "test-token",
		ScanRoot: "Fotos", ScanEvery: time.Hour,
	}
}

// testConfig wraps the sections in the full unified config New expects (without
// ModuleErr: manual config = valid config).
func testConfig(common config.Common, pcfg config.PhotosConfig) *config.Config {
	return &config.Config{Common: common, Photos: pcfg}
}

// TestNewWithoutCredentialsOK: OCAPPS_PHOTOS_USER/APP_TOKEN are no longer
// mandatory (H8), the module starts without them and does not depend on
// OpenCloud.
func TestNewWithoutCredentialsOK(t *testing.T) {
	m, err := New(testConfig(config.Common{OpenCloudURL: "http://127.0.0.1:1", PhotosDataDir: t.TempDir()}, config.PhotosConfig{ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatalf("New without credentials must not fail (H8): %v", err)
	}
	if err := m.Healthy(); err != nil {
		t.Fatalf("Healthy = SQLite ping, without OpenCloud (H8): %v", err)
	}
	// the namespace serves the real routes from startup (no init 503)
	mux := http.NewServeMux()
	m.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, PublicPrefix+"/api/stats", nil))
	if rr.Code == http.StatusServiceUnavailable {
		t.Fatal("the module no longer serves an init 503 (H8), the routes respond from startup")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("stats without auth -> %d, want 401", rr.Code)
	}
}

// TestNewTokenWithoutUserFails: the DEPRECATED static Bearer without
// OCAPPS_PHOTOS_USER has no identity -> config error (H8 §6.2).
func TestNewTokenWithoutUserFails(t *testing.T) {
	pcfg := config.PhotosConfig{Token: "test-token", ScanEvery: time.Hour}
	_, err := New(testConfig(config.Common{OpenCloudURL: "http://x", PhotosDataDir: t.TempDir()}, pcfg), slog.Default(), nil)
	if err == nil || !strings.Contains(err.Error(), "OCAPPS_PHOTOS_TOKEN") {
		t.Fatalf("Token without User must be a config error: %v", err)
	}
}

// I1: a ModuleErr("photos") registered by config.Load (for example
// OCAPPS_PHOTOS_SCAN_EVERY=0s, which PARSES fine but is invalid) must bring New
// down - before, the module started "healthy" and panicked in
// time.NewTicker(<=0) after the initial scan.
func TestNewFailsWithInvalidScanEvery(t *testing.T) {
	for _, raw := range []string{"0s", "-5m", "not-a-duration"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("OCAPPS_AUTH_MODE", "local") // no mandatory OPENCLOUD_URL
			t.Setenv("OCAPPS_DATA_DIR", t.TempDir())
			t.Setenv("OCAPPS_PHOTOS_SCAN_EVERY", raw)
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load: %v (an invalid SCAN_EVERY degrades the module, it is not fatal)", err)
			}
			if cfg.ModuleErr("photos") == nil {
				t.Fatalf("SCAN_EVERY=%q should register ModuleErr(photos)", raw)
			}
			_, err = New(cfg, slog.Default(), nil)
			if err == nil {
				t.Fatalf("New with SCAN_EVERY=%q should fail (module failed in init, D3)", raw)
			}
		})
	}
}

// TestModuleWithoutInitAgainstOpenCloud: with OpenCloud DOWN the module starts
// and is healthy anyway (H8 §7: there is no initBackend and no retry loop, the
// scans are postponed to the user activity).
func TestModuleWithoutInitAgainstOpenCloud(t *testing.T) {
	common := config.Common{OpenCloudURL: "http://127.0.0.1:1", PhotosDataDir: t.TempDir()}
	m, err := New(testConfig(common, photosCfg()), slog.Default(), nil)
	if err != nil {
		t.Fatalf("New with OpenCloud down: %v", err)
	}
	if err := m.Healthy(); err != nil {
		t.Fatalf("Healthy with OpenCloud down: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// the backfill cannot resolve the legacy owner (Graph down): log and go on
	if err := m.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestModuleOKAgainstFakeGraph(t *testing.T) {
	srv := fakeOpenCloud(t, propfindOnePhoto)
	dataDir := t.TempDir()
	common := config.Common{OpenCloudURL: srv.URL, PhotosDataDir: dataDir}
	m, err := New(testConfig(common, photosCfg()), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Healthy(); err != nil {
		t.Fatalf("Healthy: %v", err)
	}

	// Q3: mediasecret persisted with 0600 permissions
	sec := filepath.Join(dataDir, "mediasecret")
	fi, err := os.Stat(sec)
	if err != nil {
		t.Fatalf("mediasecret not persisted: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mediasecret permissions = %o, want 600", fi.Mode().Perm())
	}

	// real routes under the prefix with the static token (it maps to the legacy
	// owner resolved against the fake Graph)
	mux := http.NewServeMux()
	m.Register(mux)
	req := httptest.NewRequest(http.MethodGet, PublicPrefix+"/api/stats", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("stats -> %d (%s)", rr.Code, rr.Body.String())
	}

	// Run returns when ctx is cancelled
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancelling ctx")
	}
}

// TestTouchTriggersScanOfEmptyIndex: the first activity of a user (an
// authenticated request -> Touch) enqueues its scan and fills ITS index (H8 §7).
func TestTouchTriggersScanOfEmptyIndex(t *testing.T) {
	srv := fakeOpenCloud(t, propfindOnePhoto)
	common := config.Common{OpenCloudURL: srv.URL, PhotosDataDir: t.TempDir()}
	m, err := New(testConfig(common, photosCfg()), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	// request with Basic app-password -> validated against the fake Graph -> Touch
	mux := http.NewServeMux()
	m.Register(mux)
	req := httptest.NewRequest(http.MethodGet, PublicPrefix+"/api/stats", nil)
	req.SetBasicAuth("admin", "app-token")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("stats -> %d", rr.Code)
	}

	// the scan triggered by the activity indexes the photo for user-1
	waitFor(t, 5*time.Second, func() bool {
		stats, err := m.st.Stats(context.Background(), "user-1")
		return err == nil && stats["assets"].(int64) == 1
	}, "the activity scan did not index the user photo")
}

// TestSeededUserScansOnTicker: a user of OCAPPS_PHOTOS_USERS is seeded when Run
// starts and the ScanEvery ticker enqueues its scan (intervals injectable for the
// test, H8 §7/§9).
func TestSeededUserScansOnTicker(t *testing.T) {
	srv := fakeOpenCloud(t, propfindOnePhoto)
	pcfg := photosCfg()
	pcfg.Users = []config.AppUser{{User: "admin", Token: "app-token"}}
	m, err := New(testConfig(config.Common{OpenCloudURL: srv.URL, PhotosDataDir: t.TempDir()}, pcfg), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.sched.every = 30 * time.Millisecond // injectable ticker
	m.sched.sweep = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	waitFor(t, 5*time.Second, func() bool {
		stats, err := m.st.Stats(context.Background(), "user-1")
		return err == nil && stats["assets"].(int64) == 1
	}, "the seeded user did not scan on the ticker")

	// the seeded session has its space resolved and is not purged
	sess := m.reg.get("user-1")
	if sess == nil || !sess.seeded || sess.webdavURL == "" {
		t.Fatalf("seeded session: %+v", sess)
	}
	if n := m.reg.purge(time.Now()); n != 0 {
		t.Fatalf("seeded sessions are not purged: %d", n)
	}
}

// TestScanExpiredTokenPostpones: a scan that receives a 401 marks the Bearer
// session as expired and does NOT count as lastScan (it will resume on the next
// user activity, H8 §7), the index stays intact.
func TestScanExpiredTokenPostpones(t *testing.T) {
	srv := fakeOpenCloud(t, func(path string) (int, string) {
		return http.StatusUnauthorized, ""
	})
	m, err := New(testConfig(config.Common{OpenCloudURL: srv.URL, PhotosDataDir: t.TempDir()},
		config.PhotosConfig{ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Bearer session of the user (as if it had made a request)
	m.reg.Touch(context.Background(), &auth.User{ID: "user-1"}, auth.Credential{Bearer: "caducado"})

	m.sched.scanOne(context.Background(), "user-1")

	sess := m.reg.get("user-1")
	if sess == nil || !sess.expired || sess.webdavURL != "" {
		t.Fatalf("session after 401: %+v, want expired and an invalidated space", sess)
	}
	m.sched.mu.Lock()
	_, scanned := m.sched.lastScan["user-1"]
	m.sched.mu.Unlock()
	if scanned {
		t.Fatal("a scan with an expired token does NOT count as lastScan")
	}
}

// TestSchedulerDedupeAndSemaphore: the queue dedupes by owner and the global
// semaphore limits to 2 concurrent scans (H8 §7).
func TestSchedulerDedupeAndSemaphore(t *testing.T) {
	var inFlight atomic.Int32
	var maxSeen atomic.Int32
	block := make(chan struct{})
	srv := fakeOpenCloud(t, func(path string) (int, string) {
		n := inFlight.Add(1)
		for {
			if m := maxSeen.Load(); n <= m || maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		<-block // scans blocked until the test releases them
		inFlight.Add(-1)
		return http.StatusNotFound, ""
	})
	m, err := New(testConfig(config.Common{OpenCloudURL: srv.URL, PhotosDataDir: t.TempDir()},
		config.PhotosConfig{ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cap(m.sched.maxPar) != 2 {
		t.Fatalf("global semaphore: cap=%d, want 2", cap(m.sched.maxPar))
	}
	// Bearer sessions of 4 users
	for i := 0; i < 4; i++ {
		owner := fmt.Sprintf("user-%d", i)
		m.reg.Touch(context.Background(), &auth.User{ID: owner}, auth.Credential{Bearer: "tok"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.sched.run(ctx)
	// dedupe: enqueuing the same owner twice counts it only once
	m.sched.enqueue("user-0")
	m.sched.enqueue("user-0")
	m.sched.enqueue("user-1")
	m.sched.enqueue("user-2")
	m.sched.enqueue("user-3")

	waitFor(t, 5*time.Second, func() bool { return inFlight.Load() == 2 }, "there are no 2 scans in flight")
	if got := maxSeen.Load(); got > 2 {
		t.Fatalf("concurrent scans = %d, want <= 2", got)
	}
	m.sched.mu.Lock()
	queuedCount := len(m.sched.queued)
	scanningCount := len(m.sched.scanning)
	m.sched.mu.Unlock()
	if scanningCount != 2 {
		t.Fatalf("scanning=%d, want 2", scanningCount)
	}
	// user-0 was queued only once: queued holds at most the 2 remaining ones
	if queuedCount > 2 {
		t.Fatalf("broken dedupe: queued=%d", queuedCount)
	}
	close(block)
	waitFor(t, 5*time.Second, func() bool { return inFlight.Load() == 0 }, "the scans did not finish")
	if got := maxSeen.Load(); got > 2 {
		t.Fatalf("concurrent scans = %d, want <= 2", got)
	}
}

// TestPurgeBearerSessions: Bearer sessions without activity for more than 24h are
// purged, Basic and seeded ones are not (H8 §2).
func TestPurgeBearerSessions(t *testing.T) {
	m, err := New(testConfig(config.Common{OpenCloudURL: "http://x", PhotosDataDir: t.TempDir()},
		config.PhotosConfig{ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.reg.Touch(ctx, &auth.User{ID: "bearer-viejo"}, auth.Credential{Bearer: "t1"})
	m.reg.Touch(ctx, &auth.User{ID: "bearer-nuevo"}, auth.Credential{Bearer: "t2"})
	m.reg.Touch(ctx, &auth.User{ID: "basico"}, auth.Credential{Username: "admin", Password: "app-token"})

	// age one Bearer session and the Basic one (which is NOT purged)
	m.reg.mu.Lock()
	m.reg.sessions["bearer-viejo"].lastSeen = time.Now().Add(-25 * time.Hour)
	m.reg.sessions["basico"].lastSeen = time.Now().Add(-72 * time.Hour)
	m.reg.mu.Unlock()

	if n := m.reg.purge(time.Now()); n != 1 {
		t.Fatalf("purged = %d, want 1", n)
	}
	if m.reg.get("bearer-viejo") != nil {
		t.Fatal("bearer-viejo should have been purged")
	}
	if m.reg.get("bearer-nuevo") == nil || m.reg.get("basico") == nil {
		t.Fatal("bearer-nuevo and the Basic session must survive")
	}
}

// TestBackfillWithMockedMeID: a memories.db from the single-tenant era is
// migrated (002) and its owner empty rows are adopted into the oc_id resolved
// against Graph with the app token (H8 §5.3).
func TestBackfillWithMockedMeID(t *testing.T) {
	dataDir := t.TempDir()
	// live pre-H8 database with the single-tenant schema, one asset and one album
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "memories.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TABLE assets (id INTEGER PRIMARY KEY, path TEXT NOT NULL UNIQUE, etag TEXT NOT NULL, filename TEXT NOT NULL, media_type TEXT NOT NULL DEFAULT 'image', taken_at INTEGER NOT NULL, exif_done INTEGER NOT NULL DEFAULT 0, width INTEGER, height INTEGER, camera TEXT, lens TEXT, iso INTEGER, aperture TEXT, shutter TEXT, focal TEXT, lat REAL, lon REAL, size INTEGER NOT NULL DEFAULT 0, is_favorite INTEGER NOT NULL DEFAULT 0, deleted_at INTEGER, created_at INTEGER NOT NULL DEFAULT (unixepoch()), is_archived INTEGER NOT NULL DEFAULT 0, phash TEXT);
CREATE TABLE scan_state (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE albums (id INTEGER PRIMARY KEY, name TEXT NOT NULL, created_at INTEGER NOT NULL DEFAULT (unixepoch()));
CREATE TABLE album_assets (album_id INTEGER NOT NULL REFERENCES albums(id) ON DELETE CASCADE, asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE, added_at INTEGER NOT NULL DEFAULT (unixepoch()), PRIMARY KEY (album_id, asset_id));
CREATE TABLE asset_tags (asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE, tag TEXT NOT NULL, PRIMARY KEY (asset_id, tag));
CREATE TABLE geocode (lat_key REAL NOT NULL, lon_key REAL NOT NULL, name TEXT NOT NULL, ts INTEGER NOT NULL, PRIMARY KEY (lat_key, lon_key));
INSERT INTO assets (path, etag, filename, taken_at) VALUES ('/dav/x/a.jpg','e1','a.jpg',1700000000);
INSERT INTO albums (name) VALUES ('Vacaciones');
`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	srv := fakeOpenCloud(t, nil) // fake Graph: /me -> user-1
	m, err := New(testConfig(config.Common{OpenCloudURL: srv.URL, PhotosDataDir: dataDir}, photosCfg()), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// the backfill runs at the start of Run: the orphans end up adopted
	waitFor(t, 5*time.Second, func() bool {
		oa, ob, err := m.st.OrphanCounts(context.Background())
		return err == nil && oa == 0 && ob == 0
	}, "the backfill did not adopt the single-tenant rows")
	stats, err := m.st.Stats(context.Background(), "user-1")
	if err != nil || stats["assets"].(int64) != 1 {
		t.Fatalf("asset adopted by user-1: %v %v", stats, err)
	}
	cancel()
	<-done
}

// TestBackfillWithoutLegacyUserWarns: without OCAPPS_PHOTOS_USER/APP_TOKEN the
// module starts anyway and leaves the orphan rows (WARN in the logs, H8 §5.3).
func TestBackfillWithoutLegacyUserWarns(t *testing.T) {
	m, err := New(testConfig(config.Common{OpenCloudURL: "http://127.0.0.1:1", PhotosDataDir: t.TempDir()},
		config.PhotosConfig{ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := m.st.UpsertByETag(ctx, "", "/dav/x/vieja.jpg", "e1", "vieja.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	m.backfill(ctx) // no credentials: WARN and go on
	oa, _, err := m.st.OrphanCounts(ctx)
	if err != nil || oa != 1 {
		t.Fatalf("the orphan rows remain without legacy credentials: %d %v", oa, err)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}
