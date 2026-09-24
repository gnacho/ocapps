package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	commonstore "github.com/gnacho/ocapps/internal/common/store"
	"github.com/gnacho/ocapps/internal/notes/store"
)

// newTestServer builds a Server on a temporary store. The validator, image
// proxy and attachment store are nil because the handlers under test only use
// the note store.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notes.db")
	db, err := commonstore.Open(path)
	if err != nil {
		t.Fatalf("common store.Open: %v", err)
	}
	s, err := store.Open(db, path, "")
	if err != nil {
		_ = db.Close()
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewServer(Base, "", s, nil, nil, nil)
}

// authedRequest builds a request whose context carries the authenticated user,
// mirroring what the middleware injects via the noteKeyType{} key.
func authedRequest(method, path string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, path, body)
	ctx := context.WithValue(req.Context(), noteKeyType{}, &auth.User{ID: "user-a"})
	return req.WithContext(ctx)
}

func TestCapabilitiesJSON(t *testing.T) {
	s := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/capabilities", nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	s.handleCapabilities(w, req)

	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	var resp capabilitiesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if resp.Ocs.Meta.Status != "ok" || resp.Ocs.Meta.StatusCode != 200 || resp.Ocs.Meta.Message != "OK" {
		t.Fatalf("meta = %+v", resp.Ocs.Meta)
	}
	notes := resp.Ocs.Data.Capabilities.Notes
	if len(notes.APIVersion) != 2 || notes.APIVersion[0] != "0.2" || notes.APIVersion[1] != "1.4" {
		t.Fatalf("api_version = %#v, want [0.2 1.4]", notes.APIVersion)
	}
	if notes.Version != Version {
		t.Fatalf("version = %q, want %q", notes.Version, Version)
	}
}

func TestCapabilitiesFormatParam(t *testing.T) {
	s := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/capabilities?format=json", nil)
	w := httptest.NewRecorder()
	s.handleCapabilities(w, req)

	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var resp capabilitiesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if len(resp.Ocs.Data.Capabilities.Notes.APIVersion) != 2 {
		t.Fatalf("api_version = %#v, want two elements", resp.Ocs.Data.Capabilities.Notes.APIVersion)
	}
}

func TestCapabilitiesXMLDefault(t *testing.T) {
	s := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/capabilities", nil)
	w := httptest.NewRecorder()
	s.handleCapabilities(w, req)

	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/xml") {
		t.Fatalf("Content-Type = %q, want application/xml", ct)
	}
	if !strings.Contains(w.Body.String(), "<api_version>") {
		t.Fatalf("xml body missing api_version: %s", w.Body.String())
	}
}

