// Package httpx: helpers HTTP compartidos por los tres módulos (SPEC §2.1,
// §4.4). Unifica writeJSON/errorStatus/decodeBody/withCORS de ocnews, ocnotes
// y ocphotos. CORS mantiene ACAO * (sin cookies, auth por cabecera) con la
// unión de métodos y cabeceras de los tres servicios.
package httpx

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// Cabeceras CORS unificadas (SPEC §4.4): unión de las de news, notes y
// photos. Sin Access-Control-Allow-Credentials: no hay cookies.
const (
	corsAllowOrigin   = "*"
	corsAllowMethods  = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowHeaders  = "Authorization, Content-Type, If-Match, OCS-APIRequest"
	corsExposeHeaders = "ETag, Last-Modified, X-Notes-API-Versions, X-Notes-Chunk-Cursor"
)

// WriteJSON escribe v como JSON con el código dado. Fija Content-Type ANTES
// de WriteHeader (corrige el bug menor de ocphotos que escribía siempre 200).
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("escribir respuesta JSON", "err", err)
	}
}

// HTTPError es un error con código HTTP, code estable para clientes y
// mensaje presentable. Los handlers lo devuelven y ErrorStatus lo escribe.
type HTTPError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *HTTPError) Error() string { return e.Code + ": " + e.Message }

// NewError construye un HTTPError.
func NewError(status int, code, msg string) *HTTPError {
	return &HTTPError{Status: status, Code: code, Message: msg}
}

// ErrorStatus escribe el envelope {"error":{"code","message"}}. Si err es
// (o envuelve) un *HTTPError usa su status/code/message; si no, 500
// "internal" sin filtrar el mensaje interno al cliente (va al log).
func ErrorStatus(w http.ResponseWriter, r *http.Request, err error) {
	var he *HTTPError
	if !errors.As(err, &he) {
		slog.Error("error interno", "path", r.URL.Path, "err", err)
		he = &HTTPError{Status: http.StatusInternalServerError, Code: "internal", Message: "internal error"}
	}
	if he.Status < 400 || he.Status > 599 {
		he.Status = http.StatusInternalServerError
	}
	WriteJSON(w, he.Status, map[string]*HTTPError{"error": he})
}

// maxBodyBytes es el tope de lectura de bodies de API (4 MB, como ocnews).
const maxBodyBytes = 4 << 20

// DecodeBody decodifica JSON tolerante (port de ocnews): decide por el
// CONTENIDO (primer byte no-blanco '{' o '['), no por el Content-Type —
// `curl -d` manda form-urlencoded aunque el body sea JSON. Si no es JSON,
// deja el body intacto (devuelve nil) para que el handler haga ParseForm.
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

// CORS es el middleware unificado (SPEC §4.4). El preflight OPTIONS se
// responde aquí con 204 SIN pasar por auth ni por el handler siguiente.
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

// Preflight devuelve un handler 204 para registrar explícitamente
// "OPTIONS <prefix>/" antes del handler autenticado (patrón de ocnews).
func Preflight() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}
