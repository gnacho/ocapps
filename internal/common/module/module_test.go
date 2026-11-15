package module

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestUnavailable: the D3 substitute handler responds 503 with the
// {"error":"module <name> unavailable"} envelope and JSON Content-Type on any
// method.
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
			t.Fatalf("%s: body is not JSON: %v", method, err)
		}
		if body["error"] != "module news unavailable" {
			t.Fatalf("%s: error = %q", method, body["error"])
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Fatalf("%s: Content-Type = %q", method, ct)
		}
	}
}