func TestUpdateNoteFavorite(t *testing.T) {
	s := newTestServer(t)

	cases := []struct {
		name string
		body string
		want bool
	}{
		{"one", `{"favorite":1}`, true},
		{"zero", `{"favorite":0}`, false},
		{"true", `{"favorite":true}`, true},
		{"absent", `{"title":"renamed"}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			note, err := s.store.CreateNote("user-a", "title", "content", "", 1)
			if err != nil {
				t.Fatalf("CreateNote: %v", err)
			}

			req := authedRequest(http.MethodPut, "/notes", strings.NewReader(tc.body))
			req.Header.Set("If-Match", `"`+note.Etag+`"`)
			w := httptest.NewRecorder()
			s.handleUpdateNote(w, req, note.ID)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}

			var got store.Note
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got.Favorite != tc.want {
				t.Fatalf("favorite = %v, want %v", got.Favorite, tc.want)
			}
		})
	}
}

func TestUpdateNoteIfMatchMismatch(t *testing.T) {
	s := newTestServer(t)

	note, err := s.store.CreateNote("user-a", "title", "content", "", 1)
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}

	req := authedRequest(http.MethodPut, "/notes", strings.NewReader(`{"favorite":1}`))
	req.Header.Set("If-Match", `"wrong-etag"`)
	w := httptest.NewRecorder()
	s.handleUpdateNote(w, req, note.ID)

	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", w.Code)
	}

	got, err := s.store.GetNote("user-a", note.ID)
	if err != nil {
		t.Fatalf("GetNote: %v", err)
	}
	if got.Favorite {
		t.Fatal("favorite changed despite mismatched If-Match")
	}
}

func TestReadonlyPresent(t *testing.T) {
	s := newTestServer(t)

	note, err := s.store.CreateNote("user-a", "title", "content", "", 1)
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}

	// List response.
	req := authedRequest(http.MethodGet, "/notes", nil)
	w := httptest.NewRecorder()
	s.handleGetNotes(w, req)

	var list []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list length = %d, want 1", len(list))
	}
	if _, ok := list[0]["readonly"]; !ok {
		t.Fatalf("list note missing readonly: %v", list[0])
	}

	// Single-note response.
	req = authedRequest(http.MethodGet, "/notes/1", nil)
	w = httptest.NewRecorder()
	s.handleGetNote(w, req, note.ID)

	var single map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &single); err != nil {
		t.Fatalf("decode single: %v", err)
	}
	if _, ok := single["readonly"]; !ok {
		t.Fatalf("single note missing readonly: %v", single)
	}
}

func TestPruneBeforeReturnsStubs(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.store.CreateNote("user-a", "old", "old body", "", 100); err != nil {
		t.Fatalf("CreateNote old: %v", err)
	}
	if _, err := s.store.CreateNote("user-a", "new", "new body", "", 200); err != nil {
		t.Fatalf("CreateNote new: %v", err)
	}

	req := authedRequest(http.MethodGet, "/notes?pruneBefore=150", nil)
	w := httptest.NewRecorder()
	s.handleGetNotes(w, req)

	var list []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list length = %d, want 2 (all notes)", len(list))
	}

	var full, stub map[string]interface{}
	for _, n := range list {
		if _, hasContent := n["content"]; hasContent {
			full = n
		} else {
			stub = n
		}
	}
	if full == nil || stub == nil {
		t.Fatalf("expected one full and one stub note, got %v", list)
	}
	if _, ok := stub["id"]; !ok {
		t.Fatalf("stub missing id: %v", stub)
	}
	if len(stub) != 1 {
		t.Fatalf("stub should only carry id, got %v", stub)
	}

	want := time.Unix(200, 0).Format(http.TimeFormat)
	if got := w.Header().Get("Last-Modified"); got != want {
		t.Fatalf("Last-Modified = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// /ocs/v2.php/cloud/user extendido (SPEC §4.2, H5): único handler de la ruta
// en el servicio unificado. Modo XML por defecto (Iotas) + modo JSON para
// news-android (id=username, displayname, display-name; 401 sin auth).
// ---------------------------------------------------------------------------

// newTestServerConGraph builds a Server whose validator talks to a fake
// Graph that only admits Basic nacho:app-token.
func newTestServerConGraph(t *testing.T) *Server {
	t.Helper()
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "nacho" || p != "app-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"id": "uuid-nacho", "displayName": "Nacho",
			"onPremisesSamAccountName": "nacho",
		})
	}))
	t.Cleanup(graph.Close)

	path := filepath.Join(t.TempDir(), "notes.db")
	db, err := commonstore.Open(path)
	if err != nil {
		t.Fatalf("common store.Open: %v", err)
	}
	s, err := store.Open(db, path, "")
	if err != nil {
		_ = db.Close()
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	v := auth.NewGraphValidator(graph.URL, auth.MultiTenant(), nil)
	return NewServer(Base, "", s, v, nil, nil)
}

func TestUserInfoJSONConAuth(t *testing.T) {
	s := newTestServerConGraph(t)
	for _, variant := range []string{"?format=json", "accept"} {
		req := httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/user", nil)
		if variant == "?format=json" {
			req = httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/user?format=json", nil)
		} else {
			req.Header.Set("Accept", "application/json")
			req.Header.Set("OCS-APIRequest", "true")
		}
		req.SetBasicAuth("nacho", "app-token")
		w := httptest.NewRecorder()
		s.handleUserInfo(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (%s)", variant, w.Code, w.Body.String())
		}
		body := w.Body.String()
		for _, want := range []string{`"ocs"`, `"status":"ok"`, `"statuscode":200`,
			`"id":"nacho"`, `"displayname":"Nacho"`, `"display-name":"Nacho"`} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s: falta %s en %s", variant, want, body)
			}
		}
	}
}

func TestUserInfoJSONSinAuth(t *testing.T) {
	s := newTestServerConGraph(t)
	req := httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/user?format=json", nil)
	w := httptest.NewRecorder()
	s.handleUserInfo(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("JSON sin auth: status = %d, want 401", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("JSON sin auth: falta WWW-Authenticate")
	}
	if !strings.Contains(w.Body.String(), `"statuscode":401`) {
		t.Fatalf("JSON sin auth: body = %s", w.Body.String())
	}
}

func TestUserInfoXMLModoPorDefecto(t *testing.T) {
	s := newTestServerConGraph(t)

	// sin auth: comportamiento histórico (Iotas) → 200 "User"
	req := httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/user", nil)
	w := httptest.NewRecorder()
	s.handleUserInfo(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("XML sin auth: status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<id>User</id>") || !strings.Contains(body, "<displayname>User</displayname>") {
		t.Fatalf("XML sin auth: body = %s", body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/xml") {
		t.Fatalf("XML sin auth: Content-Type = %q", ct)
	}

	// con auth: id/displayname = display name del usuario Graph
	req2 := httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/user", nil)
	req2.SetBasicAuth("nacho", "app-token")
	w2 := httptest.NewRecorder()
	s.handleUserInfo(w2, req2)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), "<id>Nacho</id>") {
		t.Fatalf("XML con auth: %d %s", w2.Code, w2.Body.String())
	}
}

func TestCapabilitiesMergeCoreJSON(t *testing.T) {
	var gotAuth, gotFormat string
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotFormat = r.URL.Query().Get("format")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ocs":{"meta":{"status":"ok","statuscode":200,"message":"OK"},`+
			`"data":{"capabilities":{"core":{"status":{"productversion":"8.0.1","versionstring":"0.1.0"}},`+
			`"files":{"bigfilechunking":true}}}}}`)
	}))
	defer core.Close()

	s := NewServer(Base, core.URL, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/capabilities?format=json", nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OCS-APIRequest", "true")
	req.SetBasicAuth("nacho", "app-token")
	w := httptest.NewRecorder()
	s.handleCapabilities(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if gotAuth == "" {
		t.Fatal("el core no recibio la cabecera Authorization")
	}
	if gotFormat != "json" {
		t.Fatalf("el core recibio format=%q, want json", gotFormat)
	}
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	caps := doc["ocs"].(map[string]any)["data"].(map[string]any)["capabilities"].(map[string]any)
	if _, ok := caps["core"]; !ok {
		t.Fatalf("falta el bloque core: %s", w.Body.String())
	}
	notes, ok := caps["notes"].(map[string]any)
	if !ok {
		t.Fatalf("falta el bloque notes: %s", w.Body.String())
	}
	if notes["version"] != Version {
		t.Fatalf("notes.version = %v, want %s", notes["version"], Version)
	}
}

