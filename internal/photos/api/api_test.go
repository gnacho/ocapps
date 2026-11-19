package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/webdav"
	"github.com/gnacho/ocapps/internal/photos/exif"
	"github.com/gnacho/ocapps/internal/photos/store"
)

// fakeProvider implements SessionProvider for tests (no real registry).
type fakeProvider struct {
	mu      sync.Mutex
	legacy  string // owner of the static token, "" = error
	dav     *webdav.Client
	prefix  string
	touched []string
	scans   []string
	// spacePrefixes: webDavUrl path of each space id (folders by space)
	spacePrefixes map[string]string
}

func (f *fakeProvider) Touch(_ context.Context, u *auth.User, _ auth.Credential) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, u.ID)
}
func (f *fakeProvider) DavFor(_ context.Context, _ string) *webdav.Client { return f.dav }
func (f *fakeProvider) SpacePrefix(_ context.Context, _ string) string    { return f.prefix }
func (f *fakeProvider) SpacePrefixOf(_ context.Context, _, spaceID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spacePrefixes == nil {
		return ""
	}
	return f.spacePrefixes[spaceID]
}
func (f *fakeProvider) LegacyOwner(_ context.Context) (string, error) {
	if f.legacy == "" {
		return "", errors.New("legacy not configured")
	}
	return f.legacy, nil
}
func (f *fakeProvider) RequestScan(owner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scans = append(f.scans, owner)
}

func newTestServer(t *testing.T) (*Server, *store.Store, *fakeProvider) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fp := &fakeProvider{legacy: "legacy-owner"}
	s := New(st, nil, nil, nil, fp,
		Config{Token: "test-token", Secret: []byte("0123456789abcdef0123456789abcdef")},
		slog.Default())
	return s, st, fp
}

func do(t *testing.T, h http.Handler, method, path, authz string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestRoutesUnderPublicPrefix: every route hangs from PublicPrefix
// (SPEC §4.3) and the prefix is mandatory (no proxy strip).
func TestRoutesUnderPublicPrefix(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	bearer := "Bearer test-token"

	// with auth: the routes exist under the prefix
	for _, p := range []string{
		"/api/stats",
		"/api/assets",
		"/api/duplicates",
		"/api/tags",
		"/api/albums",
		"/api/folders",
		"/api/geo",
		"/api/places",
		"/api/memories/on-this-day",
		"/api/memories/highlights",
		"/api/timeline/calendar",
	} {
		rr := do(t, h, http.MethodGet, PublicPrefix+p, bearer)
		if rr.Code == http.StatusNotFound || rr.Code == http.StatusUnauthorized {
			t.Fatalf("GET %s%s -> %d", PublicPrefix, p, rr.Code)
		}
	}

	// without the prefix: there is no route (the proxy no longer strips)
	if rr := do(t, h, http.MethodGet, "/api/stats", bearer); rr.Code != http.StatusNotFound {
		t.Fatalf("GET /api/stats without prefix -> %d, want 404", rr.Code)
	}

	// Q7: dead endpoints removed (no consumer in the extension)
	for _, p := range []string{
		"/api/thumb?path=/Fotos/IMG.heic", // thumb by path
		"/api/assets/1",                   // asset by id (bare GET)
		"/api/assets/1/hls/index.m3u8",    // HLS
	} {
		rr := do(t, h, http.MethodGet, PublicPrefix+p, bearer)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("GET %s%s -> %d, want 404 (endpoint removed in Q7)", PublicPrefix, p, rr.Code)
		}
	}

	// without auth: 401 (except the signed video and the preflight)
	if rr := do(t, h, http.MethodGet, PublicPrefix+"/api/stats", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("stats without auth -> %d, want 401", rr.Code)
	}
	if rr := do(t, h, http.MethodGet, PublicPrefix+"/api/stats", "Bearer otro"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("stats with a wrong token -> %d, want 401", rr.Code)
	}

	// OPTIONS preflight without auth: 204 (common CORS, SPEC §4.4)
	rr := do(t, h, http.MethodOptions, PublicPrefix+"/api/stats", "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS -> %d, want 204", rr.Code)
	}
	if acao := rr.Header().Get("Access-Control-Allow-Origin"); acao != "*" {
		t.Fatalf("ACAO = %q", acao)
	}
}

