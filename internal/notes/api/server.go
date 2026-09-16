// Package api: servidor de la API Nextcloud Notes (v0.2/v1.4) + OCS, portado
// a ocapps sobre los paquetes comunes (SPEC §4.1, §6). El contrato externo
// (rutas, métodos, códigos, formatos y cabeceras propias como
// X-Notes-API-Versions, X-Notes-Chunk-Cursor, ETag/If-Match y OCS) se conserva
// intacto; CORS y auth usan los middlewares unificados (SPEC §4.4, §6.3).
package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/httpx"
	"github.com/gnacho/ocapps/internal/common/imgproxy"
	"github.com/gnacho/ocapps/internal/notes/attachments"
	"github.com/gnacho/ocapps/internal/notes/store"
)

const (
	Base               = "/index.php/apps/notes/api/v1/"
	Base14             = "/index.php/apps/notes/api/v1.4/"
	APIVersionsHeader  = "X-Notes-API-Versions"
	AllowedAPIVersions = "0.2, 1.4"
	Version            = "6.0.0"
)

// maxRequestBytes caps the multipart body: the 32 MiB file cap plus 1 MiB of
// multipart overhead. Anything larger is rejected with 413 before parsing.
const maxRequestBytes = attachments.MaxAttachmentBytes + 1<<20

type noteKeyType struct{}

// currentUser returns the authenticated user's stable id (graph id) from the
// request context, set by middleware. It is the scoping key for notes, settings
// and attachments; it must not be the username, which may be empty.
func currentUser(r *http.Request) string {
	if u, ok := r.Context().Value(noteKeyType{}).(*auth.User); ok && u != nil {
		return u.ID
	}
	return ""
}

type prunedNote struct {
	ID int64 `json:"id"`
}

// jsonBool decodes a JSON value that may be a boolean (true/false) or an
// integer (1/0), as Iotas sends favorite as 1|0 while the Notes API uses
// booleans. A null value leaves Set false, meaning "do not change".
type jsonBool struct {
	Set   bool
	Value bool
}

func (b *jsonBool) UnmarshalJSON(data []byte) error {
	switch strings.TrimSpace(string(data)) {
	case "null":
		b.Set = false
		return nil
	case "true", "1":
		b.Set, b.Value = true, true
		return nil
	case "false", "0":
		b.Set, b.Value = true, false
		return nil
	}
	return fmt.Errorf("invalid favorite value %s", string(data))
}

type Server struct {
	base        string
	store       *store.Store
	validator   *auth.GraphValidator
	images      *imgproxy.Proxy
	attachments *attachments.Store
}

func NewServer(base string, s *store.Store, v *auth.GraphValidator, images *imgproxy.Proxy, attachments *attachments.Store) *Server {
	return &Server{base: base, store: s, validator: v, images: images, attachments: attachments}
}

func (s *Server) Router() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc(s.base+"notes", s.handleNotes)
	mux.HandleFunc(s.base+"notes/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, s.base), "/")
		if len(parts) >= 2 && parts[0] == "notes" && parts[1] != "" {
			idStr := parts[1]
			s.handleNoteByID(w, r, idStr)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc(s.base+"settings", s.handleSettings)
	mux.HandleFunc(s.base+"img/sign", s.handleImgSign)
	mux.HandleFunc(Base14+"attachment/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, Base14), "/")
		if len(parts) >= 2 && parts[0] == "attachment" && parts[1] != "" {
			s.handleAttachment(w, r, parts[1])
			return
		}
		http.NotFound(w, r)
	})

	// OCS endpoints for clients (capabilities, user info)
	ocsMux := http.NewServeMux()
	ocsMux.HandleFunc("/ocs/v2.php/cloud/capabilities", s.handleCapabilities)
	ocsMux.HandleFunc("/ocs/v2.php/cloud/user", s.handleUserInfo)

	// Cadena unificada de la API (SPEC §4.4, §6.3): cabecera de versiones
	// propia → CORS unificado (preflight OPTIONS → 204 sin auth) → middleware
	// de auth común → inyección del shadow user en la clave de contexto de
	// notes (escopado por graph ID en la columna `user`, como siempre).
	apiHandler := s.withAPIVersions(httpx.CORS(auth.Middleware(s.validator, s.shadowUser(mux))))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// img: ruta PÚBLICA firmada (el <img> del navegador no lleva auth)
		if s.images != nil && r.URL.Path == s.base+"img" {
			s.images.Serve(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/ocs/v2.php/") {
			ocsMux.ServeHTTP(w, r)
			return
		}
		apiHandler.ServeHTTP(w, r)
	})
}