func TestCapabilitiesMergeCoreXML(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		_, _ = io.WriteString(w, `<?xml version="1.0"?><ocs><meta><status>ok</status>`+
			`<statuscode>100</statuscode><message>OK</message></meta><data><capabilities>`+
			`<core><status><productversion>8.0.1</productversion></status></core>`+
			`</capabilities></data></ocs>`)
	}))
	defer core.Close()

	s := NewServer(Base, core.URL, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/capabilities", nil)
	w := httptest.NewRecorder()
	s.handleCapabilities(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "<core>") || !strings.Contains(body, "8.0.1") {
		t.Fatalf("falta el bloque core: %s", body)
	}
	if !strings.Contains(body, "<notes>") || !strings.Contains(body, Version) {
		t.Fatalf("falta el bloque notes: %s", body)
	}
	if i := strings.Index(body, "</capabilities>"); i < 0 || strings.Index(body, "<notes>") > i {
		t.Fatalf("notes debe ir dentro de capabilities: %s", body)
	}
}

func TestCapabilitiesFallbackWhenCoreFails(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer core.Close()

	s := NewServer(Base, core.URL, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/ocs/v2.php/cloud/capabilities?format=json", nil)
	w := httptest.NewRecorder()
	s.handleCapabilities(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp capabilitiesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Ocs.Data.Capabilities.Notes.Version != Version {
		t.Fatalf("fallback no devolvio notes: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"core"`) {
		t.Fatalf("el fallback no debe incluir core: %s", w.Body.String())
	}
}