// TestStaticTokenLegacyOwner: the DEPRECATED static Bearer maps to the legacy
// owner (oc_id of OCAPPS_PHOTOS_USER), H8 §6.2.
func TestStaticTokenLegacyOwner(t *testing.T) {
	s, st, _ := newTestServer(t)
	h := s.Handler()
	ctx := context.Background()
	// one asset of the legacy owner and another one of another owner: the
	// static token only sees its own
	if _, _, err := st.UpsertByETag(ctx, "legacy-owner", "/dav/x/a.jpg", "e1", "a.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.UpsertByETag(ctx, "otro", "/dav/x/b.jpg", "e2", "b.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	rr := do(t, h, http.MethodGet, PublicPrefix+"/api/assets", "Bearer test-token")
	if rr.Code != http.StatusOK {
		t.Fatalf("assets -> %d", rr.Code)
	}
	var body struct {
		Assets []store.Asset `json:"assets"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Assets) != 1 || body.Assets[0].Path != "/dav/x/a.jpg" {
		t.Fatalf("the static token must only see the legacy owner's assets: %+v", body.Assets)
	}
	// token by query param (compat) also maps
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/stats?token=test-token", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("stats?token -> %d", rr.Code)
	}
}

// TestSignedVideo: the signature uses PublicPrefix and the signed route is
// exempt from auth (the HMAC signature is the credential). video-url verifies
// ownership BEFORE signing (H8).
func TestSignedVideo(t *testing.T) {
	// fake DAV serving the video content
	davSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("video-data"))
	}))
	defer davSrv.Close()

	s, st, fp := newTestServer(t)
	fp.dav = webdav.NewBearer(davSrv.URL, "tok")
	h := s.Handler()
	bearer := "Bearer test-token"
	ctx := context.Background()

	// without the owner asset: video-url -> 404 (nothing foreign or
	// non-existent is signed)
	rr := do(t, h, http.MethodPost, PublicPrefix+"/api/assets/42/video-url", bearer)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("video-url of a non-existent asset -> %d, want 404", rr.Code)
	}

	id, _, err := st.UpsertByETag(ctx, "legacy-owner", "/dav/x/v.mp4", "e1", "v.mp4", "video", time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	// an asset with the same path OF ANOTHER owner is no good for signing
	if _, _, err := st.UpsertByETag(ctx, "otro", "/dav/x/v.mp4", "e2", "v.mp4", "video", time.Now(), 100); err != nil {
		t.Fatal(err)
	}

	rr = do(t, h, http.MethodPost, PublicPrefix+"/api/assets/"+strconv.FormatInt(id, 10)+"/video-url", bearer)
	if rr.Code != http.StatusOK {
		t.Fatalf("video-url -> %d", rr.Code)
	}
	var body struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body.URL, PublicPrefix+"/api/video/"+strconv.FormatInt(id, 10)+"?exp=") {
		t.Fatalf("signed url without PublicPrefix: %q", body.URL)
	}

	// the signed URL passes the auth exemption (no Bearer) and serves the video
	rr = do(t, h, http.MethodGet, body.URL, "")
	if rr.Code != http.StatusOK || rr.Body.String() != "video-data" {
		t.Fatalf("GET %q without auth -> %d %q", body.URL, rr.Code, rr.Body.String())
	}
	// tampered signature -> 401 from the handler itself
	tampered := strings.Replace(body.URL, "sig=", "sig=0", 1)
	if rr := do(t, h, http.MethodGet, tampered, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("tampered signature -> %d, want 401", rr.Code)
	}
	// expired -> 401
	exp := time.Now().Add(-time.Hour).Unix()
	expired := PublicPrefix + "/api/video/" + strconv.FormatInt(id, 10) + "?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + s.signVideo(id, exp)
	if rr := do(t, h, http.MethodGet, expired, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("expired signature -> %d, want 401", rr.Code)
	}
}

// TestRescanPerOwner: POST /api/admin/rescan enqueues a scan OF THE OWNER of the
// request only (H8 §6).
func TestRescanPerOwner(t *testing.T) {
	s, _, fp := newTestServer(t)
	h := s.Handler()
	for i := 0; i < 3; i++ {
		rr := do(t, h, http.MethodPost, PublicPrefix+"/api/admin/rescan", "Bearer test-token")
		if !strings.Contains(rr.Body.String(), "queued") {
			t.Fatalf("rescan %d: %d %s", i, rr.Code, rr.Body.String())
		}
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.scans) != 3 {
		t.Fatalf("queued scans: %v", fp.scans)
	}
	for _, owner := range fp.scans {
		if owner != "legacy-owner" {
			t.Fatalf("rescan queued for %q, want the owner of the request", owner)
		}
	}
}

// graphFake validates Bearer tokens: "tok-alice" -> id-alice, "tok-bob" -> id-bob.
func graphFake(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graph/v1.0/me" {
			http.NotFound(w, r)
			return
		}
		switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
		case "tok-alice":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "id-alice", "onPremisesSamAccountName": "alice"})
		case "tok-bob":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "id-bob", "onPremisesSamAccountName": "bob"})
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newMultiServer mounts the API with a real Graph validator against a fake.
func newMultiServer(t *testing.T) (*Server, *store.Store, *fakeProvider) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fp := &fakeProvider{}
	gv := auth.NewGraphValidator(graphFake(t).URL, auth.MultiTenant(), slog.Default())
	s := New(st, nil, nil, gv, fp, Config{Secret: []byte("0123456789abcdef0123456789abcdef")}, slog.Default())
	return s, st, fp
}

// TestMultiTenantAPIIsolation: two users authenticated by OIDC do not see each
// other: assets, stats, albums, tags and cross thumb -> 404/empty (H8 §9).
func TestMultiTenantAPIIsolation(t *testing.T) {
	s, st, _ := newMultiServer(t)
	h := s.Handler()
	ctx := context.Background()

	idA, _, _ := st.UpsertByETag(ctx, "id-alice", "/dav/spaces/sa/Fotos/a.jpg", "e1", "a.jpg", "image", time.Now(), 100)
	if _, _, err := st.UpsertByETag(ctx, "id-bob", "/dav/spaces/sb/Fotos/a.jpg", "e2", "a.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTag(ctx, "id-alice", idA, "playa"); err != nil {
		t.Fatal(err)
	}
	alA, _ := st.CreateAlbum(ctx, "id-alice", "Vacaciones")

	// assets: each one only sees its own
	getAssets := func(token string) []store.Asset {
		t.Helper()
		rr := do(t, h, http.MethodGet, PublicPrefix+"/api/assets", "Bearer "+token)
		if rr.Code != http.StatusOK {
			t.Fatalf("assets %s -> %d", token, rr.Code)
		}
		var body struct {
			Assets []store.Asset `json:"assets"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Assets
	}
	if got := getAssets("tok-alice"); len(got) != 1 || got[0].ID != idA {
		t.Fatalf("alice's assets: %+v", got)
	}
	if got := getAssets("tok-bob"); len(got) != 1 || got[0].ID == idA {
		t.Fatalf("bob's assets: %+v", got)
	}

	// thumb / tags / album of a foreign id -> 404 (IDOR rule, no leak)
	for _, p := range []string{
		"/api/assets/" + strconv.FormatInt(idA, 10) + "/thumb",
		"/api/assets/" + strconv.FormatInt(idA, 10) + "/tags",
		"/api/assets/" + strconv.FormatInt(idA, 10) + "/video-url",
		"/api/albums/" + strconv.FormatInt(alA, 10) + "/assets",
	} {
		method := http.MethodGet
		if strings.HasSuffix(p, "video-url") {
			method = http.MethodPost
		}
		rr := do(t, h, method, PublicPrefix+p, "Bearer tok-bob")
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s %s as bob -> %d, want 404", method, p, rr.Code)
		}
	}
	// writing on a foreign id -> 404
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, PublicPrefix+"/api/assets/"+strconv.FormatInt(idA, 10)+"/favorite", strings.NewReader(`{"favorite":true}`))
	req.Header.Set("Authorization", "Bearer tok-bob")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("foreign favorite -> %d, want 404", rr.Code)
	}

	// tags and albums: bob does not see alice's
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/tags", "Bearer tok-bob")
	if !strings.Contains(rr.Body.String(), `"tags":[]`) {
		t.Fatalf("bob's tags: %s", rr.Body.String())
	}
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/albums", "Bearer tok-bob")
	if !strings.Contains(rr.Body.String(), `"albums":[]`) {
		t.Fatalf("bob's albums: %s", rr.Body.String())
	}
	// and alice does see hers (sanity)
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/tags", "Bearer tok-alice")
	if !strings.Contains(rr.Body.String(), "playa") {
		t.Fatalf("alice's tags: %s", rr.Body.String())
	}
}

