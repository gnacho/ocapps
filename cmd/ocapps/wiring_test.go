package main

// Tests de integración del wiring H5 (SPEC §10.3): arranque con un módulo
// forzado a fallar, tabla de registro de rutas del §4.1 contra el mux real y
// los dos modos OCS end-to-end contra el mux integrado.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/config"
	newsapi "github.com/gnacho/ocapps/internal/news/api"
	"github.com/gnacho/ocapps/internal/photos"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// localCfg: config mínima en modo local (news con admin bootstrap, notes
// enabled, photos disabled) sobre un data dir temporal.
func localCfg(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	return &config.Config{
		Common: config.Common{
			ListenAddr:    "127.0.0.1:0",
			DataDir:       dir,
			LogLevel:      "error",
			AuthMode:      "local",
			NewsEnabled:   true,
			NotesEnabled:  true,
			PhotosEnabled: false,
			NewsDataDir:   filepath.Join(dir, "news"),
			NotesDataDir:  filepath.Join(dir, "notes"),
			PhotosDataDir: filepath.Join(dir, "photos"),
		},
		News: config.NewsConfig{
			FetchTimeout:  5 * time.Second,
			FeedInterval:  time.Minute,
			MaxGap:        10 * time.Minute,
			RetentionDays: 90,
			NtfyURL:       "https://ntfy.sh",
			AuthUser:      "admin",
			AuthPass:      "pass1234",
		},
	}
}

