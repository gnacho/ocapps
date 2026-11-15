package news

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Behaviour of unsupported method and trailing slash with exact patterns.
func TestExactPatternEdgeCases(t *testing.T) {
	m := testModule(t)
	mux := http.NewServeMux()
	m.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// DELETE /api/me (path exists with other methods) -> 405, not 404
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/me", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /api/me: %d (expected 405)", resp.StatusCode)
	}
	// GET /api/users/1 (only PUT/DELETE/OPTIONS registered) -> 405
	req2, _ := http.NewRequest("GET", srv.URL+"/api/users/1", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/users/1: %d (esperado 405)", resp2.StatusCode)
	}
}
