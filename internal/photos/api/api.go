// Package api — REST consumida por la extensión ocphotos bajo PublicPrefix
// (SPEC §4.3: el proxy deja de strip-pear /ocphotos-api/). Multi-tenant
// (H8, SPEC §6.2): cualquier usuario de OpenCloud entra con su Bearer OIDC
// (validado por el middleware común con política MultiTenant) o con Basic
// app-password; el *auth.User* resuelto se inyecta en el request context y
// TODOS los handlers scopean al owner (oc_id) con OwnerFrom. El Bearer
// estático OCAPPS_PHOTOS_TOKEN está DEPRECATED: solo existe si
// OCAPPS_PHOTOS_USER está configurado y mapea al owner legacy (el oc_id de
// ese usuario). Las rutas firmadas de vídeo están exentas (la firma HMAC va
// en la URL: capability URL, ver videoStream).
//
// Q7 (SPEC §7): los endpoints sin consumidor en la extensión (GET /api/thumb,
// GET /api/assets/{id}/hls/{file}, GET /api/assets/{id}) se eliminaron en el
// port; el streaming firmado /api/video/{id} sí se usa y se mantiene.
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/httpx"
	"github.com/gnacho/ocapps/internal/common/webdav"
	"github.com/gnacho/ocapps/internal/photos/geo"
	"github.com/gnacho/ocapps/internal/photos/phash"
	"github.com/gnacho/ocapps/internal/photos/store"
	"github.com/gnacho/ocapps/internal/photos/thumb"
)

// PublicPrefix es el namespace público del módulo photos (SPEC §4.3). Se usa
// tanto en el registro de rutas como en la firma de URLs de vídeo.
const PublicPrefix = "/ocphotos-api"

// Config: dependencias de configuración del servidor API.
type Config struct {
	Token  string // Bearer estático DEPRECATED (OCAPPS_PHOTOS_TOKEN); "" = desactivado
	Secret []byte // mediasecret para firmar URLs de vídeo (cred.LoadOrCreateSecret; Q3)
}

// SessionProvider conecta la API con el registry de sesiones en memoria del
// módulo (H8 §6.2). Lo implementa el módulo photos; la API no conoce el
// registry para no crear un ciclo de imports.
type SessionProvider interface {
	// Touch registra la actividad del owner con la credencial del request
	// (crea/actualiza su sesión en memoria y dispara scan si toca).
	Touch(ctx context.Context, u *auth.User, cred auth.Credential)
	// DavFor devuelve el cliente DAV de la sesión del owner (nil si no hay).
	DavFor(ctx context.Context, owner string) *webdav.Client
	// SpacePrefix devuelve el path del webDavUrl del espacio personal del
	// owner (handler de carpetas), resolviéndolo on-demand con la credencial
	// de su sesión si hace falta. "" si no se puede resolver.
	SpacePrefix(ctx context.Context, owner string) string
	// LegacyOwner resuelve el oc_id del usuario legacy OCAPPS_PHOTOS_USER
	// (identidad del Bearer estático DEPRECATED). Error si no se puede
	// resolver (OpenCloud caído, credenciales inválidas).
	LegacyOwner(ctx context.Context) (string, error)
	// RequestScan encola un scan solo del owner del request.
	RequestScan(owner string)
}

// ownerKey es la clave privada del owner en el request context (H8).
type ownerKey struct{}

// OwnerFrom devuelve el owner (oc_id) autenticado del request, "" si no hay.
func OwnerFrom(ctx context.Context) string {
	owner, _ := ctx.Value(ownerKey{}).(string)
	return owner
}

// withOwner inyecta el owner en el contexto (lo usa withAuth tras autenticar).
func withOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerKey{}, owner)
}

type Server struct {
	st        *store.Store
	thumbs    *thumb.Service
	geo       *geo.Geocoder
	validator *auth.GraphValidator // nil = solo token estático
	provider  SessionProvider
	cfg       Config
	log       *slog.Logger
}

