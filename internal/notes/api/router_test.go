package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/imgproxy"
	"github.com/gnacho/ocapps/internal/notes/attachments"
	"github.com/gnacho/ocapps/internal/notes/store"
)

const (
	testUser     = "alice"
	testAppToken = "app-token"
	testGraphID  = "id-alice"
)

// graphStub answers /graph/v1.0/me with a single valid Basic credential, so the
// router can be exercised through its real authentication middleware.
func graphStub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graph/v1.0/me" {
			http.NotFound(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok || u != testUser || p != testAppToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id":                       testGraphID,
			"displayName":              "Alice Liddell",
			"onPremisesSamAccountName": testUser,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newRouterServer builds the full Server (validator, image proxy and
// attachments included) over a temporary store and returns it with its router.
func newRouterServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s := newTestServer(t)
	dataDir := t.TempDir()

	images, err := imgproxy.New(dataDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("imgproxy.New: %v", err)
	}
	atts, err := attachments.New(dataDir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("attachments.New: %v", err)
	}

	g := graphStub(t)
	s.validator = auth.NewGraphValidator(g.URL, auth.MultiTenant(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.images = images
	s.attachments = atts
	return s, s.Router()
}

// do performs an authenticated request against the router.
func do(t *testing.T, h http.Handler, method, path string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.SetBasicAuth(testUser, testAppToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRouterRejectsRequestsWithoutCredentials(t *testing.T) {
	_, h := newRouterServer(t)

	req := httptest.NewRequest(http.MethodGet, Base+"notes", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	// The versions header is set on every API response, 401 included.
	if got := w.Header().Get(APIVersionsHeader); got != AllowedAPIVersions {
		t.Fatalf("%s = %q, want %q", APIVersionsHeader, got, AllowedAPIVersions)
	}
	if got := w.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Basic") {
		t.Fatalf("WWW-Authenticate = %q, want a Basic challenge", got)
	}
}

func TestRouterPreflightNeedsNoAuth(t *testing.T) {
	_, h := newRouterServer(t)

	req := httptest.NewRequest(http.MethodOptions, Base+"notes", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if got := w.Header().Get(APIVersionsHeader); got != AllowedAPIVersions {
		t.Fatalf("%s = %q on the preflight", APIVersionsHeader, got)
	}
}

// TestRouterNoteLifecycle walks the whole CRUD chain through the router, which
// also covers the handler dispatchers (notes, notes/{id}) and the shadow user
// injection of the middleware chain.
func TestRouterNoteLifecycle(t *testing.T) {
	_, h := newRouterServer(t)

	// Create.
	w := do(t, h, http.MethodPost, Base+"notes", strings.NewReader(`{"title":"Shopping","content":"milk","category":"life"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("create status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var created store.Note
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created note: %v", err)
	}
	// The note is scoped by the graph id the validator returned, never by the
	// username.
	if created.ID == 0 {
		t.Fatalf("created note has no id: %+v", created)
	}

	// List.
	w = do(t, h, http.MethodGet, Base+"notes", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", w.Code)
	}
	var list []store.Note
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v, want the created note", list)
	}

	notePath := fmt.Sprintf("%snotes/%d", Base, created.ID)

	// Read.
	w = do(t, h, http.MethodGet, notePath, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", w.Code)
	}
	if etag := w.Header().Get("ETag"); etag == "" || etag != created.Etag {
		t.Fatalf("ETag = %q, want %q", etag, created.Etag)
	}

	// Update.
	w = do(t, h, http.MethodPut, notePath, strings.NewReader(`{"title":"Shopping 2","favorite":1}`))
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var updated store.Note
	if err := json.Unmarshal(w.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated note: %v", err)
	}
	if updated.Title != "Shopping 2" || !updated.Favorite {
		t.Fatalf("updated note = %+v, want the new title and favorite", updated)
	}

	// Delete.
	w = do(t, h, http.MethodDelete, notePath, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("delete body = %q, want {}", w.Body.String())
	}

	// Gone.
	w = do(t, h, http.MethodGet, notePath, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", w.Code)
	}
}

func TestRouterCreateNoteDefaultsTheTitle(t *testing.T) {
	_, h := newRouterServer(t)

	w := do(t, h, http.MethodPost, Base+"notes", strings.NewReader(`{"content":"no title"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var created store.Note
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Title != "New note" {
		t.Fatalf("title = %q, want the default", created.Title)
	}
}

func TestRouterCreateNoteRejectsInvalidJSON(t *testing.T) {
	_, h := newRouterServer(t)

	w := do(t, h, http.MethodPost, Base+"notes", strings.NewReader(`{`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestRouterUpdateNoteRejectsInvalidFavorite(t *testing.T) {
	_, h := newRouterServer(t)

	created := createNote(t, h, "T", "c")
	w := do(t, h, http.MethodPut, fmt.Sprintf("%snotes/%d", Base, created.ID), strings.NewReader(`{"favorite":"yes"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unusable favorite value", w.Code)
	}
}

func TestRouterUpdateNoteHonoursIfMatch(t *testing.T) {
	_, h := newRouterServer(t)

	created := createNote(t, h, "T", "c")
	req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("%snotes/%d", Base, created.ID), strings.NewReader(`{"title":"x"}`))
	req.SetBasicAuth(testUser, testAppToken)
	req.Header.Set("If-Match", `"stale-etag"`)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412 on a stale If-Match", w.Code)
	}
	var current store.Note
	if err := json.Unmarshal(w.Body.Bytes(), &current); err != nil {
		t.Fatalf("decode current note: %v", err)
	}
	if current.Title != "T" {
		t.Fatalf("title = %q, the note must not change on 412", current.Title)
	}
}

func TestRouterNoteIDValidation(t *testing.T) {
	_, h := newRouterServer(t)

	if w := do(t, h, http.MethodGet, Base+"notes/not-a-number", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	// A trailing slash with no id is not a note.
	if w := do(t, h, http.MethodGet, Base+"notes/", nil); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestRouterMethodNotAllowed(t *testing.T) {
	_, h := newRouterServer(t)

	if w := do(t, h, http.MethodPatch, Base+"notes", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("notes PATCH status = %d, want 405", w.Code)
	}
	if w := do(t, h, http.MethodPatch, Base+"settings", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("settings PATCH status = %d, want 405", w.Code)
	}
	created := createNote(t, h, "T", "c")
	if w := do(t, h, http.MethodPost, fmt.Sprintf("%snotes/%d", Base, created.ID), nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("note POST status = %d, want 405", w.Code)
	}
}

func TestRouterSettings(t *testing.T) {
	_, h := newRouterServer(t)

	w := do(t, h, http.MethodGet, Base+"settings", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200", w.Code)
	}

	w = do(t, h, http.MethodPut, Base+"settings", strings.NewReader(`{"notesPath":"Notes","theme":"dark"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("put status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var settings store.Settings
	if err := json.Unmarshal(w.Body.Bytes(), &settings); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if settings["theme"] != "dark" {
		t.Fatalf("settings = %+v, want the stored theme", settings)
	}

	w = do(t, h, http.MethodPut, Base+"settings", strings.NewReader(`{`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid json status = %d, want 400", w.Code)
	}
}

func TestRouterImgSign(t *testing.T) {
	s, h := newRouterServer(t)

	w := do(t, h, http.MethodPost, Base+"img/sign", strings.NewReader(`{"urls":["https://example.com/a.jpg","ftp://example.com/skip.jpg"]}`))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var out struct {
		Signed map[string]string `json:"signed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Only the usable URL is signed, with the expected signature.
	url := "https://example.com/a.jpg"
	if len(out.Signed) != 1 {
		t.Fatalf("signed = %+v, want a single entry", out.Signed)
	}
	if got, want := out.Signed[url], imgproxy.ProxiedURL(s.base, url, s.images.Sign(url)); got != want {
		t.Fatalf("signed url = %q, want %q", got, want)
	}

	if w := do(t, h, http.MethodGet, Base+"img/sign", nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", w.Code)
	}
	if w := do(t, h, http.MethodPost, Base+"img/sign", strings.NewReader(`{`)); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid json status = %d, want 400", w.Code)
	}
	if w := do(t, h, http.MethodPost, Base+"img/sign", strings.NewReader(`{"urls":[]}`)); w.Code != http.StatusBadRequest {
		t.Fatalf("empty urls status = %d, want 400", w.Code)
	}
}

func TestRouterImgSignUnavailableWithoutProxy(t *testing.T) {
	s, _ := newRouterServer(t)
	s.images = nil
	h := s.Router()

	if w := do(t, h, http.MethodPost, Base+"img/sign", strings.NewReader(`{"urls":["https://example.com/a.jpg"]}`)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestRouterAttachmentUploadAndDownload(t *testing.T) {
	_, h := newRouterServer(t)
	created := createNote(t, h, "With attachment", "c")
	attPath := fmt.Sprintf("%sattachment/%d", Base14, created.ID)

	body, contentType := multipartBody(t, "file", "notes.txt", []byte("hello attachment"))
	req := httptest.NewRequest(http.MethodPost, attPath, body)
	req.SetBasicAuth(testUser, testAppToken)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var up struct {
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	if !strings.HasPrefix(up.Filename, fmt.Sprintf(".attachments.%d/", created.ID)) {
		t.Fatalf("filename = %q, want the attachment api path", up.Filename)
	}

	// Download it back.
	w = do(t, h, http.MethodGet, attPath+"?path="+up.Filename, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "hello attachment" {
		t.Fatalf("downloaded body = %q, want the uploaded bytes", got)
	}
	if ct := w.Header().Get("Content-Type"); ct == "" {
		t.Fatal("download must set a Content-Type")
	}
}

func TestRouterAttachmentErrors(t *testing.T) {
	_, h := newRouterServer(t)
	created := createNote(t, h, "T", "c")
	attPath := fmt.Sprintf("%sattachment/%d", Base14, created.ID)

	if w := do(t, h, http.MethodGet, Base14+"attachment/not-a-number", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d, want 400", w.Code)
	}
	if w := do(t, h, http.MethodGet, Base14+"attachment/", nil); w.Code != http.StatusNotFound {
		t.Fatalf("no id status = %d, want 404", w.Code)
	}
	// A note that does not exist cannot own attachments.
	if w := do(t, h, http.MethodGet, Base14+"attachment/999999", nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing note status = %d, want 404", w.Code)
	}
	// Method outside GET/POST.
	if w := do(t, h, http.MethodPut, attPath, nil); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT status = %d, want 405", w.Code)
	}
	// Download without a path.
	if w := do(t, h, http.MethodGet, attPath, nil); w.Code != http.StatusNotFound {
		t.Fatalf("download without path status = %d, want 404", w.Code)
	}
	// Download of an attachment that was never stored.
	if w := do(t, h, http.MethodGet, attPath+"?path=.attachments.1/none.txt", nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing attachment status = %d, want 404", w.Code)
	}
	// Upload with no file field.
	body, contentType := multipartBody(t, "other", "notes.txt", []byte("x"))
	req := httptest.NewRequest(http.MethodPost, attPath, body)
	req.SetBasicAuth(testUser, testAppToken)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing file field status = %d, want 400", w.Code)
	}
	// Upload that is not multipart at all.
	if w := do(t, h, http.MethodPost, attPath, strings.NewReader("plain text")); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid multipart status = %d, want 400", w.Code)
	}
}

func TestRouterAttachmentUnavailableWithoutStore(t *testing.T) {
	s, _ := newRouterServer(t)
	created := createNote(t, s.Router(), "T", "c")
	s.attachments = nil
	h := s.Router()

	if w := do(t, h, http.MethodGet, fmt.Sprintf("%sattachment/%d", Base14, created.ID), nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestRouterDeletesAttachmentsWithTheNote(t *testing.T) {
	_, h := newRouterServer(t)
	created := createNote(t, h, "T", "c")
	attPath := fmt.Sprintf("%sattachment/%d", Base14, created.ID)

	body, contentType := multipartBody(t, "file", "a.txt", []byte("x"))
	req := httptest.NewRequest(http.MethodPost, attPath, body)
	req.SetBasicAuth(testUser, testAppToken)
	req.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload status = %d, want 200", w.Code)
	}

	// The note goes away with its attachments (the store is told to clean
	// them, the endpoint still answers 200).
	if w := do(t, h, http.MethodDelete, fmt.Sprintf("%snotes/%d", Base, created.ID), nil); w.Code != http.StatusOK {
		t.Fatalf("delete note status = %d, want 200", w.Code)
	}
	if w := do(t, h, http.MethodGet, attPath, nil); w.Code != http.StatusNotFound {
		t.Fatalf("attachment of a deleted note status = %d, want 404", w.Code)
	}
}

func TestRouterOCSCapabilitiesAndUser(t *testing.T) {
	_, h := newRouterServer(t)

	req := httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/capabilities?format=json", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("capabilities status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	// User info in JSON mode requires credentials.
	req = httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/user?format=json", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated user info status = %d, want 401", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/user?format=json", nil)
	req.SetBasicAuth(testUser, testAppToken)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("user info status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
}

func TestCurrentUserWithoutContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, Base+"notes", nil)
	if got := currentUser(req); got != "" {
		t.Fatalf("currentUser = %q, want the empty string with no user in context", got)
	}
}

// createNote creates a note through the router and returns it.
func createNote(t *testing.T, h http.Handler, title, content string) store.Note {
	t.Helper()
	body := fmt.Sprintf(`{"title":%q,"content":%q}`, title, content)
	w := do(t, h, http.MethodPost, Base+"notes", strings.NewReader(body))
	if w.Code != http.StatusOK {
		t.Fatalf("create note status = %d (%s)", w.Code, w.Body.String())
	}
	var note store.Note
	if err := json.Unmarshal(w.Body.Bytes(), &note); err != nil {
		t.Fatalf("decode created note: %v", err)
	}
	return note
}

// multipartBody builds a multipart form with a single file field.
func multipartBody(t *testing.T, field, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write form content: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func TestRouterGetNotesFilters(t *testing.T) {
	_, h := newRouterServer(t)
	createNote(t, h, "work note", "w")
	w2 := do(t, h, http.MethodPost, Base+"notes", strings.NewReader(`{"title":"life note","content":"l","category":"life"}`))
	if w2.Code != http.StatusOK {
		t.Fatalf("create with category status = %d", w2.Code)
	}

	// Category filter.
	w := do(t, h, http.MethodGet, Base+"notes?category=life", nil)
	var byCategory []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &byCategory); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(byCategory) != 1 || byCategory[0]["title"] != "life note" {
		t.Fatalf("category filter = %+v, want the life note only", byCategory)
	}

	// Excluding a field strips it from every note and keeps the rest. The
	// handler only builds the trimmed projection for two or more fields (a
	// single value falls through, inherited from the original ocnotes API).
	w = do(t, h, http.MethodGet, Base+"notes?exclude=content,etag", nil)
	var withoutContent []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &withoutContent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(withoutContent) != 2 {
		t.Fatalf("notes = %d, want 2", len(withoutContent))
	}
	for _, n := range withoutContent {
		if _, hasContent := n["content"]; hasContent {
			t.Fatalf("note %+v still carries content", n)
		}
		if n["title"] == nil || n["id"] == nil {
			t.Fatalf("note %+v lost the fields that must stay", n)
		}
	}
}

func TestRouterGetNotesPruneBefore(t *testing.T) {
	_, h := newRouterServer(t)
	createNote(t, h, "old", "c")

	// A cutoff in the future prunes every existing note down to id and etag.
	future := time.Now().Add(time.Hour).Unix()
	w := do(t, h, http.MethodGet, fmt.Sprintf("%snotes?pruneBefore=%d", Base, future), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var pruned []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &pruned); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(pruned) != 1 {
		t.Fatalf("pruned = %+v, want one entry", pruned)
	}
	// A pruned note is reduced to its id, clients refetch the rest.
	if len(pruned[0]) != 1 {
		t.Fatalf("pruned note = %+v, want only the id", pruned[0])
	}
	if pruned[0]["id"] == nil {
		t.Fatalf("pruned note = %+v, want the id", pruned[0])
	}
}

func TestRouterGetNotesChunking(t *testing.T) {
	_, h := newRouterServer(t)
	for i := 0; i < 3; i++ {
		createNote(t, h, fmt.Sprintf("note %d", i), "c")
	}

	w := do(t, h, http.MethodGet, Base+"notes?chunkSize=2", nil)
	var chunk []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &chunk); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(chunk) != 2 {
		t.Fatalf("chunk = %d notes, want 2", len(chunk))
	}
	if cursor := w.Header().Get("X-Notes-Chunk-Cursor"); cursor == "" {
		t.Fatal("a truncated chunk must carry X-Notes-Chunk-Cursor")
	}
}

func TestRouterGetNotesEmptyList(t *testing.T) {
	_, h := newRouterServer(t)

	w := do(t, h, http.MethodGet, Base+"notes", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if body := strings.TrimSpace(w.Body.String()); body != "[]" {
		t.Fatalf("body = %q, want an empty JSON array", body)
	}
	if etag := w.Header().Get("ETag"); etag != "" {
		t.Fatalf("ETag = %q, an empty list must not carry one", etag)
	}
}

func TestCredentialParsing(t *testing.T) {
	cases := []struct {
		name   string
		header string
		wantOK bool
	}{
		{"basic", "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:token")), true},
		{"basic without colon", "Basic " + base64.StdEncoding.EncodeToString([]byte("alice")), false},
		{"basic without user", "Basic " + base64.StdEncoding.EncodeToString([]byte(":token")), false},
		{"basic with broken base64", "Basic !!!", false},
		{"bearer", "Bearer a-token", true},
		{"empty bearer", "Bearer ", false},
		{"no header", "", false},
		{"unknown scheme", "Digest abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if _, ok := credential(req); ok != tc.wantOK {
				t.Fatalf("credential(%q) ok = %v, want %v", tc.header, ok, tc.wantOK)
			}
		})
	}
}

func TestJsonBoolAcceptsNullAsUnset(t *testing.T) {
	var b jsonBool
	if err := json.Unmarshal([]byte(`null`), &b); err != nil {
		t.Fatalf("unmarshal null: %v", err)
	}
	if b.Set {
		t.Fatal("null must leave the value unset")
	}
	if err := json.Unmarshal([]byte(`"yes"`), &b); err == nil {
		t.Fatal("an unusable value must be rejected")
	}
}