// fakeOpenCloud simula Graph (/me, /me/drives) para arrancar los tres
// módulos en modo opencloud: admite Basic nacho:app-token y Bearer oc-token.
func fakeOpenCloud(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authed := r.Header.Get("Authorization") == "Bearer oc-token"
		if u, p, ok := r.BasicAuth(); ok && u == "nacho" && p == "app-token" {
			authed = true
		}
		if !authed {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/graph/v1.0/me":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id": "uuid-nacho", "displayName": "Nacho",
				"onPremisesSamAccountName": "nacho",
			})
		case "/graph/v1.0/me/drives":
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{
				"id": "d1", "name": "Personal", "driveType": "personal",
				"root": map[string]any{"webDavUrl": srv.URL + "/dav/spaces/1"},
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// opencloudCfg: los tres módulos enabled contra el OpenCloud falso.
func opencloudCfg(t *testing.T, ocURL string) *config.Config {
	t.Helper()
	cfg := localCfg(t)
	cfg.AuthMode = "opencloud"
	cfg.OpenCloudURL = ocURL
	cfg.News.AuthUser, cfg.News.AuthPass = "", "" // modo opencloud: sin bootstrap
	cfg.PhotosEnabled = true
	cfg.Photos = config.PhotosConfig{
		User: "nacho", AppToken: "app-token", Token: "photos-token",
		ScanRoot: "Fotos", ScanEvery: time.Hour,
	}
	return cfg
}

func get(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// TestIntegracionModuloForzadoAFallar (SPEC §10.3): news fuerza el fallo de
// su constructor (modo local sin usuarios ni credenciales de bootstrap) → su
// namespace sirve 503, los demás módulos responden normal (401 con auth
// requerida, NUNCA 503), /readyz → 503, /healthz → 200 con news failed y el
// proceso sigue vivo.
func TestIntegracionModuloForzadoAFallar(t *testing.T) {
	cfg := localCfg(t)
	cfg.News.AuthUser, cfg.News.AuthPass = "", "" // bootstrap imposible → New falla

	a := wire(cfg, discardLog())
	srv := httptest.NewServer(a.mux)
	t.Cleanup(srv.Close)

	// namespace de news → 503 {"error":"module news unavailable"}
	for _, path := range []string{
		newsapi.Base + "/version",
		newsapi.Base + "/items",
		"/api/me",
		"/api/me/settings",
		"/api/users",
		"/api/users/1",
	} {
		code, body := get(t, srv.Client(), srv.URL+path)
		if code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503 (%s)", path, code, body)
		}
		if !strings.Contains(body, `"error":"module news unavailable"`) {
			t.Fatalf("%s: body = %s", path, body)
		}
	}

	// notes sigue sirviendo: 401 con auth requerida (NO 503 ni 404)
	code, body := get(t, srv.Client(), srv.URL+"/index.php/apps/notes/api/v1/notes")
	if code != http.StatusUnauthorized {
		t.Fatalf("notes API: status = %d, want 401 (%s)", code, body)
	}
	code, _ = get(t, srv.Client(), srv.URL+"/ocs/v2.php/cloud/capabilities")
	if code != http.StatusOK {
		t.Fatalf("notes OCS capabilities: status = %d, want 200", code)
	}

	// /healthz → 200 siempre, con news failed y notes ok
	code, body = get(t, srv.Client(), srv.URL+"/healthz")
	if code != http.StatusOK {
		t.Fatalf("healthz: status = %d, want 200", code)
	}
	var hz struct {
		Version string            `json:"version"`
		Modules map[string]string `json:"modules"`
	}
	if err := json.Unmarshal([]byte(body), &hz); err != nil {
		t.Fatalf("healthz JSON: %v (%s)", err, body)
	}
	if !strings.HasPrefix(hz.Modules["news"], "failed:") {
		t.Fatalf("healthz modules.news = %q, want failed:...", hz.Modules["news"])
	}
	if hz.Modules["notes"] != "ok" {
		t.Fatalf("healthz modules.notes = %q, want ok", hz.Modules["notes"])
	}
	if hz.Modules["photos"] != "disabled" {
		t.Fatalf("healthz modules.photos = %q, want disabled", hz.Modules["photos"])
	}

	// /readyz → 503 (un módulo enabled failed), mismo body
	code, body = get(t, srv.Client(), srv.URL+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz: status = %d, want 503 (%s)", code, body)
	}
	if !strings.Contains(body, `"news":"failed:`) {
		t.Fatalf("readyz body sin news failed: %s", body)
	}

	// proceso vivo: healthz sigue respondiendo tras todos los fallos
	if code, _ = get(t, srv.Client(), srv.URL+"/healthz"); code != http.StatusOK {
		t.Fatalf("proceso no vivo: healthz = %d", code)
	}
}

// TestIntegracionTablaDeRutas (SPEC §10.3/§4.1): todos los paths públicos de
// la tabla de enrutado tienen patrón en el mux real con los TRES módulos
// enabled; ningún namespace pisa a otro (un patrón duplicado haría panic en
// la propia llamada a wire) y /ocs/v2.php/cloud/user es único (lo sirve
// notes: XML 200 sin auth, algo que el stub eliminado de news nunca hacía).
func TestIntegracionTablaDeRutas(t *testing.T) {
	oc := fakeOpenCloud(t)
	a := wire(opencloudCfg(t, oc.URL), discardLog())
	srv := httptest.NewServer(a.mux)
	t.Cleanup(srv.Close)

	paths := []struct {
		method, path string
	}{
		// news: contrato News API v1.3 + excepciones públicas + API propia
		{"GET", "/index.php/apps/news/api/v1-3/version"},
		{"GET", "/index.php/apps/news/api/v1-3/folders"},
		{"GET", "/index.php/apps/news/api/v1-3/feeds"},
		{"GET", "/index.php/apps/news/api/v1-3/items"},
		{"GET", "/index.php/apps/news/api/v1-3/img"},
		{"GET", "/index.php/apps/news/api/v1-3/favicon/abc"},
		{"GET", "/index.php/apps/news/api/v1-3/share/token"},
		{"GET", "/index.php/apps/news/api/v1-3/websub/callback/1"},
		{"POST", "/index.php/apps/news/api/v1-3/websub/callback/1"},
		{"GET", "/api/me"},
		{"PUT", "/api/me"},
		{"PUT", "/api/me/password"},
		{"GET", "/api/me/settings"},
		{"PUT", "/api/me/settings"},
		{"GET", "/api/me/rules"},
		{"PUT", "/api/me/rules"},
		{"GET", "/api/users"},
		{"POST", "/api/users"},
		{"PUT", "/api/users/1"},
		{"DELETE", "/api/users/1"},
		// notes: contrato Notes API + OCS
		{"GET", "/index.php/apps/notes/api/v1/notes"},
		{"GET", "/index.php/apps/notes/api/v1/settings"},
		{"GET", "/index.php/apps/notes/api/v1.4/attachment/1"},
		{"GET", "/ocs/v2.php/cloud/capabilities"},
		{"GET", "/ocs/v2.php/cloud/user"},
		// photos: namespace sin strip (§4.3)
		{"GET", "/ocphotos-api/api/stats"},
		{"GET", "/ocphotos-api/api/assets"},
		// common
		{"GET", "/healthz"},
		{"GET", "/readyz"},
	}
	for _, p := range paths {
		req := httptest.NewRequest(p.method, p.path, nil)
		if _, pattern := a.mux.Handler(req); pattern == "" {
			t.Errorf("%s %s: sin patrón registrado (sería 404)", p.method, p.path)
		}
	}

	// Unicidad de /ocs/v2.php/cloud/user: wire() no ha panado (patrón
	// duplicado) y el handler que responde es el de NOTES (XML 200 sin auth;
	// el stub de news eliminado respondía siempre JSON y 401 sin auth).
	code, body := get(t, srv.Client(), srv.URL+"/ocs/v2.php/cloud/user")
	if code != http.StatusOK || !strings.Contains(body, "<id>User</id>") {
		t.Fatalf("ocs user por defecto: %d %s (want 200 XML notes)", code, body)
	}

	// readyz verde con los tres módulos sanos
	code, body = get(t, srv.Client(), srv.URL+"/readyz")
	if code != http.StatusOK {
		t.Fatalf("readyz con módulos sanos: %d (%s)", code, body)
	}
	for _, want := range []string{`"news":"ok"`, `"notes":"ok"`, `"photos":"ok"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("readyz body sin %s: %s", want, body)
		}
	}
}

// TestIntegracionOCSDosModos (SPEC §4.2/§10.3): /ocs/v2.php/cloud/user
// end-to-end contra el mux integrado, en ambos modos.
func TestIntegracionOCSDosModos(t *testing.T) {
	oc := fakeOpenCloud(t)
	a := wire(opencloudCfg(t, oc.URL), discardLog())
	srv := httptest.NewServer(a.mux)
	t.Cleanup(srv.Close)

	// JSON con auth (news-android: ?format=json) → envelope OCS con
	// id=username y displayname/display-name.
	req, _ := http.NewRequest("GET", srv.URL+"/ocs/v2.php/cloud/user?format=json", nil)
	req.SetBasicAuth("nacho", "app-token")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("JSON con auth: %d %s", resp.StatusCode, body)
	}
	for _, want := range []string{`"ocs"`, `"statuscode":200`, `"id":"nacho"`,
		`"displayname":"Nacho"`, `"display-name":"Nacho"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("JSON con auth: falta %s en %s", want, body)
		}
	}

	// JSON vía Accept (clientes OCS con OCS-APIRequest) → también JSON.
	reqA, _ := http.NewRequest("GET", srv.URL+"/ocs/v2.php/cloud/user", nil)
	reqA.SetBasicAuth("nacho", "app-token")
	reqA.Header.Set("Accept", "application/json")
	reqA.Header.Set("OCS-APIRequest", "true")
	respA, err := srv.Client().Do(reqA)
	if err != nil {
		t.Fatal(err)
	}
	_ = respA.Body.Close()
	if respA.StatusCode != http.StatusOK ||
		!strings.Contains(respA.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("JSON vía Accept: %d %q", respA.StatusCode, respA.Header.Get("Content-Type"))
	}

	// JSON sin auth → 401.
	code, body := get(t, srv.Client(), srv.URL+"/ocs/v2.php/cloud/user?format=json")
	if code != http.StatusUnauthorized || !strings.Contains(body, `"statuscode":401`) {
		t.Fatalf("JSON sin auth: %d %s (want 401)", code, body)
	}

	// XML por defecto sin auth → 200 "User" (modo Iotas conservado).
	code, body = get(t, srv.Client(), srv.URL+"/ocs/v2.php/cloud/user")
	if code != http.StatusOK || !strings.Contains(body, "<id>User</id>") {
		t.Fatalf("XML sin auth: %d %s (want 200 <id>User</id>)", code, body)
	}

	// XML con auth → 200 con el display name.
	reqX, _ := http.NewRequest("GET", srv.URL+"/ocs/v2.php/cloud/user", nil)
	reqX.SetBasicAuth("nacho", "app-token")
	respX, err := srv.Client().Do(reqX)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = respX.Body.Close() }()
	bx, _ := io.ReadAll(respX.Body)
	if respX.StatusCode != http.StatusOK || !strings.Contains(string(bx), "<id>Nacho</id>") {
		t.Fatalf("XML con auth: %d %s", respX.StatusCode, bx)
	}
}

