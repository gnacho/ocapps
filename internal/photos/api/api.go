// Package api - REST consumed by the ocphotos extension under PublicPrefix
// (SPEC §4.3: the proxy no longer strips /ocphotos-api/). Multi-tenant
// (H8, SPEC §6.2): every OpenCloud user comes in with its OIDC Bearer
// (validated by the common middleware with the MultiTenant policy) or with a
// Basic app-password, the resolved *auth.User* is injected into the request
// context and ALL handlers scope to the owner (oc_id) with OwnerFrom. The
// static Bearer OCAPPS_PHOTOS_TOKEN is DEPRECATED: it only exists if
// OCAPPS_PHOTOS_USER is configured and it maps to the legacy owner (the oc_id
// of that user). The signed video routes are exempt (the HMAC signature goes in
// the URL: capability URL, see videoStream).
//
// Q7 (SPEC §7): the endpoints without a consumer in the extension (GET
// /api/thumb, GET /api/assets/{id}/hls/{file}, GET /api/assets/{id}) were removed
// in the port, the signed streaming /api/video/{id} is used and kept.
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
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/httpx"
	"github.com/gnacho/ocapps/internal/common/live"
	"github.com/gnacho/ocapps/internal/common/webdav"
	"github.com/gnacho/ocapps/internal/photos/exif"
	"github.com/gnacho/ocapps/internal/photos/geo"
	"github.com/gnacho/ocapps/internal/photos/phash"
	"github.com/gnacho/ocapps/internal/photos/store"
	"github.com/gnacho/ocapps/internal/photos/thumb"
	"github.com/gnacho/ocapps/internal/xmp"
)

// TopicStream is the topic of the stream events: "photos" when the index or the
// metadata of an asset changes, "albums" when the albums change.
const (
	TopicPhotos = "photos"
	TopicAlbums = "albums"
)

// PublicPrefix is the public namespace of the photos module (SPEC §4.3). It is
// used both in the route registration and in the signing of video URLs.
const PublicPrefix = "/ocphotos-api"

// Config: configuration dependencies of the API server.
type Config struct {
	Token  string // DEPRECATED static Bearer (OCAPPS_PHOTOS_TOKEN), "" = disabled
	Secret []byte // mediasecret to sign video URLs (cred.LoadOrCreateSecret, Q3)
}

// SessionProvider connects the API with the in-memory session registry of the
// module (H8 §6.2). The photos module implements it, the API does not know the
// registry to avoid an import cycle.
type SessionProvider interface {
	// Touch registers the owner activity with the credential of the request
	// (it creates or updates its in-memory session and triggers a scan when
	// due).
	Touch(ctx context.Context, u *auth.User, cred auth.Credential)
	// DavFor returns the DAV client of the owner session (nil if there is none).
	DavFor(ctx context.Context, owner string) *webdav.Client
	// SpacePrefix returns the path of the webDavUrl of the personal space of
	// the owner (folders handler), resolving it on demand with the credential
	// of its session when needed. "" if it cannot be resolved.
	SpacePrefix(ctx context.Context, owner string) string
	// SpacePrefixOf returns the path of the webDavUrl of a space of the owner
	// (folders handler of that space). "" if the space is unknown or not
	// visible (the handler answers with an empty listing).
	SpacePrefixOf(ctx context.Context, owner, spaceID string) string
	// LegacyOwner resolves the oc_id of the legacy user OCAPPS_PHOTOS_USER
	// (identity of the DEPRECATED static Bearer). Error if it cannot be
	// resolved (OpenCloud down, invalid credentials).
	LegacyOwner(ctx context.Context) (string, error)
	// RequestScan enqueues a scan of the owner of the request only.
	RequestScan(owner string)
}

// ownerKey is the private key of the owner in the request context (H8).
type ownerKey struct{}

// OwnerFrom returns the authenticated owner (oc_id) of the request, "" if there
// is none.
func OwnerFrom(ctx context.Context) string {
	owner, _ := ctx.Value(ownerKey{}).(string)
	return owner
}