func New(st *store.Store, th *thumb.Service, gc *geo.Geocoder, v *auth.GraphValidator, sp SessionProvider, cfg Config, log *slog.Logger) *Server {
	return &Server{st: st, thumbs: th, geo: gc, validator: v, provider: sp, cfg: cfg, log: log}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PublicPrefix+"/api/stats", s.stats)
	mux.HandleFunc("GET "+PublicPrefix+"/api/assets", s.assets)
	mux.HandleFunc("POST "+PublicPrefix+"/api/assets/{id}/favorite", s.favorite)
	mux.HandleFunc("POST "+PublicPrefix+"/api/assets/{id}/archive", s.archive)
	mux.HandleFunc("GET "+PublicPrefix+"/api/duplicates", s.duplicates)
	mux.HandleFunc("GET "+PublicPrefix+"/api/assets/{id}/thumb", s.thumbHandler)
	mux.HandleFunc("POST "+PublicPrefix+"/api/assets/{id}/video-url", s.videoURL)
	mux.HandleFunc("GET "+PublicPrefix+"/api/video/{id}", s.videoStream)
	mux.HandleFunc("GET "+PublicPrefix+"/api/assets/{id}/original", s.original)
	mux.HandleFunc("GET "+PublicPrefix+"/api/memories/on-this-day", s.onThisDay)
	mux.HandleFunc("GET "+PublicPrefix+"/api/timeline/calendar", s.calendar)
	mux.HandleFunc("GET "+PublicPrefix+"/api/memories/highlights", s.highlights)
	mux.HandleFunc("GET "+PublicPrefix+"/api/geo", s.geoHandler)
	mux.HandleFunc("GET "+PublicPrefix+"/api/places", s.places)
	mux.HandleFunc("GET "+PublicPrefix+"/api/tags", s.listTags)
	mux.HandleFunc("GET "+PublicPrefix+"/api/tags/{tag}/assets", s.tagAssets)
	mux.HandleFunc("GET "+PublicPrefix+"/api/assets/{id}/tags", s.assetTags)
	mux.HandleFunc("POST "+PublicPrefix+"/api/assets/{id}/tags", s.addTag)
	mux.HandleFunc("DELETE "+PublicPrefix+"/api/assets/{id}/tags/{tag}", s.removeTag)
	mux.HandleFunc("GET "+PublicPrefix+"/api/folders", s.folders)
	mux.HandleFunc("GET "+PublicPrefix+"/api/albums", s.listAlbums)
	mux.HandleFunc("POST "+PublicPrefix+"/api/albums", s.createAlbum)
	mux.HandleFunc("PATCH "+PublicPrefix+"/api/albums/{id}", s.renameAlbum)
	mux.HandleFunc("DELETE "+PublicPrefix+"/api/albums/{id}", s.deleteAlbum)
	mux.HandleFunc("GET "+PublicPrefix+"/api/albums/{id}/assets", s.albumAssets)
	mux.HandleFunc("POST "+PublicPrefix+"/api/albums/{id}/assets", s.addToAlbum)
	mux.HandleFunc("DELETE "+PublicPrefix+"/api/albums/{id}/assets/{assetId}", s.removeFromAlbum)
	mux.HandleFunc("POST "+PublicPrefix+"/api/admin/rescan", s.rescan)
	// CORS por fuera de auth: el preflight OPTIONS se responde 204 sin auth
	// (SPEC §4.4).
	return httpx.CORS(s.withAuth(mux))
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /api/video/ va firmado en la URL (el <video> no puede mandar cabeceras)
		if strings.HasPrefix(r.URL.Path, PublicPrefix+"/api/video/") {
			next.ServeHTTP(w, r)
			return
		}
		// 1) token estático propio DEPRECATED (OCAPPS_PHOTOS_TOKEN) — ANTES
		//    que Graph (§6.2). Solo tiene identidad porque el módulo exige
		//    OCAPPS_PHOTOS_USER configurado: mapea al oc_id legacy.
		if s.cfg.Token != "" && (r.Header.Get("Authorization") == "Bearer "+s.cfg.Token || r.URL.Query().Get("token") == s.cfg.Token) {
			owner, err := s.provider.LegacyOwner(r.Context())
			if err != nil {
				s.log.Warn("token estático: owner legacy no resoluble", "err", err)
				httpx.ErrorStatus(w, r, httpx.NewError(http.StatusServiceUnavailable, "owner legacy no disponible", "no se pudo resolver el oc_id del usuario legacy"))
				return
			}
			next.ServeHTTP(w, r.WithContext(withOwner(r.Context(), owner)))
			return
		}
		// 2) credencial del usuario (multi-tenant, H8): Bearer OIDC de la
		//    sesión web o Basic app-password, validados contra Graph por el
		//    validador común (caché + singleflight) con política MultiTenant.
		//    El owner es el oc_id resuelto; Touch marca la actividad (dispara
		//    el scan del usuario si toca).
		if s.validator != nil {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				cred := auth.Credential{Bearer: strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))}
				if u, ok := s.validator.Validate(r.Context(), cred); ok {
					s.provider.Touch(r.Context(), u, cred)
					next.ServeHTTP(w, r.WithContext(withOwner(r.Context(), u.ID)))
					return
				}
			}
			if u, p, ok := r.BasicAuth(); ok && u != "" {
				cred := auth.Credential{Username: u, Password: p}
				if usr, ok := s.validator.Validate(r.Context(), cred); ok {
					s.provider.Touch(r.Context(), usr, cred)
					next.ServeHTTP(w, r.WithContext(withOwner(r.Context(), usr.ID)))
					return
				}
			}
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="ocphotos"`)
		httpx.ErrorStatus(w, r, httpx.NewError(http.StatusUnauthorized, "unauthorized", "credenciales inválidas"))
	})
}

func writeJSON(w http.ResponseWriter, v any) { httpx.WriteJSON(w, http.StatusOK, v) }

// storeErr mapea los errores del store a HTTP. Regla IDOR (H8): sql.ErrNoRows
// (id inexistente O ajeno) → 404 "not found", sin delatar existencia.
func storeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), 500)
}

// davFor: cliente DAV de la sesión del owner; 503 si aún no hay sesión (el
// usuario se autenticó pero su sesión no está disponible, caso excepcional).
func (s *Server) davFor(w http.ResponseWriter, r *http.Request, owner string) *webdav.Client {
	dav := s.provider.DavFor(r.Context(), owner)
	if dav == nil {
		http.Error(w, "sesión del usuario no disponible", http.StatusServiceUnavailable)
	}
	return dav
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	st, err := s.st.Stats(r.Context(), OwnerFrom(r.Context()))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, st)
}

// GET /api/assets?before_taken=&before_id=&limit=&favorites=1&q=
func (s *Server) assets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	beforeTaken := int64(1<<62 - 1)
	beforeID := int64(1<<62 - 1)
	if v := q.Get("before_taken"); v != "" {
		beforeTaken, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := q.Get("before_id"); v != "" {
		beforeID, _ = strconv.ParseInt(v, 10, 64)
	}
	limit := 500
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	list, err := s.st.AssetsPage(r.Context(), OwnerFrom(r.Context()), beforeTaken, beforeID, limit, q.Get("favorites") == "1", q.Get("archived") == "1", q.Get("q"))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"assets": list})
}

func (s *Server) favorite(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Favorite bool `json:"favorite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if err := s.st.SetFavorite(r.Context(), OwnerFrom(r.Context()), id, body.Favorite); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) archive(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Archived bool `json:"archived"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if err := s.st.SetArchived(r.Context(), OwnerFrom(r.Context()), id, body.Archived); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// duplicates: calcula los hashes perceptuales que falten (acotado) y devuelve
// los grupos de fotos casi iguales (distancia de Hamming <= 8).
func (s *Server) duplicates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := OwnerFrom(ctx)
	pending, err := s.st.AssetsWithoutPHash(ctx, owner, 60)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	computed := 0
	for _, a := range pending {
		if err := s.computePHash(ctx, owner, a); err == nil {
			computed++
		}
	}
	hashes, err := s.st.AllPHashes(ctx, owner)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	groups := phash.Group(hashes, 8)
	out := make([][]store.Asset, 0, len(groups))
	for _, ids := range groups {
		list := make([]store.Asset, 0, len(ids))
		for _, id := range ids {
			if a, err := s.st.AssetByID(ctx, owner, id); err == nil {
				list = append(list, a)
			}
		}
		out = append(out, list)
	}
	writeJSON(w, map[string]any{"groups": out, "computed": computed, "pending": len(pending) - computed})
}

