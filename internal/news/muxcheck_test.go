package news

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Comportamiento de método no soportado y trailing slash con patrones exactos.
func TestExactPatternEdgeCases(t *testing.T) {
	m := testModule(t)
	mux := http.NewServeMux()
	m.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// DELETE /api/me (path existe con otros métodos) → 405, no 404
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/me", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /api/me: %d (esperado 405)", resp.StatusCode)
	}
	// GET /api/users/1 (solo PUT/DELETE/OPTIONS registrados) → 405
	req2, _ := http.NewRequest("GET", srv.URL+"/api/users/1", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/users/1: %d (esperado 405)", resp2.StatusCode)
	}
}
