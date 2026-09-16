package news

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/config"
	"github.com/gnacho/ocapps/internal/news/api"
)

// testModule construye el módulo en modo local con data dir temporal y un
// admin bootstrap.
func testModule(t *testing.T) *Module {
	t.Helper()
	cfg := &config.Config{
		Common: config.Common{
			NewsDataDir: t.TempDir(),
			AuthMode:    "local",
			NewsEnabled: true,
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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := New(cfg, log, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestModuleBasics(t *testing.T) {
	m := testModule(t)
	if m.Name() != "news" || !m.Enabled() {
		t.Fatalf("Name/Enabled: %s %v", m.Name(), m.Enabled())
	}
	if err := m.Healthy(); err != nil {
		t.Fatalf("Healthy: %v", err)
	}
}

// TestRunShutdownGracioso: Run bloquea hasta cancelar ctx y vuelve sin error.
func TestRunShutdownGracioso(t *testing.T) {
	m := testModule(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run no retornó tras cancelar ctx")
	}
}

// TestRegisterContractPaths: los paths del contrato (SPEC §4.1) quedan
// registrados en el mux compartido (ninguno cae a 404 por falta de patrón)
// y responden con el comportamiento esperado de auth/preflight.
func TestRegisterContractPaths(t *testing.T) {
	m := testModule(t)
	mux := http.NewServeMux()
	m.Register(mux)

	// 1) todos los paths del contrato tienen patrón registrado (≠ 404 por
	// ausencia de ruta). Incluye base v1.3, excepciones públicas, /api/*
	// exactos y el stub OCS.
	paths := []struct {
		method, path string
	}{
		{"GET", api.Base + "/version"},
		{"GET", api.Base + "/status"},
		{"GET", api.Base + "/user"},
		{"GET", api.Base + "/folders"},
		{"POST", api.Base + "/folders"},
		{"GET", api.Base + "/feeds"},
		{"POST", api.Base + "/feeds"},
		{"GET", api.Base + "/items"},
		{"POST", api.Base + "/items/read"},
		{"GET", api.Base + "/img"},               // público (firma HMAC)
		{"GET", api.Base + "/favicon/abcdef"},    // público
		{"GET", api.Base + "/share/sometoken"},   // público
		{"GET", api.Base + "/websub/callback/1"}, // público (hub)
		{"POST", api.Base + "/websub/callback/1"},
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
		{"GET", "/ocs/v2.php/cloud/user"},
		{"OPTIONS", api.Base + "/folders"}, // preflight sin auth
		{"OPTIONS", "/api/me"},             // preflight sin auth
	}
	for _, p := range paths {
		req := httptest.NewRequest(p.method, p.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("%s %s: sin patrón registrado (sería 404)", p.method, p.path)
		}
	}

	// /api/ desconocido NO debe existir (patrones exactos, no catch-all).
	req := httptest.NewRequest("GET", "/api/no-existe", nil)
	if _, pattern := mux.Handler(req); pattern != "" {
		t.Fatalf("/api/no-existe no debe tener patrón (catch-all prohibido): %q", pattern)
	}

	// 2) comportamiento: auth exigida en API, preflight 204, públicos ≠ 401.
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	check := func(method, path, wantDesc string, wantCode int, auth bool) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if auth {
			req.SetBasicAuth("admin", "pass1234")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != wantCode {
			t.Errorf("%s %s (%s): got %d, want %d", method, path, wantDesc, resp.StatusCode, wantCode)
		}
	}

	check("GET", api.Base+"/version", "sin auth → 401", http.StatusUnauthorized, false)
	check("GET", api.Base+"/version", "con auth → 200", http.StatusOK, true)
	check("GET", "/api/me", "sin auth → 401", http.StatusUnauthorized, false)
	check("GET", "/api/me", "con auth → 200", http.StatusOK, true)
	check("GET", "/api/me/settings", "con auth → 200", http.StatusOK, true)
	check("GET", "/api/me/rules", "con auth → 200", http.StatusOK, true)
	check("GET", "/api/users", "con auth admin → 200", http.StatusOK, true)
	check("OPTIONS", api.Base+"/folders", "preflight → 204", http.StatusNoContent, false)
	check("OPTIONS", "/api/me", "preflight → 204", http.StatusNoContent, false)
	check("GET", api.Base+"/img", "público: firma mala → 403", http.StatusForbidden, false)

	// OCS stub: con auth → 200 y payload OCS JSON (news-android pide JSON).
	req2, err := http.NewRequest("GET", srv.URL+"/ocs/v2.php/cloud/user?format=json", nil)
	if err != nil {
		t.Fatal(err)
	}
	req2.SetBasicAuth("admin", "pass1234")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	body, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ocs"`) ||
		!strings.Contains(string(body), `"displayname"`) {
		t.Fatalf("ocs stub: %d %s", resp2.StatusCode, body)
	}
}

// TestNewConfigInvalida: un ModuleErr de config degrada el módulo (New
// devuelve error) sin tocar el proceso — el 503 lo registra H5 (D3).
func TestNewConfigInvalida(t *testing.T) {
	cfg := &config.Config{
		Common: config.Common{NewsDataDir: t.TempDir(), AuthMode: "local", NewsEnabled: true},
		News:   config.NewsConfig{}, // durations a cero → ModuleErr lo fija config.Load; aquí simulamos New con valores inválidos
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Sin AuthUser/Pass y BD vacía en modo local → bootstrap imposible → error.
	if _, err := New(cfg, log, nil); err == nil {
		t.Fatal("New con modo local sin usuarios ni bootstrap debe fallar")
	}
}
