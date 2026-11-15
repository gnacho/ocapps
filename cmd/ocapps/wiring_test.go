package main

// H5 wiring integration tests (SPEC §10.3): startup with one module forced to
// fail, the route registration table of §4.1 against the real mux and the two
// OCS modes end-to-end against the integrated mux.

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

// localCfg: minimal config in local mode (news with bootstrap admin, notes
// enabled, photos disabled) on a temporary data dir.
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

// fakeOpenCloud simulates Graph (/me, /me/drives) to start the three modules in
// opencloud mode: it admits Basic nacho:app-token and Bearer oc-token.
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

// opencloudCfg: the three modules enabled against the fake OpenCloud.
func opencloudCfg(t *testing.T, ocURL string) *config.Config {
	t.Helper()
	cfg := localCfg(t)
	cfg.AuthMode = "opencloud"
	cfg.OpenCloudURL = ocURL
	cfg.News.AuthUser, cfg.News.AuthPass = "", "" // opencloud mode: no bootstrap
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

// TestForcedFailingModuleIntegration (SPEC §10.3): news forces its constructor
// to fail (local mode with no users and no bootstrap credentials) -> its
// namespace serves 503, the other modules respond normally (401 with auth
// required, NEVER 503), /readyz -> 503, /healthz -> 200 with news failed and the
// process stays alive.
func TestForcedFailingModuleIntegration(t *testing.T) {
	cfg := localCfg(t)
	cfg.News.AuthUser, cfg.News.AuthPass = "", "" // bootstrap impossible -> New fails

	a := wire(cfg, discardLog())
	srv := httptest.NewServer(a.mux)
	t.Cleanup(srv.Close)

	// news namespace -> 503 {"error":"module news unavailable"}
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

	// notes keeps serving: 401 with auth required (NOT 503 nor 404)
	code, body := get(t, srv.Client(), srv.URL+"/index.php/apps/notes/api/v1/notes")
	if code != http.StatusUnauthorized {
		t.Fatalf("notes API: status = %d, want 401 (%s)", code, body)
	}
	code, _ = get(t, srv.Client(), srv.URL+"/ocs/v2.php/cloud/capabilities")
	if code != http.StatusOK {
		t.Fatalf("notes OCS capabilities: status = %d, want 200", code)
	}

	// /healthz -> 200 always, with news failed and notes ok
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

	// /readyz -> 503 (one enabled module failed), same body
	code, body = get(t, srv.Client(), srv.URL+"/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("readyz: status = %d, want 503 (%s)", code, body)
	}
	if !strings.Contains(body, `"news":"failed:`) {
		t.Fatalf("readyz body without news failed: %s", body)
	}

	// process alive: healthz keeps responding after all the failures
	if code, _ = get(t, srv.Client(), srv.URL+"/healthz"); code != http.StatusOK {
		t.Fatalf("process not alive: healthz = %d", code)
	}
}

// TestRoutesTableIntegration (SPEC §10.3/§4.1): all the public paths of the
// routing table have a pattern in the real mux with the THREE modules enabled,
// no namespace steps on another one (a duplicate pattern would panic in the
// wire call itself) and /ocs/v2.php/cloud/user is unique (notes serves it:
// XML 200 without auth, something the removed news stub never did).
func TestRoutesTableIntegration(t *testing.T) {
	oc := fakeOpenCloud(t)
	a := wire(opencloudCfg(t, oc.URL), discardLog())
	srv := httptest.NewServer(a.mux)
	t.Cleanup(srv.Close)

	paths := []struct {
		method, path string
	}{
		// news: News API v1.3 contract + public exceptions + own API
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
		// notes: Notes API + OCS contract
		{"GET", "/index.php/apps/notes/api/v1/notes"},
		{"GET", "/index.php/apps/notes/api/v1/settings"},
		{"GET", "/index.php/apps/notes/api/v1.4/attachment/1"},
		{"GET", "/ocs/v2.php/cloud/capabilities"},
		{"GET", "/ocs/v2.php/cloud/user"},
		// photos: namespace without strip (§4.3)
		{"GET", "/ocphotos-api/api/stats"},
		{"GET", "/ocphotos-api/api/assets"},
		// common
		{"GET", "/healthz"},
		{"GET", "/readyz"},
	}
	for _, p := range paths {
		req := httptest.NewRequest(p.method, p.path, nil)
		if _, pattern := a.mux.Handler(req); pattern == "" {
			t.Errorf("%s %s: no registered pattern (would be 404)", p.method, p.path)
		}
	}

	// Uniqueness of /ocs/v2.php/cloud/user: wire() did not panic (duplicate
	// pattern) and the handler that responds is the NOTES one (XML 200 without
	// auth, the removed news stub always responded JSON and 401 without auth).
	code, body := get(t, srv.Client(), srv.URL+"/ocs/v2.php/cloud/user")
	if code != http.StatusOK || !strings.Contains(body, "<id>User</id>") {
		t.Fatalf("ocs user by default: %d %s (want 200 XML notes)", code, body)
	}

	// readyz green with the three healthy modules
	code, body = get(t, srv.Client(), srv.URL+"/readyz")
	if code != http.StatusOK {
		t.Fatalf("readyz with healthy modules: %d (%s)", code, body)
	}
	for _, want := range []string{`"news":"ok"`, `"notes":"ok"`, `"photos":"ok"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("readyz body without %s: %s", want, body)
		}
	}
}

// TestOCSTwoModesIntegration (SPEC §4.2/§10.3): /ocs/v2.php/cloud/user
// end-to-end against the integrated mux, in both modes.
func TestOCSTwoModesIntegration(t *testing.T) {
	oc := fakeOpenCloud(t)
	a := wire(opencloudCfg(t, oc.URL), discardLog())
	srv := httptest.NewServer(a.mux)
	t.Cleanup(srv.Close)

	// JSON with auth (news-android: ?format=json) -> OCS envelope with
	// id=username and displayname/display-name.
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
		t.Fatalf("JSON with auth: %d %s", resp.StatusCode, body)
	}
	for _, want := range []string{`"ocs"`, `"statuscode":200`, `"id":"nacho"`,
		`"displayname":"Nacho"`, `"display-name":"Nacho"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("JSON with auth: missing %s in %s", want, body)
		}
	}

	// JSON through Accept (OCS clients with OCS-APIRequest) -> JSON as well.
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
		t.Fatalf("JSON through Accept: %d %q", respA.StatusCode, respA.Header.Get("Content-Type"))
	}

	// JSON without auth -> 401.
	code, body := get(t, srv.Client(), srv.URL+"/ocs/v2.php/cloud/user?format=json")
	if code != http.StatusUnauthorized || !strings.Contains(body, `"statuscode":401`) {
		t.Fatalf("JSON without auth: %d %s (want 401)", code, body)
	}

	// XML by default without auth -> 200 "User" (Iotas mode kept).
	code, body = get(t, srv.Client(), srv.URL+"/ocs/v2.php/cloud/user")
	if code != http.StatusOK || !strings.Contains(body, "<id>User</id>") {
		t.Fatalf("XML without auth: %d %s (want 200 <id>User</id>)", code, body)
	}

	// XML with auth -> 200 with the display name.
	reqX, _ := http.NewRequest("GET", srv.URL+"/ocs/v2.php/cloud/user", nil)
	reqX.SetBasicAuth("nacho", "app-token")
	respX, err := srv.Client().Do(reqX)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = respX.Body.Close() }()
	bx, _ := io.ReadAll(respX.Body)
	if respX.StatusCode != http.StatusOK || !strings.Contains(string(bx), "<id>Nacho</id>") {
		t.Fatalf("XML with auth: %d %s", respX.StatusCode, bx)
	}
}

