package thumb

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gnacho/ocapps/internal/common/webdav"
)

// pngFixture returns a solid PNG of the given size.
func pngFixture(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	return buf.Bytes()
}

// imageServer serves body on every path and counts the requests.
func imageServer(t *testing.T, status int, ctype string, body []byte) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", ctype)
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// jpegSize decodes the JPEG at path and returns its dimensions.
func jpegSize(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open thumbnail: %v", err)
	}
	defer func() { _ = f.Close() }()
	img, format, err := image.Decode(f)
	if err != nil {
		t.Fatalf("decode thumbnail: %v", err)
	}
	if format != "jpeg" {
		t.Fatalf("thumbnail format = %q, want jpeg", format)
	}
	return img.Bounds().Dx(), img.Bounds().Dy()
}

func TestNewCreatesCacheDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "thumbs")
	svc, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if svc.cacheDir != dir {
		t.Fatalf("cache dir = %q, want %q", svc.cacheDir, dir)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("cache dir not created: %v", err)
	}
}

func TestNewFailsWhenPathIsFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("prepare file: %v", err)
	}
	if _, err := New(filepath.Join(file, "thumbs")); err == nil {
		t.Fatal("New must fail when the cache directory cannot be created")
	}
}

func TestCacheKeyIsStableAndSpecific(t *testing.T) {
	base := cacheKey("/dav/spaces/a/f.jpg", "etag-1", 400)
	if base != cacheKey("/dav/spaces/a/f.jpg", "etag-1", 400) {
		t.Fatal("cacheKey must be deterministic")
	}
	if !strings.HasSuffix(base, ".jpg") {
		t.Fatalf("cacheKey = %q, want a .jpg suffix", base)
	}
	cases := []string{
		cacheKey("/dav/spaces/a/f.jpg", "etag-2", 400), // another etag
		cacheKey("/dav/spaces/b/f.jpg", "etag-1", 400), // another space
		cacheKey("/dav/spaces/a/f.jpg", "etag-1", 800), // another size
	}
	for _, other := range cases {
		if other == base {
			t.Fatalf("cacheKey must differ per href, etag and size, got %q twice", base)
		}
	}
}

func TestGetScalesAndCaches(t *testing.T) {
	photo := pngFixture(t, 200, 100)
	srv, hits := imageServer(t, http.StatusOK, "image/png", photo)
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dav := webdav.New(srv.URL, "user", "app-token")

	got, err := svc.Get(context.Background(), dav, "/dav/spaces/s/f.png", "etag-1", 50)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if w, h := jpegSize(t, got); w != 50 || h != 25 {
		t.Fatalf("thumbnail = %dx%d, want 50x25", w, h)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("downloads = %d, want 1", n)
	}

	// Second call: served from the cache, no new download.
	again, err := svc.Get(context.Background(), dav, "/dav/spaces/s/f.png", "etag-1", 50)
	if err != nil {
		t.Fatalf("Get (cached): %v", err)
	}
	if again != got {
		t.Fatalf("cached path = %q, want %q", again, got)
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Fatalf("downloads after the cache hit = %d, want 1", n)
	}

	// Different etag invalidates the entry.
	if _, err := svc.Get(context.Background(), dav, "/dav/spaces/s/f.png", "etag-2", 50); err != nil {
		t.Fatalf("Get (new etag): %v", err)
	}
	if n := atomic.LoadInt32(hits); n != 2 {
		t.Fatalf("downloads after the new etag = %d, want 2", n)
	}
}

func TestGetKeepsSmallImagesUntouched(t *testing.T) {
	srv, _ := imageServer(t, http.StatusOK, "image/png", pngFixture(t, 20, 10))
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := svc.Get(context.Background(), webdav.New(srv.URL, "u", "t"), "/dav/spaces/s/small.png", "e", 400)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if w, h := jpegSize(t, got); w != 20 || h != 10 {
		t.Fatalf("thumbnail = %dx%d, want 20x10", w, h)
	}
}

