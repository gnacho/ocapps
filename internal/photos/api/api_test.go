package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/photos/store"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, nil, nil, nil, nil, nil,
		Config{Token: "test-token", Secret: []byte("0123456789abcdef0123456789abcdef")},
		slog.Default())
}

func do(t *testing.T, h http.Handler, method, path, authz string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestRutasBajoPublicPrefix: todas las rutas cuelgan de PublicPrefix
// (SPEC §4.3) y el prefijo es obligatorio (sin strip de proxy).
func TestRutasBajoPublicPrefix(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	bearer := "Bearer test-token"

	// con auth: las rutas existen bajo el prefijo
	for _, p := range []string{
		"/api/stats",
		"/api/assets",
		"/api/duplicates",
		"/api/tags",
		"/api/albums",
		"/api/folders",
		"/api/geo",
		"/api/places",
		"/api/memories/on-this-day",
		"/api/memories/highlights",
		"/api/timeline/calendar",
	} {
		rr := do(t, h, http.MethodGet, PublicPrefix+p, bearer)
		if rr.Code == http.StatusNotFound || rr.Code == http.StatusUnauthorized {
			t.Fatalf("GET %s%s -> %d", PublicPrefix, p, rr.Code)
		}
	}

	// sin prefijo: no hay ruta (el proxy ya no strip-pea)
	if rr := do(t, h, http.MethodGet, "/api/stats", bearer); rr.Code != http.StatusNotFound {
		t.Fatalf("GET /api/stats sin prefijo -> %d, quiero 404", rr.Code)
	}

	// sin auth: 401 (salvo video firmado y preflight)
	if rr := do(t, h, http.MethodGet, PublicPrefix+"/api/stats", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("stats sin auth -> %d, quiero 401", rr.Code)
	}
	if rr := do(t, h, http.MethodGet, PublicPrefix+"/api/stats", "Bearer otro"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("stats con token erróneo -> %d, quiero 401", rr.Code)
	}

	// preflight OPTIONS sin auth: 204 (CORS común, SPEC §4.4)
	rr := do(t, h, http.MethodOptions, PublicPrefix+"/api/stats", "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS -> %d, quiero 204", rr.Code)
	}
	if acao := rr.Header().Get("Access-Control-Allow-Origin"); acao != "*" {
		t.Fatalf("ACAO = %q", acao)
	}
}

// TestVideoFirmado: la firma usa PublicPrefix y la ruta firmada está exenta
// de auth (la firma HMAC es la credencial).
func TestVideoFirmado(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	bearer := "Bearer test-token"

	rr := do(t, h, http.MethodPost, PublicPrefix+"/api/assets/42/video-url", bearer)
	if rr.Code != http.StatusOK {
		t.Fatalf("video-url -> %d", rr.Code)
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body.URL, PublicPrefix+"/api/video/42?exp=") {
		t.Fatalf("url firmada sin PublicPrefix: %q", body.URL)
	}

	// la URL firmada pasa la exemption de auth (sin Bearer) y llega al
	// handler: el asset 42 no existe -> 404 (no 401).
	if rr := do(t, h, http.MethodGet, body.URL, ""); rr.Code != http.StatusNotFound {
		t.Fatalf("GET %q sin auth -> %d, quiero 404 (exenta de auth)", body.URL, rr.Code)
	}
	// firma manipulada -> 401 del propio handler
	tampered := strings.Replace(body.URL, "sig=", "sig=0", 1)
	if rr := do(t, h, http.MethodGet, tampered, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("firma manipulada -> %d, quiero 401", rr.Code)
	}
	// expirada -> 401
	exp := time.Now().Add(-time.Hour).Unix()
	expired := PublicPrefix + "/api/video/42?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + s.signVideo(42, exp)
	if rr := do(t, h, http.MethodGet, expired, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("firma expirada -> %d, quiero 401", rr.Code)
	}
}

// TestRescanBuffer: Q5 — el canal de rescans admite 4 peticiones encoladas y
// la quinta se descarta (respuesta "ya hay un scan en curso").
func TestRescanBuffer(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	bearer := "Bearer test-token"
	for i := 0; i < 4; i++ {
		rr := do(t, h, http.MethodPost, PublicPrefix+"/api/admin/rescan", bearer)
		if !strings.Contains(rr.Body.String(), "encolado") {
			t.Fatalf("rescan %d: %s", i, rr.Body.String())
		}
	}
	rr := do(t, h, http.MethodPost, PublicPrefix+"/api/admin/rescan", bearer)
	if !strings.Contains(rr.Body.String(), "en curso") {
		t.Fatalf("rescan con cola llena: %s", rr.Body.String())
	}
	if n := len(s.rescanCh); n != 4 {
		t.Fatalf("buffer del canal = %d, quiero 4", n)
	}
}
