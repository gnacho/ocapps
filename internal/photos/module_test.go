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

// fakeOpenCloud simula Graph (/me, /me/drives) y WebDAV (PROPFIND) para el
// wiring multi-tenant. propfind decide la respuesta de cada path PROPFIND
// (nil -> 404 en todo).
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
			fmt.Fprint(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeOpenCloudReq es fakeOpenCloud con acceso al *http.Request (p. ej.
// para exigir Basic en los PROPFIND).
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
			fmt.Fprint(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const unaFotoXML = `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">
<d:response><d:href>/dav/spaces/1/Fotos/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
<d:response><d:href>/dav/spaces/1/Fotos/IMG.jpg</d:href><d:propstat><d:prop><d:resourcetype/><d:getetag>"e1"</d:getetag><d:getlastmodified>Mon, 02 Jan 2006 15:04:05 GMT</d:getlastmodified><d:getcontentlength>100</d:getcontentlength><d:getcontenttype>image/jpeg</d:getcontenttype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>
</d:multistatus>`

func propfindUnaFoto(path string) (int, string) {
	if path == "/dav/spaces/1/Fotos/" {
		return http.StatusMultiStatus, unaFotoXML
	}
	return http.StatusNotFound, ""
}

func photosCfg() config.PhotosConfig {
	return config.PhotosConfig{
		User: "admin", AppToken: "app-token", Token: "test-token",
		ScanRoot: "Fotos", ScanEvery: time.Hour,
	}
}

// testConfig envuelve las secciones en la config unificada completa que
// espera New (sin ModuleErr: config manual = config válida).
func testConfig(common config.Common, pcfg config.PhotosConfig) *config.Config {
	return &config.Config{Common: common, Photos: pcfg}
}

// TestNewSinCredencialesOK: OCAPPS_PHOTOS_USER/APP_TOKEN ya NO son
// obligatorios (H8): el módulo arranca sin ellos y no depende de OpenCloud.
func TestNewSinCredencialesOK(t *testing.T) {
	m, err := New(testConfig(config.Common{OpenCloudURL: "http://127.0.0.1:1", PhotosDataDir: t.TempDir()}, config.PhotosConfig{ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatalf("New sin credenciales no debe fallar (H8): %v", err)
	}
	if err := m.Healthy(); err != nil {
		t.Fatalf("Healthy = ping SQLite, sin OpenCloud (H8): %v", err)
	}
	// el namespace sirve las rutas reales desde el arranque (no 503 de init)
	mux := http.NewServeMux()
	m.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, PublicPrefix+"/api/stats", nil))
	if rr.Code == http.StatusServiceUnavailable {
		t.Fatal("el módulo ya no sirve 503 de init (H8): las rutas responden desde el arranque")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("stats sin auth -> %d, quiero 401", rr.Code)
	}
}

// TestNewTokenSinUserFalla: el Bearer estático DEPRECATED sin
// OCAPPS_PHOTOS_USER no tiene identidad -> error de config (H8 §6.2).
func TestNewTokenSinUserFalla(t *testing.T) {
	pcfg := config.PhotosConfig{Token: "test-token", ScanEvery: time.Hour}
	_, err := New(testConfig(config.Common{OpenCloudURL: "http://x", PhotosDataDir: t.TempDir()}, pcfg), slog.Default(), nil)
	if err == nil || !strings.Contains(err.Error(), "OCAPPS_PHOTOS_TOKEN") {
		t.Fatalf("Token sin User debe ser error de config: %v", err)
	}
}

// I1: un ModuleErr("photos") registrado por config.Load (p. ej.
// OCAPPS_PHOTOS_SCAN_EVERY=0s, que PARSEA bien pero es inválido) debe
// tumbar New — antes el módulo arrancaba "sano" y paniqueaba en
// time.NewTicker(≤0) tras el scan inicial.
func TestNewFalloConScanEveryInvalido(t *testing.T) {
	for _, raw := range []string{"0s", "-5m", "no-es-duracion"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("OCAPPS_AUTH_MODE", "local") // sin OPENCLOUD_URL obligatoria
			t.Setenv("OCAPPS_DATA_DIR", t.TempDir())
			t.Setenv("OCAPPS_PHOTOS_SCAN_EVERY", raw)
			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load: %v (SCAN_EVERY inválido degrada el módulo, no es fatal)", err)
			}
			if cfg.ModuleErr("photos") == nil {
				t.Fatalf("SCAN_EVERY=%q debería registrar ModuleErr(photos)", raw)
			}
			_, err = New(cfg, slog.Default(), nil)
			if err == nil {
				t.Fatalf("New con SCAN_EVERY=%q debería fallar (módulo failed en init, D3)", raw)
			}
		})
	}
}

