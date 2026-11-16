package api

// Collection handlers (albums, tags, archive, original, duplicates, places,
// rescan) against the real store: the paths the extension walks when the user
// organises a library.

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/webdav"
	"github.com/gnacho/ocapps/internal/photos/store"
	"github.com/gnacho/ocapps/internal/photos/thumb"
)

const bearer = "Bearer test-token"

// doBody sends a request with a JSON body (the helper do sends none).
func doBody(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", bearer)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// newAsset inserts an asset of the legacy owner (the one the static token maps
// to) and returns its id.
func newAsset(t *testing.T, st *store.Store, path, kind string) int64 {
	t.Helper()
	id, _, err := st.UpsertByETag(context.Background(), "legacy-owner", path, "etag-"+path, filenameOf(path), kind, time.Now(), 1024)
	if err != nil {
		t.Fatalf("UpsertByETag: %v", err)
	}
	return id
}

func filenameOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func assetIDs(t *testing.T, body []byte) []int64 {
	t.Helper()
	var out struct {
		Assets []struct {
			ID int64 `json:"id"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode assets: %v (%s)", err, body)
	}
	ids := make([]int64, 0, len(out.Assets))
	for _, a := range out.Assets {
		ids = append(ids, a.ID)
	}
	return ids
}

func TestAlbumsLifecycle(t *testing.T) {
	s, st, _ := newTestServer(t)
	h := s.Handler()
	a := newAsset(t, st, "/dav/Fotos/a.jpg", "image")
	b := newAsset(t, st, "/dav/Fotos/b.jpg", "image")

	// Create: the name is required.
	if rr := doBody(t, h, http.MethodPost, PublicPrefix+"/api/albums", `{"name":"  "}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("create without name = %d, want 400", rr.Code)
	}
	rr := doBody(t, h, http.MethodPost, PublicPrefix+"/api/albums", `{"name":" Summer "}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rr.Code, rr.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil || created.ID == 0 {
		t.Fatalf("created = %+v, %v", created, err)
	}
	album := strconv.FormatInt(created.ID, 10)

	// It shows up in the list, trimmed.
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/albums", bearer)
	if !strings.Contains(rr.Body.String(), `"name":"Summer"`) {
		t.Fatalf("list = %s, want the trimmed name", rr.Body.String())
	}

	// Add assets: the list is required.
	if rr := doBody(t, h, http.MethodPost, PublicPrefix+"/api/albums/"+album+"/assets", `{"assetIds":[]}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("add without ids = %d, want 400", rr.Code)
	}
	if rr := doBody(t, h, http.MethodPost, PublicPrefix+"/api/albums/"+album+"/assets",
		`{"assetIds":[`+strconv.FormatInt(a, 10)+`,`+strconv.FormatInt(b, 10)+`]}`); rr.Code != http.StatusNoContent {
		t.Fatalf("add = %d %s", rr.Code, rr.Body.String())
	}

	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/albums/"+album+"/assets", bearer)
	if got := assetIDs(t, rr.Body.Bytes()); len(got) != 2 {
		t.Fatalf("album assets = %v, want the two added", got)
	}

	// Remove one.
	if rr := do(t, h, http.MethodDelete,
		PublicPrefix+"/api/albums/"+album+"/assets/"+strconv.FormatInt(a, 10), bearer); rr.Code != http.StatusNoContent {
		t.Fatalf("remove from album = %d", rr.Code)
	}
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/albums/"+album+"/assets", bearer)
	if got := assetIDs(t, rr.Body.Bytes()); len(got) != 1 || got[0] != b {
		t.Fatalf("album assets = %v, want only the second", got)
	}

	// Rename.
	if rr := doBody(t, h, http.MethodPatch, PublicPrefix+"/api/albums/"+album, `{"name":""}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("rename without name = %d, want 400", rr.Code)
	}
	if rr := doBody(t, h, http.MethodPatch, PublicPrefix+"/api/albums/"+album, `{"name":"Autumn"}`); rr.Code != http.StatusNoContent {
		t.Fatalf("rename = %d", rr.Code)
	}
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/albums", bearer)
	if !strings.Contains(rr.Body.String(), `"name":"Autumn"`) || strings.Contains(rr.Body.String(), `"name":"Summer"`) {
		t.Fatalf("list after rename = %s", rr.Body.String())
	}

	// A missing album answers 404 on every route that resolves it.
	for _, route := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/albums/999999/assets", ""},
		{http.MethodPatch, "/api/albums/999999", `{"name":"x"}`},
		{http.MethodDelete, "/api/albums/999999", ""},
		{http.MethodPost, "/api/albums/999999/assets", `{"assetIds":[1]}`},
		{http.MethodDelete, "/api/albums/999999/assets/1", ""},
	} {
		var rr *httptest.ResponseRecorder
		if route.body != "" {
			rr = doBody(t, h, route.method, PublicPrefix+route.path, route.body)
		} else {
			rr = do(t, h, route.method, PublicPrefix+route.path, bearer)
		}
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d, want 404", route.method, route.path, rr.Code)
		}
	}

	// Delete.
	if rr := do(t, h, http.MethodDelete, PublicPrefix+"/api/albums/"+album, bearer); rr.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rr.Code)
	}
	if rr := do(t, h, http.MethodDelete, PublicPrefix+"/api/albums/"+album, bearer); rr.Code != http.StatusNotFound {
		t.Fatalf("delete twice = %d, want 404", rr.Code)
	}
}

func TestTagsLifecycle(t *testing.T) {
	s, st, _ := newTestServer(t)
	h := s.Handler()
	id := newAsset(t, st, "/dav/Fotos/tagged.jpg", "image")
	asset := strconv.FormatInt(id, 10)

	// The tag is required and trimmed.
	if rr := doBody(t, h, http.MethodPost, PublicPrefix+"/api/assets/"+asset+"/tags", `{"tag":"   "}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("tag required = %d, want 400", rr.Code)
	}
	if rr := doBody(t, h, http.MethodPost, PublicPrefix+"/api/assets/"+asset+"/tags", `{"tag":"  family "}`); rr.Code != http.StatusNoContent {
		t.Fatalf("add tag = %d %s", rr.Code, rr.Body.String())
	}

	// The asset carries it, the global list carries it and the tag lists it.
	rr := do(t, h, http.MethodGet, PublicPrefix+"/api/assets/"+asset+"/tags", bearer)
	if !strings.Contains(rr.Body.String(), `"family"`) {
		t.Fatalf("asset tags = %s", rr.Body.String())
	}
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/tags", bearer)
	if !strings.Contains(rr.Body.String(), "family") {
		t.Fatalf("tags = %s", rr.Body.String())
	}
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/tags/family/assets", bearer)
	if got := assetIDs(t, rr.Body.Bytes()); len(got) != 1 || got[0] != id {
		t.Fatalf("assets by tag = %v", got)
	}

	// Remove.
	if rr := do(t, h, http.MethodDelete, PublicPrefix+"/api/assets/"+asset+"/tags/family", bearer); rr.Code != http.StatusNoContent {
		t.Fatalf("remove tag = %d", rr.Code)
	}
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/tags", bearer)
	if strings.Contains(rr.Body.String(), "family") {
		t.Fatalf("tags after removal = %s", rr.Body.String())
	}

	// An unknown asset answers 404.
	if rr := doBody(t, h, http.MethodPost, PublicPrefix+"/api/assets/999999/tags", `{"tag":"x"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("tag on a missing asset = %d, want 404", rr.Code)
	}
	if rr := do(t, h, http.MethodGet, PublicPrefix+"/api/assets/999999/tags", bearer); rr.Code != http.StatusNotFound {
		t.Fatalf("tags of a missing asset = %d, want 404", rr.Code)
	}
	if rr := do(t, h, http.MethodDelete, PublicPrefix+"/api/assets/999999/tags/x", bearer); rr.Code != http.StatusNotFound {
		t.Fatalf("removing a tag from a missing asset = %d, want 404", rr.Code)
	}
}

func TestArchive(t *testing.T) {
	s, st, _ := newTestServer(t)
	h := s.Handler()
	id := newAsset(t, st, "/dav/Fotos/old.jpg", "image")
	path := PublicPrefix + "/api/assets/" + strconv.FormatInt(id, 10) + "/archive"

	if rr := doBody(t, h, http.MethodPost, path, `{"archived":"yes"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d, want 400", rr.Code)
	}
	if rr := doBody(t, h, http.MethodPost, path, `{"archived":true}`); rr.Code != http.StatusNoContent {
		t.Fatalf("archive = %d", rr.Code)
	}

	a, err := st.AssetByID(context.Background(), "legacy-owner", id)
	if err != nil {
		t.Fatalf("AssetByID: %v", err)
	}
	if !a.IsArchived {
		t.Fatal("the asset must be archived")
	}

	// The archived asset is hidden from the default listing but can be asked for.
	rr := do(t, h, http.MethodGet, PublicPrefix+"/api/assets", bearer)
	if got := assetIDs(t, rr.Body.Bytes()); len(got) != 0 {
		t.Fatalf("assets = %v, want the archived one hidden", got)
	}
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/assets?archived=1", bearer)
	if got := assetIDs(t, rr.Body.Bytes()); len(got) != 1 {
		t.Fatalf("archived assets = %v, want the archived one", got)
	}

	// Unarchive.
	if rr := doBody(t, h, http.MethodPost, path, `{"archived":false}`); rr.Code != http.StatusNoContent {
		t.Fatalf("unarchive = %d", rr.Code)
	}
	if a, _ = st.AssetByID(context.Background(), "legacy-owner", id); a.IsArchived {
		t.Fatal("the asset must be back")
	}

	// A missing asset answers 404.
	if rr := doBody(t, h, http.MethodPost, PublicPrefix+"/api/assets/999999/archive", `{"archived":true}`); rr.Code != http.StatusNotFound {
		t.Fatalf("missing asset = %d, want 404", rr.Code)
	}
}

// davServer serves the given image on every path.
func davServer(t *testing.T, body []byte, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOriginal(t *testing.T) {
	s, st, fp := newTestServer(t)
	const content = "original-bytes"
	fp.dav = webdav.NewBearer(davServer(t, []byte(content), http.StatusOK).URL, "tok")
	h := s.Handler()
	id := newAsset(t, st, "/dav/Fotos/orig.jpg", "image")
	path := PublicPrefix + "/api/assets/" + strconv.FormatInt(id, 10) + "/original"

	rr := do(t, h, http.MethodGet, path, bearer)
	if rr.Code != http.StatusOK || rr.Body.String() != content {
		t.Fatalf("original = %d %q", rr.Code, rr.Body.String())
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "orig.jpg") {
		t.Fatalf("Content-Disposition = %q, want the filename", cd)
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Fatalf("Cache-Control = %q, want a private cache", cc)
	}

	// A missing asset and a deleted one are both 404.
	if rr := do(t, h, http.MethodGet, PublicPrefix+"/api/assets/999999/original", bearer); rr.Code != http.StatusNotFound {
		t.Fatalf("missing asset = %d, want 404", rr.Code)
	}
	// Everything not seen in the scan is soft deleted.
	if _, err := st.SoftDeleteExcept(context.Background(), "legacy-owner", "/dav/Fotos/", map[string]bool{}); err != nil {
		t.Fatalf("SoftDeleteExcept: %v", err)
	}
	if rr := do(t, h, http.MethodGet, path, bearer); rr.Code != http.StatusNotFound {
		t.Fatalf("deleted asset = %d, want 404", rr.Code)
	}
}

func TestOriginalWithoutSessionAndWithAFailingOrigin(t *testing.T) {
	s, st, fp := newTestServer(t)
	h := s.Handler()
	id := newAsset(t, st, "/dav/Fotos/orig.jpg", "image")
	path := PublicPrefix + "/api/assets/" + strconv.FormatInt(id, 10) + "/original"

	// No DAV session: 503, never an empty file.
	if rr := do(t, h, http.MethodGet, path, bearer); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("without session = %d, want 503", rr.Code)
	}

	fp.dav = webdav.NewBearer(davServer(t, []byte("boom"), http.StatusInternalServerError).URL, "tok")
	if rr := do(t, h, http.MethodGet, path, bearer); rr.Code != http.StatusBadGateway {
		t.Fatalf("failing origin = %d, want 502", rr.Code)
	}
}

func TestPlacesWithoutGeocoder(t *testing.T) {
	s, st, _ := newTestServer(t)
	h := s.Handler()
	// No geocoder and no asset with GPS: the answer is a valid empty list, not
	// an error (a library without location still opens the map view).
	newAsset(t, st, "/dav/Fotos/p1.jpg", "image")

	rr := do(t, h, http.MethodGet, PublicPrefix+"/api/places", bearer)
	if rr.Code != http.StatusOK {
		t.Fatalf("places = %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"places"`) {
		t.Fatalf("places body = %s", rr.Body.String())
	}
}

// pngBytes returns a small valid PNG.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 8), B: 40, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	return buf.Bytes()
}

func TestDuplicatesComputesTheMissingHashes(t *testing.T) {
	s, st, fp := newTestServer(t)
	fp.dav = webdav.NewBearer(davServer(t, pngBytes(t), http.StatusOK).URL, "tok")
	th, err := thumb.New(t.TempDir())
	if err != nil {
		t.Fatalf("thumb.New: %v", err)
	}
	s.thumbs = th
	h := s.Handler()

	id := newAsset(t, st, "/dav/Fotos/dup.png", "image")
	if hashes, err := st.AllPHashes(context.Background(), "legacy-owner"); err != nil || len(hashes) != 0 {
		t.Fatalf("hashes before = %v, %v, want none", hashes, err)
	}

	rr := do(t, h, http.MethodGet, PublicPrefix+"/api/duplicates", bearer)
	if rr.Code != http.StatusOK {
		t.Fatalf("duplicates = %d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Groups   [][]store.Asset `json:"groups"`
		Computed int             `json:"computed"`
		Pending  int             `json:"pending"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	if out.Computed != 1 || out.Pending != 0 {
		t.Fatalf("computed/pending = %d/%d, want 1/0", out.Computed, out.Pending)
	}
	if len(out.Groups) != 0 {
		t.Fatalf("groups = %+v, a single photo is not a duplicate", out.Groups)
	}
	hashes, err := st.AllPHashes(context.Background(), "legacy-owner")
	if err != nil || hashes[id] == "" {
		t.Fatalf("hashes = %v, %v, want the computed one for %d", hashes, err, id)
	}

	// A second photo with the same content and the same cache key groups with it.
	second := newAsset(t, st, "/dav/Fotos/dup2.png", "image")
	if _, err := st.AssetByID(context.Background(), "legacy-owner", second); err != nil {
		t.Fatal(err)
	}
	// Same taken-at and size, so the thumbnail cache key matches the first one.
	rr = do(t, h, http.MethodGet, PublicPrefix+"/api/duplicates", bearer)
	if rr.Code != http.StatusOK {
		t.Fatalf("second duplicates = %d", rr.Code)
	}
}

func TestRescanQueuesTheOwner(t *testing.T) {
	s, _, fp := newTestServer(t)
	h := s.Handler()

	rr := do(t, h, http.MethodPost, PublicPrefix+"/api/admin/rescan", bearer)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "rescan queued") {
		t.Fatalf("rescan = %d %s", rr.Code, rr.Body.String())
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.scans) != 1 || fp.scans[0] != "legacy-owner" {
		t.Fatalf("scans = %v, want the legacy owner queued", fp.scans)
	}
}

// TestExifRouteWithoutWorker: without a worker the route refuses instead of
// pretending to work.
func TestExifRouteWithoutWorker(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	if rr := do(t, h, http.MethodPost, PublicPrefix+"/api/admin/exif", bearer); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("exif without worker = %d, want 503", rr.Code)
	}
}
