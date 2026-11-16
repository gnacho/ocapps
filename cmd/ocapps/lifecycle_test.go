package main

// Lifecycle tests of the process itself (run, graceful shutdown, streams and
// the extension panel): the wiring is covered in wiring_test.go, here the
// pieces that only run when the binary starts.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// routePattern returns the pattern the mux would use for a request ("" when
// nothing matches).
func routePattern(a *app, method, path string) string {
	req := httptest.NewRequest(method, path, nil)
	_, pattern := a.mux.Handler(req)
	return pattern
}

// newRequest builds a request for the mux of a.
func newRequest(t *testing.T, method, path string) *http.Request {
	t.Helper()
	return httptest.NewRequest(method, path, nil)
}

// serve runs the mux of a and returns the recorder.
func serve(a *app, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.mux.ServeHTTP(rec, req)
	return rec
}

// freeAddr returns a loopback address that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close the probe listener: %v", err)
	}
	return addr
}

// setRunEnv prepares the environment of config.Load for run: every module
// disabled, so the test only exercises the process lifecycle.
func setRunEnv(t *testing.T, addr, dataDir string) {
	t.Helper()
	t.Setenv("OCAPPS_AUTH_MODE", "local")
	t.Setenv("OCAPPS_LISTEN_ADDR", addr)
	t.Setenv("OCAPPS_DATA_DIR", dataDir)
	t.Setenv("OCAPPS_LOG_LEVEL", "error")
	t.Setenv("OCAPPS_NEWS_ENABLED", "false")
	t.Setenv("OCAPPS_NOTES_ENABLED", "false")
	t.Setenv("OCAPPS_PHOTOS_ENABLED", "false")
	t.Setenv("OCAPPS_ADMIN_TOKEN", "")
}

// withNotifyContext replaces the signal hook and returns the cancel function
// of the context run will use, plus a restore for the hook.
func withNotifyContext(t *testing.T) (context.CancelFunc, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	previous := notifyContext
	notifyContext = func(context.Context, ...os.Signal) (context.Context, context.CancelFunc) {
		return ctx, func() {}
	}
	return cancel, func() { notifyContext = previous }
}

// waitForHealthz polls /healthz until it answers 200 and returns the body.
func waitForHealthz(t *testing.T, addr string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return string(body)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the server did not answer /healthz on %s", addr)
	return ""
}

func TestRunServesUntilTheSignalArrives(t *testing.T) {
	addr := freeAddr(t)
	setRunEnv(t, addr, t.TempDir())
	cancel, restore := withNotifyContext(t)
	defer restore()

	done := make(chan error, 1)
	go func() { done <- run() }()

	body := waitForHealthz(t, addr)
	var hz struct {
		Version string            `json:"version"`
		Modules map[string]string `json:"modules"`
	}
	if err := json.Unmarshal([]byte(body), &hz); err != nil {
		t.Fatalf("decode healthz: %v (%s)", err, body)
	}
	if hz.Version == "" {
		t.Fatalf("healthz without version: %s", body)
	}
	for name, status := range hz.Modules {
		if status != "disabled" {
			t.Fatalf("module %s = %q, want disabled", name, status)
		}
	}

	// readyz is green: a disabled module never degrades the process.
	resp, err := http.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz = %d, want 200 with every module disabled", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v, want nil after a normal shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after the shutdown signal")
	}
}

func TestRunFailsOnAnInvalidConfig(t *testing.T) {
	addr := freeAddr(t)
	setRunEnv(t, addr, t.TempDir())
	t.Setenv("OCAPPS_LOG_LEVEL", "chatty")

	err := run()
	if err == nil || !strings.Contains(err.Error(), "OCAPPS_LOG_LEVEL") {
		t.Fatalf("run = %v, want the config error", err)
	}
}

func TestRunReturnsTheListenError(t *testing.T) {
	addr := freeAddr(t)
	occupied, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("occupy the address: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	setRunEnv(t, addr, t.TempDir())
	cancel, restore := withNotifyContext(t)
	defer restore()
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run must return the listen error instead of nil")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return when the address was already in use")
	}
}

// streamModule is a module that keeps open streams, to see closeStreams run.
type streamModule struct {
	stubModule
	mu     sync.Mutex
	closed int
}

func (s *streamModule) CloseStreams() {
	s.mu.Lock()
	s.closed++
	s.mu.Unlock()
}

func (s *streamModule) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func TestCloseStreamsOnlyTouchesTheStreamingModules(t *testing.T) {
	streaming := &streamModule{stubModule: stubModule{name: "news"}}
	plain := stubModule{name: "notes"}
	a := &app{modules: []*entry{
		{name: "news", enabled: true, mod: streaming},
		{name: "notes", enabled: true, mod: plain},
		{name: "photos", enabled: false, mod: nil},
	}}

	a.closeStreams()

	if got := streaming.closeCount(); got != 1 {
		t.Fatalf("CloseStreams calls = %d, want 1", got)
	}
}