// TestModuloSinInitContraOpenCloud: con OpenCloud CAÍDO el módulo arranca y
// está sano igual (H8 §7: no hay initBackend ni retry loop; los scans se
// posponen a la actividad del usuario).
func TestModuloSinInitContraOpenCloud(t *testing.T) {
	common := config.Common{OpenCloudURL: "http://127.0.0.1:1", PhotosDataDir: t.TempDir()}
	m, err := New(testConfig(common, photosCfg()), slog.Default(), nil)
	if err != nil {
		t.Fatalf("New con OpenCloud caído: %v", err)
	}
	if err := m.Healthy(); err != nil {
		t.Fatalf("Healthy con OpenCloud caído: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// el backfill no puede resolver el legacy owner (Graph caído): log y sigue
	if err := m.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestModuloOKContraGraphFalso(t *testing.T) {
	srv := fakeOpenCloud(t, propfindUnaFoto)
	dataDir := t.TempDir()
	common := config.Common{OpenCloudURL: srv.URL, PhotosDataDir: dataDir}
	m, err := New(testConfig(common, photosCfg()), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Healthy(); err != nil {
		t.Fatalf("Healthy: %v", err)
	}

	// Q3: mediasecret persistido con permisos 0600
	sec := filepath.Join(dataDir, "mediasecret")
	fi, err := os.Stat(sec)
	if err != nil {
		t.Fatalf("mediasecret no persistido: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mediasecret permisos = %o, quiero 600", fi.Mode().Perm())
	}

	// rutas reales bajo el prefijo con el token estático (mapea al owner
	// legacy resuelto contra el Graph falso)
	mux := http.NewServeMux()
	m.Register(mux)
	req := httptest.NewRequest(http.MethodGet, PublicPrefix+"/api/stats", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("stats -> %d (%s)", rr.Code, rr.Body.String())
	}

	// Run retorna al cancelar ctx
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
		t.Fatal("Run no retornó tras cancelar ctx")
	}
}

// TestTouchDisparaScanDeIndiceVacio: la primera actividad de un usuario (un
// request autenticado -> Touch) encola su scan y puebla SU índice (H8 §7).
func TestTouchDisparaScanDeIndiceVacio(t *testing.T) {
	srv := fakeOpenCloud(t, propfindUnaFoto)
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

	// request con Basic app-password -> valida contra el Graph falso -> Touch
	mux := http.NewServeMux()
	m.Register(mux)
	req := httptest.NewRequest(http.MethodGet, PublicPrefix+"/api/stats", nil)
	req.SetBasicAuth("admin", "app-token")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("stats -> %d", rr.Code)
	}

	// el scan disparado por la actividad indexa la foto para user-1
	waitFor(t, 5*time.Second, func() bool {
		stats, err := m.st.Stats(context.Background(), "user-1")
		return err == nil && stats["assets"].(int64) == 1
	}, "el scan por actividad no indexó la foto del usuario")
}

// TestUsuarioSembradoEscaneaEnTicker: un usuario de OCAPPS_PHOTOS_USERS se
// siembra al arrancar Run y el ticker ScanEvery encola su scan (intervalos
// inyectables para el test, H8 §7/§9).
func TestUsuarioSembradoEscaneaEnTicker(t *testing.T) {
	srv := fakeOpenCloud(t, propfindUnaFoto)
	pcfg := photosCfg()
	pcfg.Users = []config.AppUser{{User: "admin", Token: "app-token"}}
	m, err := New(testConfig(config.Common{OpenCloudURL: srv.URL, PhotosDataDir: t.TempDir()}, pcfg), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m.sched.every = 30 * time.Millisecond // ticker inyectable
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
	}, "el usuario sembrado no escaneó en el ticker")

	// la sesión sembrada tiene su espacio resuelto y no se purga
	sess := m.reg.get("user-1")
	if sess == nil || !sess.seeded || sess.webdavURL == "" {
		t.Fatalf("sesión sembrada: %+v", sess)
	}
	if n := m.reg.purge(time.Now()); n != 0 {
		t.Fatalf("las sesiones sembradas no se purgan: %d", n)
	}
}

// TestScanTokenCaducadoPospone: un scan que recibe 401 marca la sesión Bearer
// como caducada y NO cuenta como lastScan (se reanudará con la próxima
// actividad del usuario, H8 §7); el índice queda intacto.
func TestScanTokenCaducadoPospone(t *testing.T) {
	srv := fakeOpenCloud(t, func(path string) (int, string) {
		return http.StatusUnauthorized, ""
	})
	m, err := New(testConfig(config.Common{OpenCloudURL: srv.URL, PhotosDataDir: t.TempDir()},
		config.PhotosConfig{ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// sesión Bearer del usuario (como si hubiera hecho un request)
	m.reg.Touch(context.Background(), &auth.User{ID: "user-1"}, auth.Credential{Bearer: "caducado"})

	m.sched.scanOne(context.Background(), "user-1")

	sess := m.reg.get("user-1")
	if sess == nil || !sess.expired || sess.webdavURL != "" {
		t.Fatalf("sesión tras 401: %+v, quiero expired y espacio invalidado", sess)
	}
	m.sched.mu.Lock()
	_, scanned := m.sched.lastScan["user-1"]
	m.sched.mu.Unlock()
	if scanned {
		t.Fatal("un scan con token caducado NO cuenta como lastScan")
	}
}

// TestSchedulerDedupeYSemaforo: la cola deduplica por owner y el semáforo
// global limita a 2 scans concurrentes (H8 §7).
func TestSchedulerDedupeYSemaforo(t *testing.T) {
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
		<-block // scans bloqueados hasta que el test los suelte
		inFlight.Add(-1)
		return http.StatusNotFound, ""
	})
	m, err := New(testConfig(config.Common{OpenCloudURL: srv.URL, PhotosDataDir: t.TempDir()},
		config.PhotosConfig{ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cap(m.sched.maxPar) != 2 {
		t.Fatalf("semáforo global: cap=%d, quiero 2", cap(m.sched.maxPar))
	}
	// sesiones Bearer de 4 usuarios
	for i := 0; i < 4; i++ {
		owner := fmt.Sprintf("user-%d", i)
		m.reg.Touch(context.Background(), &auth.User{ID: owner}, auth.Credential{Bearer: "tok"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.sched.run(ctx)
	// dedupe: encolar dos veces el mismo owner solo lo cuenta una vez
	m.sched.enqueue("user-0")
	m.sched.enqueue("user-0")
	m.sched.enqueue("user-1")
	m.sched.enqueue("user-2")
	m.sched.enqueue("user-3")

	waitFor(t, 5*time.Second, func() bool { return inFlight.Load() == 2 }, "no hay 2 scans en vuelo")
	if got := maxSeen.Load(); got > 2 {
		t.Fatalf("scans concurrentes = %d, quiero <= 2", got)
	}
	m.sched.mu.Lock()
	enCola := len(m.sched.queued)
	escaneando := len(m.sched.scanning)
	m.sched.mu.Unlock()
	if escaneando != 2 {
		t.Fatalf("scanning=%d, quiero 2", escaneando)
	}
	// user-0 se encoló una sola vez: queued contiene como mucho a los 2 restantes
	if enCola > 2 {
		t.Fatalf("dedupe roto: queued=%d", enCola)
	}
	close(block)
	waitFor(t, 5*time.Second, func() bool { return inFlight.Load() == 0 }, "scans no terminaron")
	if got := maxSeen.Load(); got > 2 {
		t.Fatalf("scans concurrentes = %d, quiero <= 2", got)
	}
}

// TestPurgadoSesionesBearer: las sesiones Bearer sin actividad >24h se
// purgan; las Basic y las sembradas no (H8 §2).
func TestPurgadoSesionesBearer(t *testing.T) {
	m, err := New(testConfig(config.Common{OpenCloudURL: "http://x", PhotosDataDir: t.TempDir()},
		config.PhotosConfig{ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.reg.Touch(ctx, &auth.User{ID: "bearer-viejo"}, auth.Credential{Bearer: "t1"})
	m.reg.Touch(ctx, &auth.User{ID: "bearer-nuevo"}, auth.Credential{Bearer: "t2"})
	m.reg.Touch(ctx, &auth.User{ID: "basico"}, auth.Credential{Username: "admin", Password: "app-token"})

	// envejece una sesión Bearer y la Basic (que NO se purga)
	m.reg.mu.Lock()
	m.reg.sessions["bearer-viejo"].lastSeen = time.Now().Add(-25 * time.Hour)
	m.reg.sessions["basico"].lastSeen = time.Now().Add(-72 * time.Hour)
	m.reg.mu.Unlock()

	if n := m.reg.purge(time.Now()); n != 1 {
		t.Fatalf("purgadas = %d, quiero 1", n)
	}
	if m.reg.get("bearer-viejo") != nil {
		t.Fatal("bearer-viejo debió purgarse")
	}
	if m.reg.get("bearer-nuevo") == nil || m.reg.get("basico") == nil {
		t.Fatal("bearer-nuevo y la sesión Basic deben sobrevivir")
	}
}

// TestBackfillConMeIDMockeado: una memories.db de la era single-tenant se
// migra (002) y sus filas owner=” se adoptan al oc_id resuelto contra Graph
// con el app-token (H8 §5.3).
func TestBackfillConMeIDMockeado(t *testing.T) {
	dataDir := t.TempDir()
	// BD viva pre-H8 con esquema single-tenant y un asset + álbum
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
	db.Close()

	srv := fakeOpenCloud(t, nil) // Graph falso: /me -> user-1
	m, err := New(testConfig(config.Common{OpenCloudURL: srv.URL, PhotosDataDir: dataDir}, photosCfg()), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// el backfill corre al inicio de Run: los huérfanos quedan adoptados
	waitFor(t, 5*time.Second, func() bool {
		oa, ob, err := m.st.OrphanCounts(context.Background())
		return err == nil && oa == 0 && ob == 0
	}, "el backfill no adoptó las filas single-tenant")
	stats, err := m.st.Stats(context.Background(), "user-1")
	if err != nil || stats["assets"].(int64) != 1 {
		t.Fatalf("asset adoptado por user-1: %v %v", stats, err)
	}
	cancel()
	<-done
}

// TestBackfillSinUsuarioLegacyAvisa: sin OCAPPS_PHOTOS_USER/APP_TOKEN el
// módulo arranca igual y deja las filas huérfanas (WARN en logs, H8 §5.3).
func TestBackfillSinUsuarioLegacyAvisa(t *testing.T) {
	m, err := New(testConfig(config.Common{OpenCloudURL: "http://127.0.0.1:1", PhotosDataDir: t.TempDir()},
		config.PhotosConfig{ScanEvery: time.Hour}), slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := m.st.UpsertByETag(ctx, "", "/dav/x/vieja.jpg", "e1", "vieja.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	m.backfill(ctx) // sin credenciales: WARN y sigue
	oa, _, err := m.st.OrphanCounts(ctx)
	if err != nil || oa != 1 {
		t.Fatalf("las filas huérfanas permanecen sin credenciales legacy: %d %v", oa, err)
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