// TestBasicAuthMultiTenant: Basic app-password also authenticates (H8 §1) and
// the owner is the resolved oc_id.
func TestBasicAuthMultiTenant(t *testing.T) {
	// the fake Graph only validates Bearer, extend it with Basic
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); ok && u == "alice" && p == "app-token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "id-alice", "onPremisesSamAccountName": "alice"})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	fp := &fakeProvider{}
	gv := auth.NewGraphValidator(srv.URL, auth.MultiTenant(), slog.Default())
	s := New(st, nil, nil, gv, fp, Config{Secret: []byte("x")}, slog.Default())
	h := s.Handler()

	ctx := context.Background()
	if _, _, err := st.UpsertByETag(ctx, "id-alice", "/dav/x/a.jpg", "e1", "a.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, PublicPrefix+"/api/stats", nil)
	req.SetBasicAuth("alice", "app-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"assets":1`) {
		t.Fatalf("stats with Basic -> %d %s", rr.Code, rr.Body.String())
	}
	// and Touch received the activity of id-alice
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.touched) != 1 || fp.touched[0] != "id-alice" {
		t.Fatalf("touched: %v", fp.touched)
	}
}

// TestFoldersWithoutSpace: a user without a resolved space gets a coherent
// empty answer, not a 500 (H8 §6).
func TestFoldersWithoutSpace(t *testing.T) {
	s, _, fp := newMultiServer(t)
	fp.prefix = "" // unresolvable space
	h := s.Handler()
	rr := do(t, h, http.MethodGet, PublicPrefix+"/api/folders", "Bearer tok-alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("folders without space -> %d, want an empty 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"folders":[]`) || !strings.Contains(rr.Body.String(), `"assets":[]`) {
		t.Fatalf("coherent empty answer: %s", rr.Body.String())
	}
	// with a resolved space it uses the prefix of the session
	fp.prefix = "/dav/spaces/sa"
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/folders", "Bearer tok-alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("folders with space -> %d", rr.Code)
	}
}

// TestFavoriteAndTagWriteSidecar: after changing the favourite flag or a tag,
// the handler writes the XMP package next to the photo (state derived from the
// index).
func TestFavoriteAndTagWriteSidecar(t *testing.T) {
	s, st, fp := newMultiServer(t)
	var mu sync.Mutex
	var puts []struct{ path, body string }
	davSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		puts = append(puts, struct{ path, body string }{r.URL.Path, string(b)})
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer davSrv.Close()
	fp.dav = webdav.New(davSrv.URL, "user", "token")

	h := s.Handler()
	id, _, err := st.UpsertByETag(context.Background(), "id-alice", "/dav/spaces/sa/Fotos/a.jpg", "e1", "a.jpg", "image", time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	asset := strconv.FormatInt(id, 10)
	post := func(route, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, PublicPrefix+route, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer tok-alice")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	if rr := post("/api/assets/"+asset+"/favorite", `{"favorite":true}`); rr.Code != http.StatusNoContent {
		t.Fatalf("favorite -> %d", rr.Code)
	}
	mu.Lock()
	if len(puts) != 1 {
		mu.Unlock()
		t.Fatalf("PUTs after the favourite: %d, want 1", len(puts))
	}
	first := puts[0]
	mu.Unlock()
	if first.path != "/dav/spaces/sa/Fotos/a.jpg.xmp" {
		t.Errorf("sidecar path: %q", first.path)
	}
	if !strings.Contains(first.body, "<xmp:Rating>5</xmp:Rating>") {
		t.Errorf("Rating 5 missing: %s", first.body)
	}

	if rr := post("/api/assets/"+asset+"/tags", `{"tag":"playa"}`); rr.Code != http.StatusNoContent {
		t.Fatalf("addTag -> %d", rr.Code)
	}
	mu.Lock()
	if len(puts) != 2 {
		mu.Unlock()
		t.Fatalf("PUTs after the tag: %d, want 2", len(puts))
	}
	second := puts[1]
	mu.Unlock()
	if !strings.Contains(second.body, "<rdf:li>playa</rdf:li>") {
		t.Errorf("tag missing in the sidecar: %s", second.body)
	}
	if !strings.Contains(second.body, "<xmp:Rating>5</xmp:Rating>") {
		t.Errorf("the favourite flag was lost when writing the tag: %s", second.body)
	}
}

// TestExifBacklog: without a worker the route responds 503 and without a DAV
// session as well.
func TestExifBacklog(t *testing.T) {
	s, st2, fp := newTestServer(t)
	h := s.Handler()
	bearer := "Bearer test-token"

	rr := do(t, h, http.MethodPost, "/ocphotos-api/api/admin/exif", bearer)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("without worker: %d, want 503", rr.Code)
	}

	fp.dav = nil // no DAV session: the route must clean the state and respond 503
	s.SetExifWorker(exif.NewWorker(st2, slog.Default()))
	rr = do(t, h, http.MethodPost, "/ocphotos-api/api/admin/exif", bearer)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("without session: %d, want 503", rr.Code)
	}
}

// TestSpacesEndpoint: the space filter of the UI (issue #145) is fed by the
// index: the spaces with photos of the owner and their count.
func TestSpacesEndpoint(t *testing.T) {
	s, st, _ := newMultiServer(t)
	h := s.Handler()
	ctx := context.Background()
	if _, _, err := st.UpsertInSpace(ctx, "id-alice", "/dav/spaces/sa/Fotos/a.jpg", "e1", "a.jpg", "image", time.Now(), 100,
		store.Space{ID: "p1", Name: "Personal", Type: "personal"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.UpsertInSpace(ctx, "id-alice", "/dav/spaces/sk/Fotos/b.jpg", "e2", "b.jpg", "image", time.Now(), 100,
		store.Space{ID: "k1", Name: "Familia", Type: "project"}); err != nil {
		t.Fatal(err)
	}

	rr := do(t, h, http.MethodGet, PublicPrefix+"/api/spaces", "Bearer tok-alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("spaces -> %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{`"id":"p1"`, `"id":"k1"`, `"name":"Familia"`, `"type":"project"`, `"count":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("spaces body misses %s: %s", want, body)
		}
	}

	// Filter by space in the timeline
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/assets?space=k1", "Bearer tok-alice")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"spaceName":"Familia"`) {
		t.Fatalf("assets?space=k1 -> %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"spaceName":"Personal"`) {
		t.Errorf("the filter let other space through: %s", rr.Body.String())
	}
}