// ---------------------------------------------------------------------------
// Supervisor (unitario, SPEC §4.5): pánico o error en Run → módulo failed,
// proceso (y el resto de goroutines) sigue.
// ---------------------------------------------------------------------------

type stubModule struct {
	name string
	run  func(ctx context.Context) error
}

func (s stubModule) Name() string                  { return s.name }
func (s stubModule) Enabled() bool                 { return true }
func (s stubModule) Register(_ *http.ServeMux)     {}
func (s stubModule) Run(ctx context.Context) error { return s.run(ctx) }
func (s stubModule) Healthy() error                { return nil }

func TestSupervisorPanicoMarcaFailed(t *testing.T) {
	e := &entry{name: "boom", enabled: true, mod: stubModule{"boom",
		func(context.Context) error { panic("explota") }}}
	sano := &entry{name: "sano", enabled: true, mod: stubModule{"sano",
		func(ctx context.Context) error { <-ctx.Done(); return nil }}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wgPanic, wgSano sync.WaitGroup
	wgPanic.Add(1)
	wgSano.Add(1)
	go e.supervise(ctx, &wgPanic, discardLog())
	go sano.supervise(ctx, &wgSano, discardLog())
	wgPanic.Wait() // el pánico no debe escapar de supervise

	if st := e.status(); !strings.HasPrefix(st, "failed:") || !strings.Contains(st, "explota") {
		t.Fatalf("status tras pánico = %q, want failed: ... explota", st)
	}
	if st := sano.status(); st != "ok" {
		t.Fatalf("módulo sano afectado por pánico ajeno: %q", st)
	}
	cancel()
	wgSano.Wait() // sano termina al cancelar
}

func TestSupervisorRunConErrorMarcaFailed(t *testing.T) {
	e := &entry{name: "fallo", enabled: true, mod: stubModule{"fallo",
		func(context.Context) error { return context.DeadlineExceeded }}}
	var wg sync.WaitGroup
	wg.Add(1)
	go e.supervise(context.Background(), &wg, discardLog())
	wg.Wait()
	if st := e.status(); !strings.HasPrefix(st, "failed:") {
		t.Fatalf("status tras error de Run = %q, want failed:", st)
	}
}

// TestHealthzSiempre200: incluso con TODOS los módulos failed, healthz es
// 200 (liveness) y readyz 503.
func TestHealthzSiempre200(t *testing.T) {
	a := &app{
		mux: http.NewServeMux(),
		modules: []*entry{
			{name: "news", enabled: true},
			{name: "notes", enabled: true},
			{name: "photos", enabled: false},
		},
	}
	for _, e := range a.modules {
		if e.enabled {
			e.setErr(context.Canceled)
			for _, ns := range failedNamespaces[e.name] {
				a.mux.Handle(ns, http.NotFoundHandler())
			}
		}
	}
	a.mux.HandleFunc("GET /healthz", a.handleHealthz)
	a.mux.HandleFunc("GET /readyz", a.handleReadyz)

	srv := httptest.NewServer(a.mux)
	t.Cleanup(srv.Close)
	if code, _ := get(t, srv.Client(), srv.URL+"/healthz"); code != http.StatusOK {
		t.Fatalf("healthz con todo failed: %d, want 200", code)
	}
	if code, _ := get(t, srv.Client(), srv.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz con todo failed: %d, want 503", code)
	}
}

// smoke de photos.PublicPrefix (import usado en la tabla): el namespace de
// photos failed es exactamente su prefijo público.
func TestFailedNamespacePhotos(t *testing.T) {
	ns := failedNamespaces["photos"]
	if len(ns) != 1 || ns[0] != photos.PublicPrefix+"/" {
		t.Fatalf("namespace failed de photos = %v", ns)
	}
}
