package news

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/config"
	"github.com/gnacho/ocapps/internal/news/api"
)

// testModule builds the module in local mode with a temp data dir and a
// bootstrap admin.
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

// TestRunShutdownGracioso: Run blocks until ctx is cancelled and returns without error.
func TestRunGracefulShutdown(t *testing.T) {
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
		t.Fatal("Run did not return after cancelling ctx")
	}
}

// TestRegisterContractPaths: the contract paths (SPEC §4.1) are left
// registered in the shared mux (none falls to 404 for lack of pattern)
// and they respond with the expected auth/preflight behaviour.
func TestRegisterContractPaths(t *testing.T) {
	m := testModule(t)
	mux := http.NewServeMux()
	m.Register(mux)

	// 1) every contract path has a registered pattern (not 404 for
	// missing route). It includes the v1.3 base, public exceptions, exact /api/*
	// and the OCS stub.
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
		{"GET", api.Base + "/img"},               // public (HMAC signature)
		{"GET", api.Base + "/favicon/abcdef"},    // public
		{"GET", api.Base + "/share/sometoken"},   // public
		{"GET", api.Base + "/websub/callback/1"}, // public (hub)
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
		{"OPTIONS", api.Base + "/folders"}, // preflight without auth
		{"OPTIONS", "/api/me"},             // preflight without auth
	}
	for _, p := range paths {
		req := httptest.NewRequest(p.method, p.path, nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("%s %s: no registered pattern (it would be a 404)", p.method, p.path)
		}
	}

	// unknown /api/ must NOT exist (exact patterns, no catch-all).
	req := httptest.NewRequest("GET", "/api/does-not-exist", nil)
	if _, pattern := mux.Handler(req); pattern != "" {
		t.Fatalf("/api/does-not-exist must not have a pattern (catch-all forbidden): %q", pattern)
	}

	// 2) behaviour: auth required in the API, preflight 204, public ones != 401.
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

	check("GET", api.Base+"/version", "without auth -> 401", http.StatusUnauthorized, false)
	check("GET", api.Base+"/version", "with auth -> 200", http.StatusOK, true)
	check("GET", "/api/me", "without auth -> 401", http.StatusUnauthorized, false)
	check("GET", "/api/me", "with auth -> 200", http.StatusOK, true)
	check("GET", "/api/me/settings", "with auth -> 200", http.StatusOK, true)
	check("GET", "/api/me/rules", "with auth -> 200", http.StatusOK, true)
	check("GET", "/api/users", "with admin auth -> 200", http.StatusOK, true)
	check("OPTIONS", api.Base+"/folders", "preflight -> 204", http.StatusNoContent, false)
	check("OPTIONS", "/api/me", "preflight -> 204", http.StatusNoContent, false)
	check("GET", api.Base+"/img", "public: bad signature -> 403", http.StatusForbidden, false)

	// SPEC §4.2: news no longer registers /ocs/v2.php/cloud/user (notes serves it
	// with its extended handler since H5).
	req2, err := http.NewRequest("GET", srv.URL+"/ocs/v2.php/cloud/user?format=json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("news must not register /ocs/v2.php/cloud/user: %d (want 404)", resp2.StatusCode)
	}
}

// TestNewConfigInvalida: a config ModuleErr degrades the module (New
// returns an error) without touching the process, H5 registers the 503 (D3).
func TestNewInvalidConfig(t *testing.T) {
	cfg := &config.Config{
		Common: config.Common{NewsDataDir: t.TempDir(), AuthMode: "local", NewsEnabled: true},
		News:   config.NewsConfig{}, // durations at zero -> ModuleErr is set by config.Load, here we simulate New with invalid values
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Without AuthUser/Pass and an empty DB in local mode -> bootstrap impossible -> error.
	if _, err := New(cfg, log, nil); err == nil {
		t.Fatal("New in local mode without users nor bootstrap must fail")
	}
}
