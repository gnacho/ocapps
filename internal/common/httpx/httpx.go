// Package httpx: HTTP helpers shared by the three modules (SPEC §2.1, §4.4).
// It unifies writeJSON/errorStatus/decodeBody/withCORS from ocnews, ocnotes
// and ocphotos. CORS keeps ACAO * (no cookies, header auth) with the union of
// methods and headers of the three services.
package httpx

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// Unified CORS headers (SPEC §4.4): union of those of news, notes and photos.
// No Access-Control-Allow-Credentials: there are no cookies.
const (
	corsAllowOrigin   = "*"
	corsAllowMethods  = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowHeaders  = "Authorization, Content-Type, If-Match, OCS-APIRequest, X-Ocapps-Client"
	corsExposeHeaders = "ETag, Last-Modified, X-Notes-API-Versions, X-Notes-Chunk-Cursor"
)

// WriteJSON writes v as JSON with the given code. It sets Content-Type BEFORE
// WriteHeader (fixes the minor ocphotos bug that always wrote 200).
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write JSON response", "err", err)
	}
}

// HTTPError is an error with an HTTP code, a stable code for clients and a
// presentable message. Handlers return it and ErrorStatus writes it.
type HTTPError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *HTTPError) Error() string { return e.Code + ": " + e.Message }

// NewError builds an HTTPError.
func NewError(status int, code, msg string) *HTTPError {
	return &HTTPError{Status: status, Code: code, Message: msg}
}

// ErrorStatus writes the {"error":{"code","message"}} envelope. If err is (or
// wraps) an *HTTPError it uses its status/code/message, otherwise 500
// "internal" without leaking the internal message to the client (it goes to
// the log).
func ErrorStatus(w http.ResponseWriter, r *http.Request, err error) {
	var he *HTTPError
	if !errors.As(err, &he) {
		slog.Error("internal error", "path", r.URL.Path, "err", err)
		he = &HTTPError{Status: http.StatusInternalServerError, Code: "internal", Message: "internal error"}
	}
	if he.Status < 400 || he.Status > 599 {
		he.Status = http.StatusInternalServerError
	}
	WriteJSON(w, he.Status, map[string]*HTTPError{"error": he})
}

// maxBodyBytes is the read cap for API bodies (4 MB, like ocnews).
const maxBodyBytes = 4 << 20

// DecodeBody decodes tolerant JSON (port of ocnews): it decides by the
// CONTENT (first non-blank byte '{' or '['), not by the Content-Type -
// `curl -d` sends form-urlencoded even when the body is JSON. If it is not
// JSON, it leaves the body intact (returns nil) so the handler can ParseForm.
func DecodeBody(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	br := bufio.NewReader(io.LimitReader(r.Body, maxBodyBytes))
	lead, err := peekNonSpace(br)
	if err != nil || (lead != '{' && lead != '[') {
		r.Body = io.NopCloser(br)
		return nil
	}
	return json.NewDecoder(br).Decode(dst)
}

func peekNonSpace(br *bufio.Reader) (byte, error) {
	for i := 0; i < 64; i++ {
		b, err := br.Peek(1)
		if err != nil {
			return 0, err
		}
		switch b[0] {
		case ' ', '\t', '\r', '\n':
			_, _ = br.ReadByte()
		default:
			return b[0], nil
		}
	}
	return 0, nil
}

// CORS is the unified middleware (SPEC §4.4). The OPTIONS preflight is
// answered here with 204 WITHOUT going through auth or the next handler.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", corsAllowOrigin)
		h.Set("Access-Control-Allow-Methods", corsAllowMethods)
		h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
		h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Preflight returns a 204 handler to explicitly register
// "OPTIONS <prefix>/" before the authenticated handler (ocnews pattern).
func Preflight() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}
