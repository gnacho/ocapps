package api

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/gnacho/ocapps/internal/photos/store"
)

// fakeProvider implementa SessionProvider para tests (sin registry real).
type fakeProvider struct {
	mu      sync.Mutex
	legacy  string // owner del token estático; "" = error
	dav     *webdav.Client
	prefix  string
	touched []string
	scans   []string
}

func (f *fakeProvider) Touch(_ context.Context, u *auth.User, _ auth.Credential) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, u.ID)
}
func (f *fakeProvider) DavFor(_ context.Context, _ string) *webdav.Client { return f.dav }
func (f *fakeProvider) SpacePrefix(_ context.Context, _ string) string    { return f.prefix }
func (f *fakeProvider) LegacyOwner(_ context.Context) (string, error) {
	if f.legacy == "" {
		return "", errors.New("legacy no configurado")
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
	t.Cleanup(func() { st.Close() })
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

// TestRutasBajoPublicPrefix: todas las rutas cuelgan de PublicPrefix
// (SPEC §4.3) y el prefijo es obligatorio (sin strip de proxy).
func TestRutasBajoPublicPrefix(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	bearer := "Bearer test-token"

	// con auth: las rutas existen bajo el prefijo
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

	// sin prefijo: no hay ruta (el proxy ya no strip-pea)
	if rr := do(t, h, http.MethodGet, "/api/stats", bearer); rr.Code != http.StatusNotFound {
		t.Fatalf("GET /api/stats sin prefijo -> %d, quiero 404", rr.Code)
	}

	// Q7: endpoints muertos eliminados (sin consumidor en la extensión)
	for _, p := range []string{
		"/api/thumb?path=/Fotos/IMG.heic", // thumb por path
		"/api/assets/1",                   // asset por id (bare GET)
		"/api/assets/1/hls/index.m3u8",    // HLS
	} {
		rr := do(t, h, http.MethodGet, PublicPrefix+p, bearer)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("GET %s%s -> %d, quiero 404 (endpoint eliminado en Q7)", PublicPrefix, p, rr.Code)
		}
	}

	// sin auth: 401 (salvo video firmado y preflight)
	if rr := do(t, h, http.MethodGet, PublicPrefix+"/api/stats", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("stats sin auth -> %d, quiero 401", rr.Code)
	}
	if rr := do(t, h, http.MethodGet, PublicPrefix+"/api/stats", "Bearer otro"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("stats con token erróneo -> %d, quiero 401", rr.Code)
	}

	// preflight OPTIONS sin auth: 204 (CORS común, SPEC §4.4)
	rr := do(t, h, http.MethodOptions, PublicPrefix+"/api/stats", "")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS -> %d, quiero 204", rr.Code)
	}
	if acao := rr.Header().Get("Access-Control-Allow-Origin"); acao != "*" {
		t.Fatalf("ACAO = %q", acao)
	}
}

// TestTokenEstaticoOwnerLegacy: el Bearer estático DEPRECATED mapea al owner
// legacy (oc_id de OCAPPS_PHOTOS_USER), H8 §6.2.
func TestTokenEstaticoOwnerLegacy(t *testing.T) {
	s, st, _ := newTestServer(t)
	h := s.Handler()
	ctx := context.Background()
	// un asset del owner legacy y otro de otro owner: el token estático solo
	// ve los suyos
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
		t.Fatalf("el token estático debe ver solo lo del owner legacy: %+v", body.Assets)
	}
	// token por query param (compat) también mapea
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/stats?token=test-token", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("stats?token -> %d", rr.Code)
	}
}

// TestVideoFirmado: la firma usa PublicPrefix y la ruta firmada está exenta
// de auth (la firma HMAC es la credencial). video-url verifica ownership
// ANTES de firmar (H8).
func TestVideoFirmado(t *testing.T) {
	// DAV falso que sirve el contenido del vídeo
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

	// sin asset del owner: video-url -> 404 (no se firma lo ajeno/inexistente)
	rr := do(t, h, http.MethodPost, PublicPrefix+"/api/assets/42/video-url", bearer)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("video-url de asset inexistente -> %d, quiero 404", rr.Code)
	}

	id, _, err := st.UpsertByETag(ctx, "legacy-owner", "/dav/x/v.mp4", "e1", "v.mp4", "video", time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	// un asset con el mismo path de OTRO owner no sirve para firmar
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
		t.Fatalf("url firmada sin PublicPrefix: %q", body.URL)
	}

	// la URL firmada pasa la exemption de auth (sin Bearer) y sirve el vídeo
	rr = do(t, h, http.MethodGet, body.URL, "")
	if rr.Code != http.StatusOK || rr.Body.String() != "video-data" {
		t.Fatalf("GET %q sin auth -> %d %q", body.URL, rr.Code, rr.Body.String())
	}
	// firma manipulada -> 401 del propio handler
	tampered := strings.Replace(body.URL, "sig=", "sig=0", 1)
	if rr := do(t, h, http.MethodGet, tampered, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("firma manipulada -> %d, quiero 401", rr.Code)
	}
	// expirada -> 401
	exp := time.Now().Add(-time.Hour).Unix()
	expired := PublicPrefix + "/api/video/" + strconv.FormatInt(id, 10) + "?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + s.signVideo(id, exp)
	if rr := do(t, h, http.MethodGet, expired, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("firma expirada -> %d, quiero 401", rr.Code)
	}
}