// withAPIVersions fija X-Notes-API-Versions en TODAS las respuestas de la API
// (incluidos 401 y preflight), como hacía el middleware original de ocnotes.
func (s *Server) withAPIVersions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(APIVersionsHeader, AllowedAPIVersions)
		next.ServeHTTP(w, r)
	})
}

// shadowUser copia el *auth.User que el middleware común deja en el contexto
// a la clave propia de notes (noteKeyType{}), conservando la semántica de
// shadow user y el escopado por graph ID sin tocar los handlers.
func (s *Server) shadowUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := auth.FromContext(r.Context()); u != nil {
			r = r.WithContext(context.WithValue(r.Context(), noteKeyType{}, u))
		}
		next.ServeHTTP(w, r)
	})
}

// handleImgSign firma un lote de URLs de imagen para el preview del cliente
// web. El contenido de la nota NO se muta: la firma vive solo en render time.
func (s *Server) handleImgSign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if s.images == nil {
		http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	var input struct {
		URLs []string `json:"urls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if len(input.URLs) == 0 || len(input.URLs) > 32 {
		http.Error(w, `{"error":"invalid url count"}`, http.StatusBadRequest)
		return
	}
	signed := make(map[string]string, len(input.URLs))
	for _, u := range input.URLs {
		if len(u) > 2048 || (!strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://")) {
			continue
		}
		signed[u] = imgproxy.ProxiedURL(s.base, u, s.images.Sign(u))
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"signed": signed})
}

// handleAttachment serves the Nextcloud Notes API v1.4 attachment endpoints.
// POST uploads the multipart `file` field; GET streams a stored attachment.
func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	user := currentUser(r)
	if _, err := s.store.GetNote(user, id); err != nil {
		if err == sql.ErrNoRows {
			w.Header().Set(APIVersionsHeader, AllowedAPIVersions)
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	if s.attachments == nil {
		http.Error(w, `{"error":"unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	switch r.Method {
	case http.MethodPost:
		s.handleAttachmentUpload(w, r, id)
	case http.MethodGet:
		s.handleAttachmentDownload(w, r, id)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAttachmentUpload(w http.ResponseWriter, r *http.Request, id int64) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

	err := r.ParseMultipartForm(1 << 20)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, `{"error":"attachment too large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `{"error":"invalid multipart form"}`, http.StatusBadRequest)
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"missing file field"}`, http.StatusBadRequest)
		return
	}
	defer file.Close()

	apiPath, err := s.attachments.Save(id, header.Filename, file)
	if err != nil {
		if err == attachments.ErrTooLarge {
			http.Error(w, `{"error":"attachment too large"}`, http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, `{"error":"save failed"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"filename": apiPath})
}

func (s *Server) handleAttachmentDownload(w http.ResponseWriter, r *http.Request, id int64) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, `{"error":"missing path"}`, http.StatusNotFound)
		return
	}

	rc, size, ct, err := s.attachments.Open(id, path)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// authenticate valida Basic/Bearer contra el validador Graph compartido. Lo
// usa handleUserInfo (OCS), donde la auth es opcional y nunca responde 401.
func (s *Server) authenticate(r *http.Request) (*auth.User, error) {
	if s.validator == nil {
		return nil, fmt.Errorf("no validator")
	}
	cred, ok := credential(r)
	if !ok {
		return nil, fmt.Errorf("no auth header")
	}
	u, ok := s.validator.Validate(r.Context(), cred)
	if !ok {
		return nil, fmt.Errorf("invalid credentials")
	}
	return u, nil
}

// credential extrae Basic o Bearer de la cabecera Authorization (misma forma
// que common/auth, cuyo parser no es exportado).
func credential(r *http.Request) (auth.Credential, bool) {
	h := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(h, "Basic "):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
		if err != nil {
			return auth.Credential{}, false
		}
		user, pass, ok := strings.Cut(string(raw), ":")
		if !ok || user == "" {
			return auth.Credential{}, false
		}
		return auth.Credential{Username: user, Password: pass}, true
	case strings.HasPrefix(h, "Bearer "):
		tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if tok == "" {
			return auth.Credential{}, false
		}
		return auth.Credential{Bearer: tok}, true
	}
	return auth.Credential{}, false
}

func (s *Server) handleNotes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetNotes(w, r)
	case http.MethodPost:
		s.handleCreateNote(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleNoteByID(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, `{"error":"invalid id"}`, http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetNote(w, r, id)
	case http.MethodPut:
		s.handleUpdateNote(w, r, id)
	case http.MethodDelete:
		s.handleDeleteNote(w, r, id)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetSettings(w, r)
	case http.MethodPut:
		s.handlePutSettings(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleGetNotes(w http.ResponseWriter, r *http.Request) {
	category := r.URL.Query().Get("category")
	exclude := strings.Split(r.URL.Query().Get("exclude"), ",")
	pruneBeforeStr := r.URL.Query().Get("pruneBefore")
	chunkSizeStr := r.URL.Query().Get("chunkSize")

	var pruneBefore int64
	if pruneBeforeStr != "" {
		pruneBefore, _ = strconv.ParseInt(pruneBeforeStr, 10, 64)
	}

	var chunkSize int
	if chunkSizeStr != "" {
		chunkSize, _ = strconv.Atoi(chunkSizeStr)
	}

	limit := 0
	if chunkSize > 0 {
		limit = chunkSize + 1
	}

	user := currentUser(r)
	notes, err := s.store.ListNotes(user, category, exclude, limit, pruneBefore)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	now := time.Now().Unix()
	if len(notes) > 0 {
		w.Header().Set("ETag", fmt.Sprintf("%x", sha256.Sum256([]byte(notes[0].Etag))))
		var newest int64
		for _, n := range notes {
			if n.Modified > newest {
				newest = n.Modified
			}
		}
		w.Header().Set("Last-Modified", time.Unix(newest, 0).Format(http.TimeFormat))
	}

	if len(notes) == 0 {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]interface{}{})
		return
	}

	if pruneBefore > 0 {
		pruned := make([]interface{}, 0, len(notes))
		for _, n := range notes {
			if n.Modified >= pruneBefore {
				pruned = append(pruned, map[string]interface{}{
					"id":       n.ID,
					"etag":     n.Etag,
					"modified": n.Modified,
					"title":    n.Title,
					"category": n.Category,
					"content":  n.Content,
					"favorite": n.Favorite,
					"readonly": n.Readonly,
				})
			} else {
				pruned = append(pruned, &prunedNote{ID: n.ID})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pruned)
		return
	}

	result := make([]map[string]interface{}, 0, len(notes))
	for _, n := range notes {
		if len(exclude) > 1 && exclude[0] != "" {
			m := map[string]interface{}{
				"id":       n.ID,
				"etag":     n.Etag,
				"modified": n.Modified,
				"title":    n.Title,
				"category": n.Category,
				"favorite": n.Favorite,
				"readonly": n.Readonly,
			}
			for _, e := range exclude {
				e = strings.TrimSpace(e)
				if e == "content" {
					delete(m, "content")
				}
			}
			result = append(result, m)
		} else {
			result = append(result, map[string]interface{}{
				"id":       n.ID,
				"etag":     n.Etag,
				"modified": n.Modified,
				"title":    n.Title,
				"category": n.Category,
				"content":  n.Content,
				"favorite": n.Favorite,
				"readonly": n.Readonly,
			})
		}
	}

	if chunkSize > 0 && len(result) > chunkSize {
		w.Header().Set("X-Notes-Chunk-Cursor", fmt.Sprintf("%d", now))
		result = result[:chunkSize]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) handleCreateNote(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Title    string `json:"title"`
		Content  string `json:"content"`
		Category string `json:"category"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}

	if input.Title == "" {
		input.Title = "New note"
	}
	now := time.Now().Unix()
	user := currentUser(r)
	note, err := s.store.CreateNote(user, input.Title, input.Content, input.Category, now)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(note)
}

func (s *Server) handleGetNote(w http.ResponseWriter, r *http.Request, id int64) {
	user := currentUser(r)
	note, err := s.store.GetNote(user, id)
	if err == sql.ErrNoRows {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", note.Etag)
	json.NewEncoder(w).Encode(note)
}

func (s *Server) handleUpdateNote(w http.ResponseWriter, r *http.Request, id int64) {
	user := currentUser(r)
	note, err := s.store.GetNote(user, id)
	if err == sql.ErrNoRows {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	expectedEtag := strings.Trim(r.Header.Get("If-Match"), "\"")
	if expectedEtag != "" && expectedEtag != note.Etag {
		w.WriteHeader(http.StatusPreconditionFailed)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(note)
		return
	}

	var input struct {
		Title    *string  `json:"title"`
		Content  *string  `json:"content"`
		Category *string  `json:"category"`
		Favorite jsonBool `json:"favorite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}

	var favorite *bool
	if input.Favorite.Set {
		favorite = &input.Favorite.Value
	}

	now := time.Now().Unix()
	updated, err := s.store.UpdateNote(user, id, input.Title, input.Content, input.Category, favorite, now)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(updated)
}

func (s *Server) handleDeleteNote(w http.ResponseWriter, r *http.Request, id int64) {
	user := currentUser(r)
	err := s.store.DeleteNote(user, id)
	if err == sql.ErrNoRows {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	if s.attachments != nil {
		if err := s.attachments.DeleteNote(id); err != nil {
			slog.Default().Error("delete note attachments", "id", id, "err", err)
		}
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, `{}`)
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	settings, err := s.store.GetSettings(user)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(settings)
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var input store.Settings
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}

	user := currentUser(r)
	result, err := s.store.UpdateSettings(user, input)
	if err != nil {
		http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// capabilitiesResponse mirrors the OCS capabilities envelope returned when a
// client asks for JSON, matching the shape Iotas expects.
type capabilitiesResponse struct {
	Ocs capabilitiesOcs `json:"ocs"`
}

type capabilitiesOcs struct {
	Meta capabilitiesMeta `json:"meta"`
	Data capabilitiesData `json:"data"`
}

type capabilitiesMeta struct {
	Status     string `json:"status"`
	StatusCode int    `json:"statuscode"`
	Message    string `json:"message"`
}

type capabilitiesData struct {
	Capabilities capabilitiesCaps `json:"capabilities"`
}

type capabilitiesCaps struct {
	Notes capabilitiesNotes `json:"notes"`
}

type capabilitiesNotes struct {
	APIVersion []string `json:"api_version"`
	Version    string   `json:"version"`
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	wantsJSON := strings.Contains(r.Header.Get("Accept"), "application/json") ||
		r.URL.Query().Get("format") == "json"

	if wantsJSON {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(capabilitiesResponse{
			Ocs: capabilitiesOcs{
				Meta: capabilitiesMeta{Status: "ok", StatusCode: 200, Message: "OK"},
				Data: capabilitiesData{Capabilities: capabilitiesCaps{Notes: capabilitiesNotes{
					APIVersion: []string{"0.2", "1.4"},
					Version:    Version,
				}}},
			},
		})
		return
	}

	xml := fmt.Sprintf(`<?xml version="1.0"?>
<ocs>
 <meta>
  <status>ok</status>
  <statuscode>200</statuscode>
  <message>OK</message>
 </meta>
 <data>
  <capabilities>
   <notes>
    <api_version>["0.2","1.4"]</api_version>
    <version>%s</version>
   </notes>
  </capabilities>
 </data>
</ocs>`, Version)

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	fmt.Fprint(w, xml)
}

func (s *Server) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	user, _ := s.authenticate(r)
	displayName := "User"
	if user != nil {
		displayName = user.DisplayName
	}

	xml := fmt.Sprintf(`<?xml version="1.0"?>
<ocs>
 <meta>
  <status>ok</status>
  <statuscode>200</statuscode>
  <message>OK</message>
 </meta>
 <data>
  <id>%s</id>
  <displayname>%s</displayname>
 </data>
</ocs>`, displayName, displayName)

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	fmt.Fprint(w, xml)
}
