// Package api — REST consumida por la extensión ocphotos bajo PublicPrefix
// (SPEC §4.3: el proxy deja de strip-pear /ocphotos-api/). Single-tenant:
// hacia fuera se protege con un Bearer estático propio (OCAPPS_PHOTOS_TOKEN,
// comprobado ANTES que Graph) o con la sesión de OpenCloud validada por el
// middleware común con política SingleTenant (SPEC §6.2). Las rutas firmadas
// de vídeo están exentas (la firma HMAC va en la URL).
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	"github.com/gnacho/ocapps/internal/photos/video"
)

// PublicPrefix es el namespace público del módulo photos (SPEC §4.3). Se usa
// tanto en el registro de rutas como en la firma de URLs de vídeo.
const PublicPrefix = "/ocphotos-api"

// Config: dependencias de configuración del servidor API.
type Config struct {
	WebDAVURL string // webDavUrl del espacio personal (handler de carpetas)
	Token     string // Bearer estático propio (OCAPPS_PHOTOS_TOKEN); "" = desactivado
	Secret    []byte // mediasecret para firmar URLs de vídeo (cred.LoadOrCreateSecret; Q3)
}

type Server struct {
	st        *store.Store
	thumbs    *thumb.Service
	dav       *webdav.Client
	geo       *geo.Geocoder
	video     *video.Transcoder
	validator *auth.GraphValidator // nil = solo token estático
	cfg       Config
	log       *slog.Logger
	rescanCh  chan struct{}
}

