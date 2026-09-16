// Package api: servidor HTTP del módulo news con la News REST API v1.3
// (contrato: docs/development/api/api-v1-3.md del repo nextcloud/news)
// bajo /index.php/apps/news/api/v1-3/ + la API propia /api/me|users.
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/gnacho/ocapps/internal/common/cred"
	"github.com/gnacho/ocapps/internal/common/httpx"
	"github.com/gnacho/ocapps/internal/common/imgproxy"
	"github.com/gnacho/ocapps/internal/news/auth"
	"github.com/gnacho/ocapps/internal/news/extract"
	"github.com/gnacho/ocapps/internal/news/favicon"
	"github.com/gnacho/ocapps/internal/news/feed"
	"github.com/gnacho/ocapps/internal/news/i18n"
	"github.com/gnacho/ocapps/internal/news/refresher"
	"github.com/gnacho/ocapps/internal/news/store"
)

// Base path exacta que usan los clientes News (Nextcloud incluido).
const Base = "/index.php/apps/news/api/v1-3"

// reportedVersion es la versión de la app News que reportamos; los clientes
// la usan para decidir features. Filtro de feeds requiere >= 28.4.0.
const reportedVersion = "28.4.0"

type Server struct {
	store     *store.Store
	validator auth.Validator
	fetcher   feed.Fetcher
	refresher *refresher.Refresher
	favicons  *favicon.Cache
	imgs      *imgproxy.Proxy
	extract   *extract.Extractor
	cred      *cred.Cipher
	retention time.Duration
	log       *slog.Logger
}

func NewServer(s *store.Store, v auth.Validator, f feed.Fetcher, r *refresher.Refresher,
	fc *favicon.Cache, ip *imgproxy.Proxy, ex *extract.Extractor, c *cred.Cipher,
	retention time.Duration, log *slog.Logger) *Server {
	return &Server{store: s, validator: v, fetcher: f, refresher: r, favicons: fc, imgs: ip,
		extract: ex, cred: c, retention: retention, log: log}
}

// Handler monta el router completo standalone (tests y uso fuera del mux
// unificado). En ocapps el módulo registra las rutas con RegisterOn sobre el
// mux compartido; /healthz lo sirve common (SPEC §4.1), aquí solo se
// mantiene para los tests del paquete.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.RegisterOn(mux)
	return mux
}

// RegisterOn registra las rutas del módulo news en un mux compartido
// (SPEC §4.1): la base del contrato News API v1.3 con auth (salvo las
// excepciones públicas /img, /favicon/, /share/, /websub/), la API propia
// /api/me|users con PATRONES EXACTOS (no catch-all /api/), el stub OCS y
// los preflight OPTIONS. NO registra /healthz (es de common).
func (s *Server) RegisterOn(mux *http.ServeMux) {
	api := http.NewServeMux()
	s.routes(api)
	authed := auth.Middleware(s.validator, httpx.CORS(api))

	// Proxy de imágenes PÚBLICO (el <img> del navegador no puede mandar auth):
	// la seguridad la aporta la firma HMAC de la URL. Con método explícito para
	// no chocar con el patrón OPTIONS de preflight.
	mux.Handle("GET "+Base+"/img", http.StripPrefix(Base, httpx.CORS(http.HandlerFunc(s.imgs.Serve))))

	// Favicon PÚBLICO: el <img> del navegador no lleva auth. Solo sirve desde el
	// cache en disco (sin fetch en demanda), así que no hay superficie SSRF.
	mux.Handle("GET "+Base+"/favicon/", http.StripPrefix(Base, httpx.CORS(s.faviconRouter())))

	// Artículo compartido PÚBLICO: la URL lleva un token aleatorio (share).
	mux.Handle("GET "+Base+"/share/", http.StripPrefix(Base, httpx.CORS(s.shareRouter())))

	// WebSub callback PÚBLICO (lo llama el hub): GET verificación + POST delivery.
	mux.Handle("GET "+Base+"/websub/", http.StripPrefix(Base, httpx.CORS(s.websubRouter())))
	mux.Handle("POST "+Base+"/websub/", http.StripPrefix(Base, httpx.CORS(s.websubRouter())))

	mux.Handle(Base+"/", http.StripPrefix(Base, authed))
	// API propia de la app (perfil/usuarios): patrones EXACTOS, no catch-all
	// /api/ (SPEC §4.1/§4.3), con su preflight OPTIONS por path (sin auth).
	pre := httpx.CORS(httpx.Preflight())
	optionsDone := map[string]bool{}
	for _, rt := range s.userAPIRoutes() {
		mux.Handle(rt.pattern, auth.Middleware(s.validator, httpx.CORS(rt.h)))
		if !optionsDone[rt.path] { // varios métodos comparten path: un solo OPTIONS
			optionsDone[rt.path] = true
			mux.Handle("OPTIONS "+rt.path, pre)
		}
	}
	// Stub OCS user para clientes Nextcloud (news-android lee el display name
	// de /ocs/v2.php/cloud/user con ?format=json; OpenCloud no lo sirve y el
	// handler de notes solo emite XML — colisión §4.2, ver docs del módulo).
	mux.Handle("/ocs/v2.php/cloud/user", auth.Middleware(s.validator, httpx.CORS(http.HandlerFunc(s.ocsUser))))
	// Preflight de CORS antes de auth (no lleva credenciales).
	mux.Handle("OPTIONS "+Base+"/", http.StripPrefix(Base, httpx.CORS(httpx.Preflight())))
}

// writeJSON es un shim sobre httpx.WriteJSON (el helper unificado fija
// Content-Type antes de WriteHeader, SPEC §2.1).
func writeJSON(w http.ResponseWriter, code int, v any) {
	httpx.WriteJSON(w, code, v)
}

func writeEmpty(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// errorStatus escribe el envelope de error con code estable y mensaje
// traducido al idioma negociado de la petición.
func errorStatus(w http.ResponseWriter, r *http.Request, status int, key string) {
	var b struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	b.Error.Code = key
	b.Error.Message = i18n.T(auth.Lang(r), key)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(b); err != nil {
		slog.Error("escribir error JSON", "err", err)
	}
}

// decodeBody es un shim sobre httpx.DecodeBody (decodifica JSON tolerante:
// decide por el CONTENIDO, no por el Content-Type; si no es JSON deja el
// body intacto para ParseForm).
func decodeBody(r *http.Request, v any) error {
	return httpx.DecodeBody(r, v)
}