func TestEnvOr(t *testing.T) {
	if got := envOr("OCAPPS_TEST_ENV_OR_UNSET", "fallback"); got != "fallback" {
		t.Fatalf("envOr with no variable = %q, want the fallback", got)
	}
	t.Setenv("OCAPPS_TEST_ENV_OR_SET", "value")
	if got := envOr("OCAPPS_TEST_ENV_OR_SET", "fallback"); got != "value" {
		t.Fatalf("envOr with a variable = %q, want its value", got)
	}
}

// writeConf writes an extensions.conf with a single valid line.
func writeConf(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "extensions.conf")
	line := "notes|https://github.com/gnacho/ocnotes|notes-.*\\.tar\\.gz\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("write conf: %v", err)
	}
	return path
}

func TestWireExtensionsStaysOffWithoutToken(t *testing.T) {
	a := wire(localCfg(t), discardLog())
	if pattern := routePattern(a, http.MethodGet, "/ocapps-admin/"); pattern != "" {
		t.Fatalf("the panel is mounted without a token: %q", pattern)
	}
}

func TestWireExtensionsMountsThePanelWithAToken(t *testing.T) {
	t.Setenv("OCAPPS_ADMIN_TOKEN", "admin-token")
	t.Setenv("OCAPPS_EXT_CONF", writeConf(t))
	a := wire(localCfg(t), discardLog())
	if pattern := routePattern(a, http.MethodGet, "/ocapps-admin/"); pattern == "" {
		t.Fatal("the panel must be mounted when an admin token is configured")
	}

	// The panel HTML is public, the API requires the token.
	req := newRequest(t, http.MethodGet, "/ocapps-admin/")
	rec := serve(a, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("panel = %d, want 200 with the HTML document", rec.Code)
	}

	rec = serve(a, newRequest(t, http.MethodGet, "/ocapps-admin/api/extensions"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("API without token = %d, want 401", rec.Code)
	}
}

func TestWireExtensionsStaysOffWithABrokenConf(t *testing.T) {
	t.Setenv("OCAPPS_ADMIN_TOKEN", "admin-token")
	t.Setenv("OCAPPS_EXT_CONF", filepath.Join(t.TempDir(), "missing.conf"))
	a := wire(localCfg(t), discardLog())
	if pattern := routePattern(a, http.MethodGet, "/ocapps-admin/"); pattern != "" {
		t.Fatalf("the panel is mounted with an unreadable conf: %q", pattern)
	}
}

func TestWireNotesFailsOnAnUnusableDataDir(t *testing.T) {
	cfg := localCfg(t)
	// A data dir under a regular file: sqlite cannot create the database.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("prepare file: %v", err)
	}
	cfg.NotesDataDir = filepath.Join(file, "notes")

	e := (&app{cfg: cfg, log: discardLog()}).wireNotes(nil)
	if e.mod != nil {
		t.Fatal("notes must not start with an unusable data dir")
	}
	if err := e.healthy(); err == nil {
		t.Fatal("the entry must carry the init error")
	}
	if got := e.status(); !strings.HasPrefix(got, "failed:") {
		t.Fatalf("status = %q, want failed", got)
	}
}

func TestWirePhotosFailsOnAnUnusableDataDir(t *testing.T) {
	cfg := localCfg(t)
	cfg.PhotosEnabled = true
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("prepare file: %v", err)
	}
	cfg.PhotosDataDir = filepath.Join(file, "photos")

	e := (&app{cfg: cfg, log: discardLog()}).wirePhotos(nil)
	if e.mod != nil {
		t.Fatal("photos must not start with an unusable data dir")
	}
	if err := e.healthy(); err == nil {
		t.Fatal("the entry must carry the init error")
	}
}

// TestRunClosesTheEnabledModulesOnShutdown exercises the last part of the
// graceful shutdown with a real module (notes), which is an io.Closer.
func TestRunClosesTheEnabledModulesOnShutdown(t *testing.T) {
	addr := freeAddr(t)
	setRunEnv(t, addr, t.TempDir())
	t.Setenv("OCAPPS_NOTES_ENABLED", "true")
	cancel, restore := withNotifyContext(t)
	defer restore()

	done := make(chan error, 1)
	go func() { done <- run() }()

	body := waitForHealthz(t, addr)
	if !strings.Contains(body, `"notes":"ok"`) {
		t.Fatalf("healthz without the notes module: %s", body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v after closing the modules", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return with an enabled module")
	}
}
