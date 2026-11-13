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

// newThumbTestServer: como newTestServer pero con el servicio de miniaturas
// apuntando a una caché temporal y el DAV falso indicado.
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

// assetDe: el asset del owner legacy por id (para derivar el etag esperado).
func assetDe(t *testing.T, st *store.Store, id int64) store.Asset {
	t.Helper()
	a, err := st.AssetByID(context.Background(), "legacy-owner", id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestThumbETagRevalidacion: la miniatura lleva ETag derivado de la cache key
// (con la sal de generación) y Cache-Control revalidable; un If-None-Match
// que coincide responde 304 sin cuerpo. Así el navegador refresca las
// miniaturas cuando el backend cambia la generación (cache-key salt) en
// vez de quedarse con las viejas hasta 30 días (immutable).
func TestThumbETagRevalidacion(t *testing.T) {
	// JPEG pequeño para el DAV falso
	src := image.NewRGBA(image.Rect(0, 0, 8, 6))
	src.Set(3, 2, color.RGBA{R: 200, A: 255})
	var foto bytes.Buffer
	if err := jpeg.Encode(&foto, src, nil); err != nil {
		t.Fatal(err)
	}
	davSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(foto.Bytes())
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

	// primera petición: 200 con ETag y política revalidable
	rr := do(t, h, http.MethodGet, url, bearer)
	if rr.Code != http.StatusOK {
		t.Fatalf("thumb -> %d, quiero 200", rr.Code)
	}
	etag := rr.Header().Get("ETag")
	if etag == "" {
		t.Fatal("thumb sin ETag")
	}
	cc := rr.Header().Get("Cache-Control")
	if !strings.Contains(cc, "must-revalidate") || strings.Contains(cc, "immutable") {
		t.Fatalf("Cache-Control = %q, quiero revalidable sin immutable", cc)
	}
	// el ETag deriva de la cache key: cambia si cambia la generación
	if want := thumb.ETag("/dav/x/a.jpg", etagFor(assetDe(t, st, id)), 400); etag != want {
		t.Fatalf("ETag = %s, quiero %s (derivado de la cache key)", etag, want)
	}

	// revalidación: If-None-Match coincidente -> 304 sin cuerpo
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", bearer)
	req.Header.Set("If-None-Match", etag)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("revalidación -> %d, quiero 304", rr.Code)
	}
	if rr.Body.Len() != 0 {
		t.Fatalf("304 con cuerpo de %d bytes", rr.Body.Len())
	}
	if rr.Header().Get("Cache-Control") != cc {
		t.Fatalf("304 sin Cache-Control: %q", rr.Header().Get("Cache-Control"))
	}

	// If-None-Match distinto -> 200 de nuevo
	req = httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", bearer)
	req.Header.Set("If-None-Match", `"otro"`)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("If-None-Match distinto -> %d, quiero 200", rr.Code)
	}
}

// TestVideoPoster304: el póster de vídeo también responde 304 por ETag, y lo
// hace ANTES de generar (sin ffmpeg en el entorno de test, sin el chequeo el
// handler caería al 302 del original).
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
		t.Fatalf("póster revalidado -> %d, quiero 304 (sin pasar por ffmpeg)", rr.Code)
	}
}