// --- Vídeo: URL firmada + streaming progresivo (Range) ---
// El elemento <video> no puede mandar el Bearer y la CSP del host bloquea blob:,
// así que se firma la URL (HMAC) y se sirve el original con soporte de Range.
// El secreto llega por Config.Secret (common/cred.LoadOrCreateSecret): si no se
// pudo cargar/generar, el módulo arranca failed (Q3 — sin fallback predecible).

func (s *Server) signVideo(id int64, exp int64) string {
	mac := hmac.New(sha256.New, s.cfg.Secret)
	fmt.Fprintf(mac, "%d|%d", id, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// videoURL: firma una URL de streaming para un vídeo (requiere sesión).
// Verifica ANTES de firmar que el asset es del owner del request (H8): la
// firma solo se genera tras la comprobación de ownership.
func (s *Server) videoURL(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := s.st.AssetByID(r.Context(), OwnerFrom(r.Context()), id)
	if err != nil || a.DeletedAt != nil {
		http.Error(w, "not found", 404)
		return
	}
	exp := time.Now().Add(6 * time.Hour).Unix()
	writeJSON(w, map[string]any{
		"url": fmt.Sprintf("%s/api/video/%d?exp=%d&sig=%s", PublicPrefix, id, exp, s.signVideo(id, exp)),
	})
}

// videoStream: sirve el vídeo con soporte de Range (validando la firma).
// CAPABILITY URL (H8): va sin sesión porque la firma HMAC ES la credencial;
// esa firma solo pudo generarse en videoURL tras verificar que el asset es
// del owner que la pidió. Aquí se resuelve el owner desde el id (los ids son
// globales) para servir con la sesión DAV de ese owner; no se expone nada que
// la capability no conceda ya.
func (s *Server) videoStream(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	exp, _ := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	sig := r.URL.Query().Get("sig")
	if exp < time.Now().Unix() || !hmac.Equal([]byte(sig), []byte(s.signVideo(id, exp))) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	owner, err := s.st.AssetOwner(r.Context(), id)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	a, err := s.st.AssetByID(r.Context(), owner, id)
	if err != nil || a.DeletedAt != nil {
		http.Error(w, "not found", 404)
		return
	}
	dav := s.davFor(w, r, owner)
	if dav == nil {
		return
	}
	rc, hdr, status, err := dav.DownloadRange(r.Context(), a.Path, r.Header.Get("Range"))
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	defer rc.Close()
	if ct := hdr.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	if cr := hdr.Get("Content-Range"); cr != "" {
		w.Header().Set("Content-Range", cr)
	}
	if cl := hdr.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.WriteHeader(status)
	_, _ = io.Copy(w, rc)
}

// computePHash: hash perceptual a partir de la miniatura en caché (no descarga el original).
func (s *Server) computePHash(ctx context.Context, owner string, a store.Asset) error {
	dav := s.provider.DavFor(ctx, owner)
	if dav == nil {
		return errors.New("sin sesión DAV del owner")
	}
	file, err := s.thumbs.Get(ctx, dav, a.Path, etagFor(a), 400)
	if err != nil {
		return err
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return err
	}
	return s.st.SetPHash(ctx, owner, a.ID, phash.DHash(img))
}

func (s *Server) thumbHandler(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	owner := OwnerFrom(r.Context())
	a, err := s.st.AssetByID(r.Context(), owner, id)
	if err != nil || a.DeletedAt != nil {
		http.Error(w, "not found", 404)
		return
	}
	dav := s.davFor(w, r, owner)
	if dav == nil {
		return
	}
	maxSize := 400
	if v := r.URL.Query().Get("w"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 64 && n <= 2048 {
			maxSize = n
		}
	}
	originalURL := PublicPrefix + "/api/assets/" + r.PathValue("id") + "/original"
	if a.MediaType == "video" {
		// póster del vídeo con ffmpeg; si falla, se sirve el original
		file, err := s.thumbs.VideoPoster(r.Context(), dav, a.Path, etagFor(a), maxSize)
		if err != nil {
			s.log.Warn("video poster", "id", id, "err", err)
			http.Redirect(w, r, originalURL, http.StatusFound)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
		http.ServeFile(w, r, file)
		return
	}
	path, err := s.thumbs.Get(r.Context(), dav, a.Path, etagFor(a), maxSize)
	if err != nil {
		s.log.Warn("thumb", "id", id, "err", err)
		http.Redirect(w, r, originalURL, http.StatusFound)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
	http.ServeFile(w, r, path)
}

// etagFor: clave de caché determinista sin exponer el etag en el JSON de la API.
func etagFor(a store.Asset) string {
	return strconv.FormatInt(a.TakenAt, 10) + "-" + strconv.FormatInt(a.Size, 10)
}

func (s *Server) original(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	owner := OwnerFrom(r.Context())
	a, err := s.st.AssetByID(r.Context(), owner, id)
	if err != nil || a.DeletedAt != nil {
		http.Error(w, "not found", 404)
		return
	}
	dav := s.davFor(w, r, owner)
	if dav == nil {
		return
	}
	rc, ct, err := dav.Download(r.Context(), a.Path)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	defer rc.Close()
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Disposition", `inline; filename="`+a.Filename+`"`)
	_, _ = io.Copy(w, rc)
}

func (s *Server) onThisDay(w http.ResponseWriter, r *http.Request) {
	// rango de días alrededor de hoy (±3 por defecto, como Memories)
	days := 3
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 30 {
			days = n
		}
	}
	now := time.Now()
	list, err := s.st.OnThisDay(r.Context(), OwnerFrom(r.Context()), int(now.Month()), now.Day(), now.Year(), days)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"assets": list, "days": days})
}

// highlights: contenido para la sección "On this day". Intenta, en orden:
// mismo día (años anteriores), mismo mes (años anteriores), las más antiguas.
func (s *Server) highlights(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	ctx := r.Context()
	owner := OwnerFrom(ctx)
	if list, err := s.st.OnThisDay(ctx, owner, int(now.Month()), now.Day(), now.Year(), 3); err == nil && len(list) > 0 {
		writeJSON(w, map[string]any{"scope": "day", "assets": list})
		return
	}
	if list, err := s.st.OnThisMonth(ctx, owner, int(now.Month()), now.Year(), 60); err == nil && len(list) > 0 {
		writeJSON(w, map[string]any{"scope": "month", "assets": list})
		return
	}
	list, err := s.st.OldestAssets(ctx, owner, 20)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"scope": "oldest", "assets": list})
}