// withOwner injects the owner into the context (used by withAuth after
// authenticating).
func withOwner(ctx context.Context, owner string) context.Context {
	return context.WithValue(ctx, ownerKey{}, owner)
}

type Server struct {
	st        *store.Store
	exif      *exif.Worker // optional: on-demand EXIF queue
	exifMu    sync.Mutex
	exifBusy  map[string]bool
	thumbs    *thumb.Service
	geo       *geo.Geocoder
	validator *auth.GraphValidator // nil = static token only
	provider  SessionProvider
	cfg       Config
	log       *slog.Logger
	hub       *live.Hub // optional: without a hub there is no stream (SetHub)
}

func New(st *store.Store, th *thumb.Service, gc *geo.Geocoder, v *auth.GraphValidator, sp SessionProvider, cfg Config, log *slog.Logger) *Server {
	return &Server{st: st, thumbs: th, geo: gc, validator: v, provider: sp, cfg: cfg, log: log,
		exifBusy: map[string]bool{}}
}

// SetExifWorker enables POST /api/admin/exif. Without a worker the route
// responds 503.
func (s *Server) SetExifWorker(w *exif.Worker) { s.exif = w }

// SetHub enables GET /api/stream and the publication of changes. Without a hub
// (tests, degraded module) the stream responds 503 and the mutations publish
// nothing.
func (s *Server) SetHub(h *live.Hub) { s.hub = h }

// publish notifies the streams of the owner that a topic has changed. The id of
// the client that made the change travels in the event so that tab does not
// reload what it already has.
func (s *Server) publish(r *http.Request, topics ...string) {
	s.publishOwner(OwnerFrom(r.Context()), live.ClientID(r), topics...)
}

// publishOwner publishes for a specific owner (background processes, for
// example the EXIF one, have no request). Without a hub it does nothing.
func (s *Server) publishOwner(owner, client string, topics ...string) {
	if s.hub == nil {
		return
	}
	for _, t := range topics {
		s.hub.Publish(owner, live.Event{Topic: t, Client: client})
	}
}

// stream is the invalidation channel (SSE) of the authenticated owner.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	if s.hub == nil {
		http.Error(w, "stream not available", http.StatusServiceUnavailable)
		return
	}
	s.hub.Serve(w, r, OwnerFrom(r.Context()))
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PublicPrefix+"/api/stats", s.stats)
	mux.HandleFunc("GET "+PublicPrefix+"/api/assets", s.assets)
	mux.HandleFunc("GET "+PublicPrefix+"/api/spaces", s.spaces)
	mux.HandleFunc("GET "+PublicPrefix+"/api/assets/resolve", s.resolve)
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
	mux.HandleFunc("POST "+PublicPrefix+"/api/admin/exif", s.exifBacklog)
	// Invalidation channel (SSE): the client stops asking every N seconds and
	// reloads when the server notifies it.
	mux.HandleFunc("GET "+PublicPrefix+"/api/stream", s.stream)
	// CORS outside auth: the OPTIONS preflight is answered 204 with no auth
	// (SPEC §4.4).
	return httpx.CORS(s.withAuth(mux))
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /api/video/ is signed in the URL (the <video> element cannot send headers)
		if strings.HasPrefix(r.URL.Path, PublicPrefix+"/api/video/") {
			next.ServeHTTP(w, r)
			return
		}
		// 1) own DEPRECATED static token (OCAPPS_PHOTOS_TOKEN) - BEFORE
		//    Graph (§6.2). It only has an identity because the module
		//    requires OCAPPS_PHOTOS_USER to be configured: it maps to the
		//    legacy oc_id.
		if s.cfg.Token != "" && (r.Header.Get("Authorization") == "Bearer "+s.cfg.Token || r.URL.Query().Get("token") == s.cfg.Token) {
			owner, err := s.provider.LegacyOwner(r.Context())
			if err != nil {
				s.log.Warn("static token: legacy owner not resolvable", "err", err)
				httpx.ErrorStatus(w, r, httpx.NewError(http.StatusServiceUnavailable, "legacy owner not available", "could not resolve the oc_id of the legacy user"))
				return
			}
			next.ServeHTTP(w, r.WithContext(withOwner(r.Context(), owner)))
			return
		}
		// 2) user credential (multi-tenant, H8): OIDC Bearer of the web
		//    session or Basic app-password, validated against Graph by the
		//    common validator (cache + singleflight) with the MultiTenant
		//    policy. The owner is the resolved oc_id, Touch marks the
		//    activity (it triggers the user scan when due).
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
		httpx.ErrorStatus(w, r, httpx.NewError(http.StatusUnauthorized, "unauthorized", "invalid credentials"))
	})
}