// TestFoldersBySpace: the folders handler resolves the prefix of the requested
// space, and an unknown one answers with a coherent empty listing instead of a
// 500 (issue #145).
func TestFoldersBySpace(t *testing.T) {
	s, _, fp := newMultiServer(t)
	h := s.Handler()
	fp.spacePrefixes = map[string]string{"k1": "/dav/spaces/sk"}
	rr := do(t, h, http.MethodGet, PublicPrefix+"/api/folders?space=k1", "Bearer tok-alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("folders?space=k1 -> %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"space":"k1"`) {
		t.Errorf("the answer does not echo the space: %s", rr.Body.String())
	}
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/folders?space=nope", "Bearer tok-alice")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"folders":[]`) {
		t.Fatalf("folders?space=nope -> %d: %s", rr.Code, rr.Body.String())
	}
}

// TestExifBacklogAll: POST /api/admin/exif?all=1 queues every image of the owner
// again (the backfill after the extractor changes, issue #75) and says how many.
func TestExifBacklogAll(t *testing.T) {
	s, st, fp := newTestServer(t)
	h := s.Handler()
	bearer := "Bearer test-token"
	ctx := context.Background()
	if _, _, err := st.UpsertByETag(ctx, "legacy-owner", "/dav/spaces/s/Fotos/a.jpg", "e1", "a.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.UpsertByETag(ctx, "legacy-owner", "/dav/spaces/s/Fotos/b.jpg", "e2", "b.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	// a 404 upstream: the worker marks them processed without touching anything
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	fp.dav = webdav.New(srv.URL, "u", "t")
	s.SetExifWorker(exif.NewWorker(st, slog.Default()))

	rr := do(t, h, http.MethodPost, "/ocphotos-api/api/admin/exif?all=1", bearer)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"queued":2`) {
		t.Errorf("the answer does not say how many assets were queued: %s", rr.Body.String())
	}
}
