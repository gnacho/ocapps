package notes

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/config"
)

// newTestModule builds the notes module on a temporary data dir. The Graph
// validator points to an unreachable server, so every credential is invalid
// (API routes must answer 401, never 404).
func newTestModule(t *testing.T) Module {
	t.Helper()
	cfg := &config.Config{
		Common: config.Common{
			NotesEnabled: true,
			NotesDataDir: t.TempDir(),
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	v := auth.NewGraphValidator("http://127.0.0.1:1", auth.MultiTenant(), log)
	m, err := New(cfg, log, v)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.(interface{ Close() error }).Close() })
	return m
}

// TestModuleRouteRegistration: every path of the Notes contract (SPEC §4.1)
// is registered on the shared mux and answers (401/403/200/204), never 404.
func TestModuleRouteRegistration(t *testing.T) {
	m := newTestModule(t)
	if m.Name() != "notes" {
		t.Fatalf("Name = %q, want notes", m.Name())
	}
	if !m.Enabled() {
		t.Fatal("Enabled = false, want true")
	}
	if err := m.Healthy(); err != nil {
		t.Fatalf("Healthy: %v", err)
	}

	mux := http.NewServeMux()
	m.Register(mux)

	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"notes list requires auth", http.MethodGet, "/index.php/apps/notes/api/v1/notes", http.StatusUnauthorized},
		{"note by id requires auth", http.MethodGet, "/index.php/apps/notes/api/v1/notes/1", http.StatusUnauthorized},
		{"settings requires auth", http.MethodGet, "/index.php/apps/notes/api/v1/settings", http.StatusUnauthorized},
		{"img sign requires auth", http.MethodPost, "/index.php/apps/notes/api/v1/img/sign", http.StatusUnauthorized},
		{"attachment requires auth", http.MethodGet, "/index.php/apps/notes/api/v1.4/attachment/1", http.StatusUnauthorized},
		{"preflight is public", http.MethodOptions, "/index.php/apps/notes/api/v1/notes", http.StatusNoContent},
		{"img proxy is public but signed", http.MethodGet, "/index.php/apps/notes/api/v1/img?u=http://example.com/x.jpg&t=bad", http.StatusForbidden},
		{"ocs capabilities", http.MethodGet, "/ocs/v2.php/cloud/capabilities", http.StatusOK},
		{"ocs user", http.MethodGet, "/ocs/v2.php/cloud/user", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code == http.StatusNotFound {
				t.Fatalf("%s %s: 404 (route not registered)", tc.method, tc.path)
			}
			if w.Code != tc.want {
				t.Fatalf("%s %s: status = %d, want %d", tc.method, tc.path, w.Code, tc.want)
			}
		})
	}

	// The API header is present on API responses (contract), including 401.
	req := httptest.NewRequest(http.MethodGet, "/index.php/apps/notes/api/v1/notes", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if got := w.Header().Get("X-Notes-API-Versions"); got != "0.2, 1.4" {
		t.Fatalf("X-Notes-API-Versions = %q, want %q", got, "0.2, 1.4")
	}

	// Sanity: an unrelated path is NOT claimed by the notes module.
	req = httptest.NewRequest(http.MethodGet, "/index.php/apps/news/api/v1-3/version", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("foreign path: status = %d, want 404", w.Code)
	}
}

// TestModuleDisabled: a disabled module registers nothing and stays healthy.
func TestModuleDisabled(t *testing.T) {
	cfg := &config.Config{Common: config.Common{NotesEnabled: false, NotesDataDir: t.TempDir()}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := New(cfg, log, nil)
	if err != nil {
		t.Fatalf("New disabled: %v", err)
	}
	if m.Enabled() {
		t.Fatal("Enabled = true, want false")
	}
	if err := m.Healthy(); err != nil {
		t.Fatalf("Healthy disabled: %v", err)
	}
	mux := http.NewServeMux()
	m.Register(mux)
	req := httptest.NewRequest(http.MethodGet, "/index.php/apps/notes/api/v1/notes", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("disabled module registered routes: status = %d, want 404", w.Code)
	}
}