func writeJSON(w http.ResponseWriter, v any) { httpx.WriteJSON(w, http.StatusOK, v) }

// storeErr maps the store errors to HTTP. IDOR rule (H8): sql.ErrNoRows (id
// that does not exist OR foreign) -> 404 "not found", without leaking existence.
func storeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), 500)
}

// davFor: DAV client of the owner session, 503 if there is no session yet (the
// user authenticated but its session is not available, an exceptional case).
func (s *Server) davFor(w http.ResponseWriter, r *http.Request, owner string) *webdav.Client {
	dav := s.provider.DavFor(r.Context(), owner)
	if dav == nil {
		http.Error(w, "user session not available", http.StatusServiceUnavailable)
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

// GET /api/assets?before_taken=&before_id=&limit=&favorites=1&q=&space=
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
	list, err := s.st.AssetsPage(r.Context(), OwnerFrom(r.Context()), beforeTaken, beforeID, limit, q.Get("favorites") == "1", q.Get("archived") == "1", q.Get("q"), q.Get("space"))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"assets": list})
}

// GET /api/spaces: the spaces (drives) with indexed photos of the owner, with
// their count. It is what feeds the space filter of the UI (issue #145).
func (s *Server) spaces(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.Spaces(r.Context(), OwnerFrom(r.Context()))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"spaces": list})
}

// resolve implements GET /api/assets/resolve, the destination of the "Open
// with" flow of Files (gnacho/ocphotos #17): the host navigates to the route of
// the extension with driveAliasAndItem (<alias><relative path>, for example
// "personal$<uuid>/Fotos/a.jpg") and fileId. The indexed path is the absolute
// DAV href, "/dav/spaces/" + driveAliasAndItem, so the resolution is a direct
// lookup by (owner, path) and works with the existing indexes (no rescan).
// fileId is accepted but not used yet: indexing the oc:id would require a schema
// migration and a rescan, it is left for when the path is not enough. Not
// indexed or foreign -> {"asset": null} (200), the honest state the extension
// shows as "Photo not found".
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	dai := r.URL.Query().Get("driveAliasAndItem")
	if dai == "" {
		http.Error(w, "driveAliasAndItem required", http.StatusBadRequest)
		return
	}
	unescaped, err := url.PathUnescape(dai)
	if err != nil {
		http.Error(w, "invalid driveAliasAndItem", http.StatusBadRequest)
		return
	}
	p := unescaped
	if !strings.HasPrefix(p, "/dav/") {
		p = "/dav/spaces/" + strings.TrimPrefix(p, "/")
	}
	asset, err := s.st.AssetByPath(r.Context(), OwnerFrom(r.Context()), p)
	if errors.Is(err, sql.ErrNoRows) {
		asset, err = s.resolveByFileID(r, unescaped)
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, map[string]any{"asset": nil})
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"asset": asset})
}

