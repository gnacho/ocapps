// Package api: HTTP server of the news module with the News REST API v1.3
// (contract: docs/development/api/api-v1-3.md from the nextcloud/news repo)
// under /index.php/apps/news/api/v1-3/ + our own /api/me|users API.
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

// Exact base path used by News clients (Nextcloud included).
const Base = "/index.php/apps/news/api/v1-3"

// reportedVersion is the version of the News app we report, clients
// use it to decide features. Feed filtering requires >= 28.4.0.
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

// Handler mounts the full standalone router (tests and use outside the
// unified mux). In ocapps the module registers the routes with RegisterOn on
// the shared mux, /healthz is served by common (SPEC §4.1), here it is only
// kept for the package tests.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.RegisterOn(mux)
	return mux
}

// RegisterOn registers the news module routes on a shared mux
// (SPEC §4.1): the News API v1.3 contract base with auth (except the
// public exceptions /img, /favicon/, /share/, /websub/), our own
// /api/me|users API with EXACT PATTERNS (no /api/ catch-all) and the
// OPTIONS preflight. It does NOT register /healthz (common owns it) nor
// /ocs/v2.php/cloud/user (§4.2 collision: notes serves it).
func (s *Server) RegisterOn(mux *http.ServeMux) {
	api := http.NewServeMux()
	s.routes(api)
	authed := auth.Middleware(s.validator, httpx.CORS(api))

	// PUBLIC image proxy (the browser <img> cannot send auth):
	// the security comes from the HMAC signature of the URL. With an explicit method to
	// avoid clashing with the OPTIONS preflight pattern.
	mux.Handle("GET "+Base+"/img", http.StripPrefix(Base, httpx.CORS(http.HandlerFunc(s.imgs.Serve))))

	// PUBLIC favicon: the browser <img> carries no auth. It only serves from the
	// on-disk cache (no fetch on demand), so there is no SSRF surface.
	mux.Handle("GET "+Base+"/favicon/", http.StripPrefix(Base, httpx.CORS(s.faviconRouter())))

	// PUBLIC shared article: the URL carries a random token (share).
	mux.Handle("GET "+Base+"/share/", http.StripPrefix(Base, httpx.CORS(s.shareRouter())))

	// PUBLIC WebSub callback (the hub calls it): GET verification + POST delivery.
	mux.Handle("GET "+Base+"/websub/", http.StripPrefix(Base, httpx.CORS(s.websubRouter())))
	mux.Handle("POST "+Base+"/websub/", http.StripPrefix(Base, httpx.CORS(s.websubRouter())))

	mux.Handle(Base+"/", http.StripPrefix(Base, authed))
	// App own API (profile/users): EXACT patterns, no /api/ catch-all
	// (SPEC §4.1/§4.3), with its per-path OPTIONS preflight (no auth).
	pre := httpx.CORS(httpx.Preflight())
	optionsDone := map[string]bool{}
	for _, rt := range s.userAPIRoutes() {
		mux.Handle(rt.pattern, auth.Middleware(s.validator, httpx.CORS(rt.h)))
		if !optionsDone[rt.path] { // several methods share path: a single OPTIONS
			optionsDone[rt.path] = true
			mux.Handle("OPTIONS "+rt.path, pre)
		}
	}
	// /ocs/v2.php/cloud/user is NOT registered here (SPEC §4.2 collision): since
	// H5 only notes serves it with its extended handler (XML by default, OCS
	// JSON for news-android with ?format=json).
	// CORS preflight before auth (it carries no credentials).
	mux.Handle("OPTIONS "+Base+"/", http.StripPrefix(Base, httpx.CORS(httpx.Preflight())))
}

// writeJSON is a shim over httpx.WriteJSON (the unified helper sets
// Content-Type before WriteHeader, SPEC §2.1).
func writeJSON(w http.ResponseWriter, code int, v any) {
	httpx.WriteJSON(w, code, v)
}

func writeEmpty(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// errorStatus writes the error envelope with a stable code and a message
// translated to the negotiated language of the request.
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
		slog.Error("write JSON error", "err", err)
	}
}

// decodeBody is a shim over httpx.DecodeBody (tolerant JSON decoding:
// it decides by the CONTENT, not by the Content-Type, if it is not JSON it leaves
// the body untouched for ParseForm).
func decodeBody(r *http.Request, v any) error {
	return httpx.DecodeBody(r, v)
}