func New(st *store.Store, th *thumb.Service, dc *webdav.Client, gc *geo.Geocoder, vt *video.Transcoder, v *auth.GraphValidator, cfg Config, log *slog.Logger) *Server {
	return &Server{
		st: st, thumbs: th, dav: dc, geo: gc, video: vt, validator: v, cfg: cfg, log: log,
		rescanCh: make(chan struct{}, 4), // Q5: buffer 4 (antes 1)
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PublicPrefix+"/api/stats", s.stats)
	mux.HandleFunc("GET "+PublicPrefix+"/api/assets", s.assets)
	mux.HandleFunc("GET "+PublicPrefix+"/api/assets/{id}", s.asset)
	mux.HandleFunc("POST "+PublicPrefix+"/api/assets/{id}/favorite", s.favorite)
	mux.HandleFunc("POST "+PublicPrefix+"/api/assets/{id}/archive", s.archive)
	mux.HandleFunc("GET "+PublicPrefix+"/api/duplicates", s.duplicates)
	mux.HandleFunc("GET "+PublicPrefix+"/api/assets/{id}/thumb", s.thumbHandler)
	mux.HandleFunc("GET "+PublicPrefix+"/api/assets/{id}/hls/{file}", s.hls)
	mux.HandleFunc("POST "+PublicPrefix+"/api/assets/{id}/video-url", s.videoURL)
	mux.HandleFunc("GET "+PublicPrefix+"/api/video/{id}", s.videoStream)
	mux.HandleFunc("GET "+PublicPrefix+"/api/thumb", s.thumbByPath)
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
		// 1) token estático propio (OCAPPS_PHOTOS_TOKEN) — ANTES que Graph (§6.2)
		if s.cfg.Token != "" && (r.Header.Get("Authorization") == "Bearer "+s.cfg.Token || r.URL.Query().Get("token") == s.cfg.Token) {
			next.ServeHTTP(w, r)
			return
		}
		// 2) sesión de OpenCloud (la usa la extensión web): Bearer validado
		//    contra Graph por el validador común (caché + singleflight) con la
		//    política single-tenant configurada en el módulo.
		if s.validator != nil {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				if _, ok := s.validator.Validate(r.Context(), auth.Credential{Bearer: strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))}); ok {
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="ocphotos"`)
		httpx.ErrorStatus(w, r, httpx.NewError(http.StatusUnauthorized, "unauthorized", "credenciales inválidas"))
	})
}

func writeJSON(w http.ResponseWriter, v any) { httpx.WriteJSON(w, http.StatusOK, v) }

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	st, err := s.st.Stats(r.Context())
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
	list, err := s.st.AssetsPage(r.Context(), beforeTaken, beforeID, limit, q.Get("favorites") == "1", q.Get("archived") == "1", q.Get("q"))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"assets": list})
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := s.st.AssetByID(r.Context(), id)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	writeJSON(w, a)
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
	if err := s.st.SetFavorite(r.Context(), id, body.Favorite); err != nil {
		http.Error(w, err.Error(), 500)
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
	if err := s.st.SetArchived(r.Context(), id, body.Archived); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// duplicates: calcula los hashes perceptuales que falten (acotado) y devuelve
// los grupos de fotos casi iguales (distancia de Hamming <= 8).
func (s *Server) duplicates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pending, err := s.st.AssetsWithoutPHash(ctx, 60)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	computed := 0
	for _, a := range pending {
		if err := s.computePHash(ctx, a); err == nil {
			computed++
		}
	}
	hashes, err := s.st.AllPHashes(ctx)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	groups := phash.Group(hashes, 8)
	out := make([][]store.Asset, 0, len(groups))
	for _, ids := range groups {
		list := make([]store.Asset, 0, len(ids))
		for _, id := range ids {
			if a, err := s.st.AssetByID(ctx, id); err == nil {
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
func (s *Server) videoURL(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	exp := time.Now().Add(6 * time.Hour).Unix()
	writeJSON(w, map[string]any{
		"url": fmt.Sprintf("%s/api/video/%d?exp=%d&sig=%s", PublicPrefix, id, exp, s.signVideo(id, exp)),
	})
}

// videoStream: sirve el vídeo con soporte de Range (validando la firma).
func (s *Server) videoStream(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	exp, _ := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	sig := r.URL.Query().Get("sig")
	if exp < time.Now().Unix() || !hmac.Equal([]byte(sig), []byte(s.signVideo(id, exp))) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	a, err := s.st.AssetByID(r.Context(), id)
	if err != nil || a.DeletedAt != nil {
		http.Error(w, "not found", 404)
		return
	}
	rc, hdr, status, err := s.dav.DownloadRange(r.Context(), a.Path, r.Header.Get("Range"))
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

// hls: sirve la playlist y los segmentos HLS de un vídeo (transcodifica bajo demanda).
func (s *Server) hls(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := s.st.AssetByID(r.Context(), id)
	if err != nil || a.DeletedAt != nil {
		http.Error(w, "not found", 404)
		return
	}
	file := r.PathValue("file")
	if file != filepath.Base(file) || (!strings.HasSuffix(file, ".m3u8") && !strings.HasSuffix(file, ".ts")) {
		http.Error(w, "bad request", 400)
		return
	}
	if s.video == nil {
		http.Error(w, "transcoding no disponible", 501)
		return
	}
	dir, err := s.video.HLS(r.Context(), a.Path, etagFor(a))
	if err != nil {
		s.log.Warn("hls", "id", id, "err", err)
		http.Error(w, err.Error(), 502)
		return
	}
	if strings.HasSuffix(file, ".m3u8") {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	} else {
		w.Header().Set("Content-Type", "video/mp2t")
	}
	w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
	http.ServeFile(w, r, filepath.Join(dir, file))
}

// thumbByPath genera una miniatura a partir de la ruta del fichero (sin índice),
// de modo que la extensión puede pedir miniaturas de HEIC que OpenCloud no sabe
// previsualizar. path = ruta dentro del espacio ("/Fotos/IMG.heic").
func (s *Server) thumbByPath(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}
	maxSize := 400
	if v := r.URL.Query().Get("w"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 64 && n <= 2048 {
			maxSize = n
		}
	}
	href := s.dav.SpaceFileURL(s.cfg.WebDAVURL, p)
	file, err := s.thumbs.Get(r.Context(), href, r.URL.Query().Get("etag"), maxSize)
	if err != nil {
		s.log.Warn("thumb by path", "path", p, "err", err)
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
	http.ServeFile(w, r, file)
}

// computePHash: hash perceptual a partir de la miniatura en caché (no descarga el original).
func (s *Server) computePHash(ctx context.Context, a store.Asset) error {
	file, err := s.thumbs.Get(ctx, a.Path, etagFor(a), 400)
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
	return s.st.SetPHash(ctx, a.ID, phash.DHash(img))
}

func (s *Server) thumbHandler(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	a, err := s.st.AssetByID(r.Context(), id)
	if err != nil || a.DeletedAt != nil {
		http.Error(w, "not found", 404)
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
		file, err := s.thumbs.VideoPoster(r.Context(), a.Path, etagFor(a), maxSize)
		if err != nil {
			s.log.Warn("video poster", "id", id, "err", err)
			http.Redirect(w, r, originalURL, http.StatusFound)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
		http.ServeFile(w, r, file)
		return
	}
	path, err := s.thumbs.Get(r.Context(), a.Path, etagFor(a), maxSize)
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
	a, err := s.st.AssetByID(r.Context(), id)
	if err != nil || a.DeletedAt != nil {
		http.Error(w, "not found", 404)
		return
	}
	rc, ct, err := s.dav.Download(r.Context(), a.Path)
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
	list, err := s.st.OnThisDay(r.Context(), int(now.Month()), now.Day(), now.Year(), days)
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
	if list, err := s.st.OnThisDay(ctx, int(now.Month()), now.Day(), now.Year(), 3); err == nil && len(list) > 0 {
		writeJSON(w, map[string]any{"scope": "day", "assets": list})
		return
	}
	if list, err := s.st.OnThisMonth(ctx, int(now.Month()), now.Year(), 60); err == nil && len(list) > 0 {
		writeJSON(w, map[string]any{"scope": "month", "assets": list})
		return
	}
	list, err := s.st.OldestAssets(ctx, 20)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"scope": "oldest", "assets": list})
}

func (s *Server) calendar(w http.ResponseWriter, r *http.Request) {
	years, err := s.st.Calendar(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"years": years})
}

func (s *Server) geoHandler(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.GeoAssets(r.Context(), 5000)
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
	clusters, err := s.st.PlaceClusters(r.Context(), 2)
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
	tags, err := s.st.ListTags(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"tags": tags})
}

func (s *Server) tagAssets(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	list, err := s.st.AssetsByTag(r.Context(), tag)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"assets": list})
}

func (s *Server) assetTags(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	tags, err := s.st.AssetTags(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 500)
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
	if err := s.st.AddTag(r.Context(), id, strings.TrimSpace(body.Tag)); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeTag(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.RemoveTag(r.Context(), id, r.PathValue("tag")); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Carpetas ---

// folders: subcarpetas y fotos de una carpeta (derivado del índice).
// path = ruta relativa dentro del espacio ("" = raíz).
func (s *Server) folders(w http.ResponseWriter, r *http.Request) {
	prefix := ""
	if u, err := url.Parse(s.cfg.WebDAVURL); err == nil {
		prefix = strings.TrimRight(u.Path, "/")
	}
	rel := strings.Trim(r.URL.Query().Get("path"), "/")
	base := prefix + "/"
	if rel != "" {
		base += rel + "/"
	}

	folders, err := s.st.Subfolders(r.Context(), base)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	assets, err := s.st.FolderAssets(r.Context(), base)
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
	albums, err := s.st.ListAlbums(r.Context())
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
	id, err := s.st.CreateAlbum(r.Context(), strings.TrimSpace(body.Name))
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
	if err := s.st.RenameAlbum(r.Context(), id, strings.TrimSpace(body.Name)); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAlbum(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.DeleteAlbum(r.Context(), id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) albumAssets(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	list, err := s.st.AlbumAssets(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 500)
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
	if err := s.st.AddToAlbum(r.Context(), id, body.AssetIDs); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeFromAlbum(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	assetID, _ := strconv.ParseInt(r.PathValue("assetId"), 10, 64)
	if err := s.st.RemoveFromAlbum(r.Context(), id, assetID); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) rescan(w http.ResponseWriter, _ *http.Request) {
	select {
	case s.rescanCh <- struct{}{}:
		writeJSON(w, map[string]any{"status": "rescan encolado"})
	default:
		// Q5: con buffer 4 los drops son excepcionales, pero se loguean.
		s.log.Warn("rescan ignorado: cola llena")
		writeJSON(w, map[string]any{"status": "ya hay un scan en curso"})
	}
}

// RescanRequests devuelve el canal para que el módulo lance scans bajo demanda.
func (s *Server) RescanRequests() <-chan struct{} { return s.rescanCh }