// resolveByFileID is the fallback when the alias comes in the form
// <type>/<name>/<path> (OpenCloud 8.0.1): fileId = storageId$spaceId!opaqueId
// and the indexed path contains "$<spaceId>/<path>", so the spaceId and the
// relative path (ALREADY unescaped) are extracted and the lookup uses both.
// E2E drive.domatix.cloud:
// personal/uitest/Fotos/IMG_x.jpg + 7d443b01…$7b251a66…!41b22c35…
func (s *Server) resolveByFileID(r *http.Request, unescaped string) (store.Asset, error) {
	fileID := r.URL.Query().Get("fileId")
	dollar := strings.Index(fileID, "$")
	bang := strings.Index(fileID, "!")
	if dollar < 0 {
		return store.Asset{}, sql.ErrNoRows
	}
	spaceID := fileID[dollar+1:]
	if bang > dollar {
		spaceID = fileID[dollar+1 : bang]
	}
	if spaceID == "" {
		return store.Asset{}, sql.ErrNoRows
	}
	itemPath := itemPathFromAlias(unescaped)
	if itemPath == "" {
		return store.Asset{}, sql.ErrNoRows
	}
	return s.st.AssetBySpaceItem(r.Context(), OwnerFrom(r.Context()), spaceID, itemPath)
}

// itemPathFromAlias extracts the relative path of the item from a
// driveAliasAndItem: "personal$uuid/Fotos/x.jpg" -> "Fotos/x.jpg" (1 alias
// segment) and "personal/uitest/Fotos/x.jpg" -> "Fotos/x.jpg" (2 segments, no
// "$").
func itemPathFromAlias(dai string) string {
	rest := strings.TrimPrefix(dai, "/")
	first, rest, found := strings.Cut(rest, "/")
	if !found {
		return ""
	}
	if !strings.Contains(first, "$") {
		// form type/name: skip the name segment too
		_, rest, found = strings.Cut(rest, "/")
		if !found {
			return ""
		}
	}
	return rest
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
	s.writeXMP(r.Context(), OwnerFrom(r.Context()), id)
	s.publish(r, TopicPhotos)
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
	s.publish(r, TopicPhotos)
	w.WriteHeader(http.StatusNoContent)
}

// duplicates: computes the missing perceptual hashes (bounded) and returns the
// groups of nearly identical photos (Hamming distance <= 8).
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

// --- Video: signed URL plus progressive streaming (Range) ---
// The <video> element cannot send the Bearer and the host CSP blocks blob:, so
// the URL is signed (HMAC) and the original is served with Range support. The
// secret comes from Config.Secret (common/cred.LoadOrCreateSecret): if it could
// not be loaded or generated, the module starts failed (Q3 - no predictable
// fallback).