func (s *Server) calendar(w http.ResponseWriter, r *http.Request) {
	years, err := s.st.Calendar(r.Context(), OwnerFrom(r.Context()))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"years": years})
}

func (s *Server) geoHandler(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.GeoAssets(r.Context(), OwnerFrom(r.Context()), 5000)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"assets": list})
}

// --- Lugares ---

// places: clusters de fotos con GPS, con nombre (geocodificación inversa cacheada).
// Geocodifica como mucho 3 sitios nuevos por petición (límite de Nominatim).
func (s *Server) places(w http.ResponseWriter, r *http.Request) {
	owner := OwnerFrom(r.Context())
	clusters, err := s.st.PlaceClusters(r.Context(), owner, 2)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	budget := 3
	for i := range clusters {
		if name, ok, _ := s.st.GetGeocode(r.Context(), clusters[i].Lat, clusters[i].Lon); ok {
			clusters[i].Name = name
			continue
		}
		if budget > 0 && s.geo != nil {
			if name, err := s.geo.Name(r.Context(), clusters[i].Lat, clusters[i].Lon, 2); err == nil {
				clusters[i].Name = name
				budget--
			}
		}
	}
	writeJSON(w, map[string]any{"places": clusters})
}

// --- Etiquetas ---

func (s *Server) listTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.st.ListTags(r.Context(), OwnerFrom(r.Context()))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"tags": tags})
}

