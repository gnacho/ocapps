package module

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUnavailable: el handler sustituto D3 responde 503 con el envelope
// {"error":"module <name> unavailable"} y Content-Type JSON en cualquier
// método.
func TestUnavailable(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		req := httptest.NewRequest(method, "/index.php/apps/news/api/v1-3/version", nil)
		w := httptest.NewRecorder()
		Unavailable("news").ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", method, w.Code)
		}
		var body map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body no es JSON: %v", method, err)
		}
		if body["error"] != "module news unavailable" {
			t.Fatalf("%s: error = %q", method, body["error"])
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Fatalf("%s: Content-Type = %q", method, ct)
		}
	}
}
