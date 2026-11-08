package extensions

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"net/http"

	"github.com/gnacho/ocapps/internal/common/httpx"
)

//go:embed panel.html
var panelHTML []byte

// Register mounts the panel and its API. The token is required: with no token
// configured the routes are not registered at all, so a misconfigured instance
// never exposes an installer that can run privileged commands.
func Register(mux *http.ServeMux, s *Service, token string) {
	if token == "" {
		return
	}
	mux.HandleFunc("GET /ocapps-admin/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(panelHTML)
	})
	mux.HandleFunc("GET /ocapps-admin/api/extensions", auth(token, s.handleList))
	mux.HandleFunc("POST /ocapps-admin/api/extensions/{id}/update", auth(token, s.handleUpdate))
}

func auth(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Ocapps-Admin-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			httpx.WriteJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

func (s *Service) handleList(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"apps":    s.List(r.Context()),
		"last":    s.Last(),
		"running": s.Running(),
	})
}

func (s *Service) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	force := r.URL.Query().Get("force") != ""
	res, err := s.Update(r.Context(), id, force)
	code := http.StatusOK
	if err != nil {
		code = http.StatusBadGateway
	}
	body, _ := json.Marshal(map[string]any{"result": res, "error": errString(err)})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
