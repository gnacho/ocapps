package photos

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/config"
)

// fakeOpenCloudConEspacio simula lo mínimo de Graph/WebDAV para el wiring.
func fakeOpenCloudConEspacio(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/graph/v1.0/me":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "user-1"})
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

func photosCfg() config.PhotosConfig {
	return config.PhotosConfig{
		User: "admin", AppToken: "app-token", Token: "test-token",
		ScanRoot: "Fotos", ScanEvery: time.Hour,
	}
}

func TestNewRequiereCredenciales(t *testing.T) {
	_, err := New(config.PhotosConfig{}, config.Common{OpenCloudURL: "http://x", PhotosDataDir: t.TempDir()}, slog.Default(), nil)
	if err == nil {
		t.Fatal("New sin user/app-token debería fallar (módulo failed en el wiring)")
	}
}

func TestModuloFailedSiGraphCae(t *testing.T) {
	common := config.Common{OpenCloudURL: "http://127.0.0.1:1", PhotosDataDir: t.TempDir()}
	m, err := New(photosCfg(), common, slog.Default(), nil)
	if err != nil {
		t.Fatalf("New no debe fallar por Graph caído (reintenta en background): %v", err)
	}
	if err := m.Healthy(); err == nil {
		t.Fatal("Healthy debería reportar el fallo de init")
	}
	// el namespace responde 503 mientras el backend no esté listo (SPEC §4.5)
	mux := http.NewServeMux()
	m.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, PublicPrefix+"/api/stats", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("namespace failed -> %d, quiero 503", rr.Code)
	}
	if got := rr.Body.String(); got == "" {
		t.Fatal("body del 503 vacío")
	}
	// Run ante ctx cancelado durante el reintento retorna sin colgarse
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run no retornó al cancelar ctx durante el reintento")
	}
}

func TestModuloOKContraGraphFalso(t *testing.T) {
	srv := fakeOpenCloudConEspacio(t)
	dataDir := t.TempDir()
	common := config.Common{OpenCloudURL: srv.URL, PhotosDataDir: dataDir}
	m, err := New(photosCfg(), common, slog.Default(), nil)
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

	// rutas reales bajo el prefijo con el token estático
	mux := http.NewServeMux()
	m.Register(mux)
	req := httptest.NewRequest(http.MethodGet, PublicPrefix+"/api/stats", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("stats -> %d (%s)", rr.Code, rr.Body.String())
	}

	// Run hace el scan inicial (PROPFIND 404 -> warn) y retorna al cancelar
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	time.Sleep(300 * time.Millisecond) // deja terminar el scan inicial
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