// ---------------------------------------------------------------------------
// Supervisor (unit, SPEC §4.5): panic or error in Run -> module failed, the
// process (and the rest of the goroutines) goes on.
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

func TestSupervisorPanicMarksFailed(t *testing.T) {
	e := &entry{name: "boom", enabled: true, mod: stubModule{"boom",
		func(context.Context) error { panic("boom") }}}
	healthy := &entry{name: "healthy", enabled: true, mod: stubModule{"healthy",
		func(ctx context.Context) error { <-ctx.Done(); return nil }}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wgPanic, wgSano sync.WaitGroup
	wgPanic.Add(1)
	wgSano.Add(1)
	go e.supervise(ctx, &wgPanic, discardLog())
	go healthy.supervise(ctx, &wgSano, discardLog())
	wgPanic.Wait() // the panic must not escape supervise

	if st := e.status(); !strings.HasPrefix(st, "failed:") || !strings.Contains(st, "boom") {
		t.Fatalf("status after panic = %q, want failed: ... boom", st)
	}
	if st := healthy.status(); st != "ok" {
		t.Fatalf("healthy module affected by someone else panic: %q", st)
	}
	cancel()
	wgSano.Wait() // the healthy one ends on cancel
}

func TestSupervisorRunErrorMarksFailed(t *testing.T) {
	e := &entry{name: "failing", enabled: true, mod: stubModule{"failing",
		func(context.Context) error { return context.DeadlineExceeded }}}
	var wg sync.WaitGroup
	wg.Add(1)
	go e.supervise(context.Background(), &wg, discardLog())
	wg.Wait()
	if st := e.status(); !strings.HasPrefix(st, "failed:") {
		t.Fatalf("status after a Run error = %q, want failed:", st)
	}
}

// TestHealthzSiempre200: even with ALL the modules failed, healthz is 200
// (liveness) and readyz 503.
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
		t.Fatalf("healthz with everything failed: %d, want 200", code)
	}
	if code, _ := get(t, srv.Client(), srv.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with everything failed: %d, want 503", code)
	}
}

// smoke of photos.PublicPrefix (import used in the table): the namespace of a
// failed photos module is exactly its public prefix.
func TestFailedNamespacePhotos(t *testing.T) {
	ns := failedNamespaces["photos"]
	if len(ns) != 1 || ns[0] != photos.PublicPrefix+"/" {
		t.Fatalf("failed namespace of photos = %v", ns)
	}
}