func (s *Server) tagAssets(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	list, err := s.st.AssetsByTag(r.Context(), OwnerFrom(r.Context()), tag)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"assets": list})
}

func (s *Server) assetTags(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	tags, err := s.st.AssetTags(r.Context(), OwnerFrom(r.Context()), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"tags": tags})
}

func (s *Server) addTag(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Tag string `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Tag) == "" {
		http.Error(w, "tag required", 400)
		return
	}
	if err := s.st.AddTag(r.Context(), OwnerFrom(r.Context()), id, strings.TrimSpace(body.Tag)); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeTag(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.RemoveTag(r.Context(), OwnerFrom(r.Context()), id, r.PathValue("tag")); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Carpetas ---

// folders: subcarpetas y fotos de una carpeta (derivado del índice del owner).
// path = ruta relativa dentro del espacio ("" = raíz). El prefijo del espacio
// sale de la sesión del usuario (webDavUrl cacheada en el registry, H8); si
// aún no está resuelto se responde con vacío coherente (no 500).
func (s *Server) folders(w http.ResponseWriter, r *http.Request) {
	owner := OwnerFrom(r.Context())
	prefix := s.provider.SpacePrefix(r.Context(), owner)
	rel := strings.Trim(r.URL.Query().Get("path"), "/")
	if prefix == "" {
		// usuario sin espacio resuelto aún: respuesta vacía coherente
		writeJSON(w, map[string]any{"path": rel, "folders": []store.FolderEntry{}, "assets": []store.Asset{}})
		return
	}
	base := prefix + "/"
	if rel != "" {
		base += rel + "/"
	}

	folders, err := s.st.Subfolders(r.Context(), owner, base)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	assets, err := s.st.FolderAssets(r.Context(), owner, base)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	for i := range folders {
		folders[i].Path = strings.TrimPrefix(base+folders[i].Name, prefix+"/")
	}
	writeJSON(w, map[string]any{"path": rel, "folders": folders, "assets": assets})
}

// --- Álbumes ---

func (s *Server) listAlbums(w http.ResponseWriter, r *http.Request) {
	albums, err := s.st.ListAlbums(r.Context(), OwnerFrom(r.Context()))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"albums": albums})
}

func (s *Server) createAlbum(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		http.Error(w, "name required", 400)
		return
	}
	id, err := s.st.CreateAlbum(r.Context(), OwnerFrom(r.Context()), strings.TrimSpace(body.Name))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"id": id})
}

func (s *Server) renameAlbum(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		http.Error(w, "name required", 400)
		return
	}
	if err := s.st.RenameAlbum(r.Context(), OwnerFrom(r.Context()), id, strings.TrimSpace(body.Name)); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAlbum(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.DeleteAlbum(r.Context(), OwnerFrom(r.Context()), id); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) albumAssets(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	list, err := s.st.AlbumAssets(r.Context(), OwnerFrom(r.Context()), id)
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"assets": list})
}

func (s *Server) addToAlbum(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var body struct {
		AssetIDs []int64 `json:"assetIds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.AssetIDs) == 0 {
		http.Error(w, "assetIds required", 400)
		return
	}
	if err := s.st.AddToAlbum(r.Context(), OwnerFrom(r.Context()), id, body.AssetIDs); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeFromAlbum(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	assetID, _ := strconv.ParseInt(r.PathValue("assetId"), 10, 64)
	if err := s.st.RemoveFromAlbum(r.Context(), OwnerFrom(r.Context()), id, assetID); err != nil {
		storeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rescan: encola un scan SOLO del owner del request (H8: ya no hay scan global).
func (s *Server) rescan(w http.ResponseWriter, r *http.Request) {
	s.provider.RequestScan(OwnerFrom(r.Context()))
	writeJSON(w, map[string]any{"status": "rescan encolado"})
}