// TestRescanPorOwner: POST /api/admin/rescan encola scan SOLO del owner del
// request (H8 §6).
func TestRescanPorOwner(t *testing.T) {
	s, _, fp := newTestServer(t)
	h := s.Handler()
	for i := 0; i < 3; i++ {
		rr := do(t, h, http.MethodPost, PublicPrefix+"/api/admin/rescan", "Bearer test-token")
		if !strings.Contains(rr.Body.String(), "encolado") {
			t.Fatalf("rescan %d: %d %s", i, rr.Code, rr.Body.String())
		}
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.scans) != 3 {
		t.Fatalf("scans encolados: %v", fp.scans)
	}
	for _, owner := range fp.scans {
		if owner != "legacy-owner" {
			t.Fatalf("rescan encolado para %q, quiero el owner del request", owner)
		}
	}
}

// graphFake valida Bearer tokens: "tok-alice" -> id-alice, "tok-bob" -> id-bob.
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

// newMultiServer monta la API con validador Graph real contra un fake.
func newMultiServer(t *testing.T) (*Server, *store.Store, *fakeProvider) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fp := &fakeProvider{}
	gv := auth.NewGraphValidator(graphFake(t).URL, auth.MultiTenant(), slog.Default())
	s := New(st, nil, nil, gv, fp, Config{Secret: []byte("0123456789abcdef0123456789abcdef")}, slog.Default())
	return s, st, fp
}

// TestMultiTenantAislamientoAPI: dos usuarios autenticados por OIDC no se
// ven: assets, stats, álbumes, tags y thumb cruzado -> 404/vacío (H8 §9).
func TestMultiTenantAislamientoAPI(t *testing.T) {
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

	// assets: cada uno solo ve el suyo
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
		t.Fatalf("assets de alice: %+v", got)
	}
	if got := getAssets("tok-bob"); len(got) != 1 || got[0].ID == idA {
		t.Fatalf("assets de bob: %+v", got)
	}

	// thumb / tags / álbum de un id ajeno -> 404 (regla IDOR, sin delatar)
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
			t.Fatalf("%s %s como bob -> %d, quiero 404", method, p, rr.Code)
		}
	}
	// escribir sobre un id ajeno -> 404
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, PublicPrefix+"/api/assets/"+strconv.FormatInt(idA, 10)+"/favorite", strings.NewReader(`{"favorite":true}`))
	req.Header.Set("Authorization", "Bearer tok-bob")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("favorite ajeno -> %d, quiero 404", rr.Code)
	}

	// tags y álbumes: bob no ve los de alice
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/tags", "Bearer tok-bob")
	if !strings.Contains(rr.Body.String(), `"tags":[]`) {
		t.Fatalf("tags de bob: %s", rr.Body.String())
	}
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/albums", "Bearer tok-bob")
	if !strings.Contains(rr.Body.String(), `"albums":[]`) {
		t.Fatalf("albums de bob: %s", rr.Body.String())
	}
	// y alice sí ve los suyos (sanity)
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/tags", "Bearer tok-alice")
	if !strings.Contains(rr.Body.String(), "playa") {
		t.Fatalf("tags de alice: %s", rr.Body.String())
	}
}

// TestBasicAuthMultiTenant: Basic app-password también autentica (H8 §1) y el
// owner es el oc_id resuelto.
func TestBasicAuthMultiTenant(t *testing.T) {
	// el fake Graph solo valida Bearer; extiéndelo con Basic
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
	defer st.Close()
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
		t.Fatalf("stats con Basic -> %d %s", rr.Code, rr.Body.String())
	}
	// y Touch recibió la actividad de id-alice
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.touched) != 1 || fp.touched[0] != "id-alice" {
		t.Fatalf("touched: %v", fp.touched)
	}
}

// TestFoldersSinEspacio: un usuario sin espacio resuelto recibe respuesta
// vacía coherente, no un 500 (H8 §6).
func TestFoldersSinEspacio(t *testing.T) {
	s, _, fp := newMultiServer(t)
	fp.prefix = "" // espacio no resoluble
	h := s.Handler()
	rr := do(t, h, http.MethodGet, PublicPrefix+"/api/folders", "Bearer tok-alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("folders sin espacio -> %d, quiero 200 vacío", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"folders":[]`) || !strings.Contains(rr.Body.String(), `"assets":[]`) {
		t.Fatalf("respuesta vacía coherente: %s", rr.Body.String())
	}
	// con espacio resuelto usa el prefijo de la sesión
	fp.prefix = "/dav/spaces/sa"
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/folders", "Bearer tok-alice")
	if rr.Code != http.StatusOK {
		t.Fatalf("folders con espacio -> %d", rr.Code)
	}
}