func TestGetRejectsFormatsWithoutDecoder(t *testing.T) {
	srv, _ := imageServer(t, http.StatusOK, "image/x-canon-cr2", []byte("not an image at all"))
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = svc.Get(context.Background(), webdav.New(srv.URL, "u", "t"), "/dav/spaces/s/raw.cr2", "e", 400)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

func TestGetPropagatesDownloadErrors(t *testing.T) {
	srv, _ := imageServer(t, http.StatusNotFound, "text/plain", []byte("gone"))
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = svc.Get(context.Background(), webdav.New(srv.URL, "u", "t"), "/dav/spaces/s/f.png", "e", 400)
	if err == nil {
		t.Fatal("Get must fail when the original cannot be downloaded")
	}
	if errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, a transport error is not ErrUnsupported", err)
	}
}

func TestVideoPosterServesTheCachedPoster(t *testing.T) {
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	key := "v" + cacheKey("/dav/spaces/s/v.mp4", "etag-1", 400)
	out := filepath.Join(svc.cacheDir, key[:2], key)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatalf("prepare cache: %v", err)
	}
	if err := os.WriteFile(out, []byte("jpeg"), 0o600); err != nil {
		t.Fatalf("prepare cached poster: %v", err)
	}

	// A nil DAV client is safe here: a cache hit must not touch the network.
	got, err := svc.VideoPoster(context.Background(), nil, "/dav/spaces/s/v.mp4", "etag-1", 400)
	if err != nil {
		t.Fatalf("VideoPoster: %v", err)
	}
	if got != out {
		t.Fatalf("poster path = %q, want %q", got, out)
	}
}

func TestVideoPosterPropagatesDownloadErrors(t *testing.T) {
	srv, _ := imageServer(t, http.StatusNotFound, "text/plain", nil)
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = svc.VideoPoster(context.Background(), webdav.New(srv.URL, "u", "t"), "/dav/spaces/s/v.mp4", "e", 400)
	if err == nil {
		t.Fatal("VideoPoster must fail when the video cannot be downloaded")
	}
	if !strings.Contains(err.Error(), "download") {
		t.Fatalf("err = %v, want the download error", err)
	}
}

func TestETagMatchesTheCacheKey(t *testing.T) {
	got := ETag("/dav/spaces/s/f.jpg", "etag-1", 400)
	if want := `"` + cacheKey("/dav/spaces/s/f.jpg", "etag-1", 400) + `"`; got != want {
		t.Fatalf("ETag = %s, want %s", got, want)
	}
	if !strings.HasPrefix(got, `"`) || !strings.HasSuffix(got, `"`) {
		t.Fatalf("ETag = %s, want a quoted validator", got)
	}
	if ETag("/dav/spaces/s/f.jpg", "etag-2", 400) == got {
		t.Fatal("ETag must change with the etag of the original")
	}
}

func TestVideoETagIsDistinctFromTheImageETag(t *testing.T) {
	const href, etag = "/dav/spaces/s/v.mp4", "etag-1"
	got := VideoETag(href, etag, 400)
	if want := `"v` + cacheKey(href, etag, 400) + `"`; got != want {
		t.Fatalf("VideoETag = %s, want %s", got, want)
	}
	// The "v" prefix is what keeps a poster and an image of the same href
	// from sharing a validator.
	if got == ETag(href, etag, 400) {
		t.Fatal("VideoETag must differ from ETag")
	}
}

func TestVideoPosterFailsWhenNoFrameCanBeExtracted(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	// Not a video: ffmpeg fails on both attempts.
	srv, _ := imageServer(t, http.StatusOK, "application/octet-stream", []byte("this is not a video"))
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := svc.VideoPoster(context.Background(), webdav.New(srv.URL, "u", "t"), "/dav/spaces/s/v.mp4", "e", 400); err == nil {
		t.Fatal("VideoPoster must fail when no frame can be extracted")
	}
}
