package api

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
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

// newThumbTestServer: like newTestServer but with the thumbnail service pointing
// to a temporary cache and the given fake DAV.
func newThumbTestServer(t *testing.T, dav *webdav.Client) (*Server, *store.Store, *fakeProvider) {
	t.Helper()
	th, err := thumb.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fp := &fakeProvider{legacy: "legacy-owner", dav: dav}
	s := New(st, th, nil, nil, fp,
		Config{Token: "test-token", Secret: []byte("0123456789abcdef0123456789abcdef")},
		slog.Default())
	return s, st, fp
}

// assetOf: the asset of the legacy owner by id (to derive the expected etag).
func assetDe(t *testing.T, st *store.Store, id int64) store.Asset {
	t.Helper()
	a, err := st.AssetByID(context.Background(), "legacy-owner", id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestThumbETagRevalidation: the thumbnail carries an ETag derived from the
// cache key (with the generation salt) and a revalidable Cache-Control, a
// matching If-None-Match responds 304 with no body. That way the browser
// refreshes the thumbnails when the backend changes the generation (cache-key
// salt) instead of keeping the old ones for up to 30 days (immutable).
func TestThumbETagRevalidation(t *testing.T) {
	// small JPEG for the fake DAV
	src := image.NewRGBA(image.Rect(0, 0, 8, 6))
	src.Set(3, 2, color.RGBA{R: 200, A: 255})
	var photo bytes.Buffer
	if err := jpeg.Encode(&photo, src, nil); err != nil {
		t.Fatal(err)
	}
	davSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(photo.Bytes())
	}))
	defer davSrv.Close()

	s, st, _ := newThumbTestServer(t, webdav.New(davSrv.URL, "u", "t"))
	h := s.Handler()
	bearer := "Bearer test-token"
	ctx := context.Background()

	id, _, err := st.UpsertByETag(ctx, "legacy-owner", "/dav/x/a.jpg", "e1", "a.jpg", "image", time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	url := PublicPrefix + "/api/assets/" + strconv.FormatInt(id, 10) + "/thumb?w=400"

	// first request: 200 with ETag and a revalidable policy
	rr := do(t, h, http.MethodGet, url, bearer)
	if rr.Code != http.StatusOK {
		t.Fatalf("thumb -> %d, want 200", rr.Code)
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Fatal("thumb without ETag")
	}
	cc := rr.Header().Get("Cache-Control")
	if !strings.Contains(cc, "must-revalidate") || strings.Contains(cc, "immutable") {
		t.Fatalf("Cache-Control = %q, want revalidable without immutable", cc)
	}
	// the ETag derives from the cache key: it changes if the generation changes
	if want := thumb.ETag("/dav/x/a.jpg", etagFor(assetDe(t, st, id)), 400); etag != want {
		t.Fatalf("ETag = %s, want %s (derived from the cache key)", etag, want)
	}

	// revalidation: matching If-None-Match -> 304 with no body
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", bearer)
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("revalidation -> %d, want 304", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("304 with a body of %d bytes", rr.Body.Len())
	}
	if rr.Header().Get("Cache-Control") != cc {
		t.Fatalf("304 without Cache-Control: %q", rr.Header().Get("Cache-Control"))
	}

	// different If-None-Match -> 200 again
	req = httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", bearer)
	req.Header.Set("If-None-Match", `"otro"`)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("different If-None-Match -> %d, want 200", rr.Code)
	}
}

// TestVideoPoster304: the video poster also responds 304 by ETag, and it does so
// BEFORE generating (with no ffmpeg in the test environment, without the check
// the handler would fall back to the 302 of the original).
func TestVideoPoster304(t *testing.T) {
	davSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("video-data"))
	}))
	defer davSrv.Close()

	s, st, _ := newThumbTestServer(t, webdav.New(davSrv.URL, "u", "t"))
	h := s.Handler()
	bearer := "Bearer test-token"
	ctx := context.Background()

	id, _, err := st.UpsertByETag(ctx, "legacy-owner", "/dav/x/v.mp4", "e1", "v.mp4", "video", time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	url := PublicPrefix + "/api/assets/" + strconv.FormatInt(id, 10) + "/thumb"

	etag := thumb.VideoETag("/dav/x/v.mp4", etagFor(assetDe(t, st, id)), 400)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", bearer)
	req.Header.Set("If-None-Match", etag)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("poster revalidated -> %d, want 304 (without going through ffmpeg)", rr.Code)
	}
}