func (s *Server) signVideo(id int64, exp int64) string {
	mac := hmac.New(sha256.New, s.cfg.Secret)
	_, _ = fmt.Fprintf(mac, "%d|%d", id, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// videoURL: signs a streaming URL for a video (requires a session). It verifies
// BEFORE signing that the asset belongs to the owner of the request (H8): the
// signature is only generated after the ownership check.
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

// videoStream: serves the video with Range support (validating the signature).
// CAPABILITY URL (H8): it carries no session because the HMAC signature IS the
// credential, that signature could only be generated in videoURL after
// verifying that the asset belongs to the owner that asked for it. Here the
// owner is resolved from the id (ids are global) to serve with the DAV session
// of that owner, nothing is exposed that the capability does not grant already.
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
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = rc.Close() }()
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

// computePHash: perceptual hash from the cached thumbnail (it does not download
// the original).
func (s *Server) computePHash(ctx context.Context, owner string, a store.Asset) error {
	dav := s.provider.DavFor(ctx, owner)
	if dav == nil {
		return errors.New("no DAV session for the owner")
	}
	file, err := s.thumbs.Get(ctx, dav, a.Path, etagFor(a), 400)
	if err != nil {
		return err
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
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
	// Browser cache policy for thumbnails and posters: revalidable
	// (ETag + 304), not immutable. With "immutable" for 30 days, a fix in the
	// GENERATION of thumbnails never reaches the client: the URL does not
	// change and the browser does not ask for 30 days (old Thumbs stay badly
	// oriented for a month). With max-age=86400 + must-revalidate the browser
	// revalidates daily and the ETag (derived from the cache key, with the same
	// salt) changes whenever the generation changes: self-healing.
	const thumbCacheControl = "public, max-age=86400, must-revalidate"
	// notModified responds 304 with the validator if If-None-Match matches,
	// it returns true when it has already written the response.
	notModified := func(etag string) bool {
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", thumbCacheControl)
		if etag != "" && strings.Contains(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
		return false
	}
	originalURL := PublicPrefix + "/api/assets/" + r.PathValue("id") + "/original"
	if a.MediaType == "video" {
		// video poster with ffmpeg, on failure the original is served
		etag := thumb.VideoETag(a.Path, etagFor(a), maxSize)
		if notModified(etag) {
			return
		}
		file, err := s.thumbs.VideoPoster(r.Context(), dav, a.Path, etagFor(a), maxSize)
		if err != nil {
			s.log.Warn("video poster", "id", id, "err", err)
			http.Redirect(w, r, originalURL, http.StatusFound)
			return
		}
		http.ServeFile(w, r, file)
		return
	}
	etag := thumb.ETag(a.Path, etagFor(a), maxSize)
	if notModified(etag) {
		return
	}
	path, err := s.thumbs.Get(r.Context(), dav, a.Path, etagFor(a), maxSize)
	if err != nil {
		s.log.Warn("thumb", "id", id, "err", err)
		http.Redirect(w, r, originalURL, http.StatusFound)
		return
	}
	http.ServeFile(w, r, path)
}

// etagFor: deterministic cache key without exposing the etag in the API JSON.
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
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = rc.Close() }()
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Disposition", `inline; filename="`+a.Filename+`"`)
	_, _ = io.Copy(w, rc)
}

func (s *Server) onThisDay(w http.ResponseWriter, r *http.Request) {
	// range of days around today (+-3 by default, like Memories)
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

// highlights: content for the "On this day" section. It tries, in order: the
// same day (previous years), the same month (previous years), the oldest ones.
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

// --- Places ---

// places: clusters of photos with GPS, with a name (cached reverse geocoding).
// It geocodes at most 3 new places per request (Nominatim limit).
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

// --- Tags ---

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
	s.writeXMP(r.Context(), OwnerFrom(r.Context()), id)
	s.publish(r, TopicPhotos)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeTag(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.RemoveTag(r.Context(), OwnerFrom(r.Context()), id, r.PathValue("tag")); err != nil {
		storeErr(w, err)
		return
	}
	s.writeXMP(r.Context(), OwnerFrom(r.Context()), id)
	s.publish(r, TopicPhotos)
	w.WriteHeader(http.StatusNoContent)
}

// writeXMP projects the tags and the favourite flag of the asset into a .xmp
// sidecar next to the photo, so that they survive outside the index (reindexing,
// path change or loss of the database). The index is still the source of truth:
// the sidecar is derived state and its write is best effort, so a failure is
// only logged and never brings down the main operation.
func (s *Server) writeXMP(ctx context.Context, owner string, id int64) {
	a, err := s.st.AssetByID(ctx, owner, id)
	if err != nil {
		s.log.Warn("xmp: asset not found", "owner", owner, "id", id, "err", err)
		return
	}
	tags, err := s.st.AssetTags(ctx, owner, id)
	if err != nil {
		s.log.Warn("xmp: could not read the tags", "owner", owner, "id", id, "err", err)
		return
	}
	dav := s.provider.DavFor(ctx, owner)
	if dav == nil {
		s.log.Warn("xmp: no DAV session for the owner", "owner", owner)
		return
	}
	href := xmp.SidecarPath(a.Path)
	// Own context: the HTTP response may finish before the write.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := dav.Put(cctx, href, "application/rdf+xml", xmp.Build(xmp.Meta{Tags: tags, Favorite: a.IsFavorite})); err != nil {
		s.log.Warn("xmp: could not write the sidecar", "owner", owner, "path", href, "err", err)
	}
}

// --- Folders ---

// folders: subfolders and photos of a folder (derived from the owner index).
// path = relative path inside the space ("" = root) and space = drive id ("" =
// the personal one, kept for compatibility with a single-space client). The
// prefix of the space comes from the user session (webDavUrl cached in the
// registry, H8), if it is not resolved yet a coherent empty answer is returned
// (not a 500).
func (s *Server) folders(w http.ResponseWriter, r *http.Request) {
	owner := OwnerFrom(r.Context())
	spaceID := r.URL.Query().Get("space")
	prefix := ""
	if spaceID != "" {
		prefix = s.provider.SpacePrefixOf(r.Context(), owner, spaceID)
	} else {
		prefix = s.provider.SpacePrefix(r.Context(), owner)
	}
	rel := strings.Trim(r.URL.Query().Get("path"), "/")
	if prefix == "" {
		// user without a resolved space yet (or a space not visible): coherent
		// empty answer
		writeJSON(w, map[string]any{"path": rel, "space": spaceID, "folders": []store.FolderEntry{}, "assets": []store.Asset{}})
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
	writeJSON(w, map[string]any{"path": rel, "space": spaceID, "folders": folders, "assets": assets})
}

// --- Albums ---

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
	s.publish(r, TopicAlbums)
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
	s.publish(r, TopicAlbums)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAlbum(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.st.DeleteAlbum(r.Context(), OwnerFrom(r.Context()), id); err != nil {
		storeErr(w, err)
		return
	}
	s.publish(r, TopicAlbums)
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
	s.publish(r, TopicAlbums)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeFromAlbum(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	assetID, _ := strconv.ParseInt(r.PathValue("assetId"), 10, 64)
	if err := s.st.RemoveFromAlbum(r.Context(), OwnerFrom(r.Context()), id, assetID); err != nil {
		storeErr(w, err)
		return
	}
	s.publish(r, TopicAlbums)
	w.WriteHeader(http.StatusNoContent)
}

// exifBacklog processes the pending EXIF queue of the owner in the background.
// It is needed because the normal worker only runs AFTER a full scan: in large
// libraries the scans chain up and the queue never advances.
func (s *Server) exifBacklog(w http.ResponseWriter, r *http.Request) {
	owner := OwnerFrom(r.Context())
	if s.exif == nil {
		http.Error(w, "exif not available", http.StatusServiceUnavailable)
		return
	}
	s.exifMu.Lock()
	if s.exifBusy[owner] {
		s.exifMu.Unlock()
		httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"status": "exif already running"})
		return
	}
	if s.exifBusy == nil {
		s.exifBusy = map[string]bool{}
	}
	s.exifBusy[owner] = true
	s.exifMu.Unlock()

	dav := s.provider.DavFor(r.Context(), owner)
	if dav == nil {
		s.exifMu.Lock()
		delete(s.exifBusy, owner)
		s.exifMu.Unlock()
		http.Error(w, "user session not available", http.StatusServiceUnavailable)
		return
	}

	go func() {
		defer func() {
			s.exifMu.Lock()
			delete(s.exifBusy, owner)
			s.exifMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		if err := s.exif.Run(ctx, owner, dav); err != nil {
			s.log.Warn("exif backlog", "owner", owner, "err", err)
		}
		// The dates and the GPS change: let the open tabs reload.
		s.publishOwner(owner, "", TopicPhotos)
	}()
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"status": "exif queued"})
}

// rescan: enqueues a scan OF THE OWNER of the request only (H8: there is no
// global scan any more).
func (s *Server) rescan(w http.ResponseWriter, r *http.Request) {
	s.provider.RequestScan(OwnerFrom(r.Context()))
	writeJSON(w, map[string]any{"status": "rescan queued"})
}
