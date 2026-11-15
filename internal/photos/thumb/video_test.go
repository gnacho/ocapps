package thumb

import (
	"context"
	"image"
	_ "image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gnacho/ocapps/internal/common/webdav"
)

// TestVideoPoster generates a test video with ffmpeg, serves it over HTTP and
// checks that VideoPoster extracts a scaled frame as JPEG.
func TestVideoPoster(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	vid := filepath.Join(dir, "v.mp4")
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10",
		"-t", "2", "-c:v", "libx264", "-pix_fmt", "yuv420p", vid)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate video: %v (%s)", err, out)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, vid)
	}))
	defer srv.Close()

	dav := webdav.New(srv.URL, "u", "t")
	svc, err := New(filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := svc.VideoPoster(context.Background(), dav, "/v.mp4", "e1", 200)
	if err != nil {
		t.Fatalf("VideoPoster: %v", err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Fatalf("poster is not an image: %v", err)
	}
	if b := img.Bounds(); b.Dx() > 200 {
		t.Fatalf("not scaled: %dx%d", b.Dx(), b.Dy())
	}
	// second call: cached
	if out2, err := svc.VideoPoster(context.Background(), dav, "/v.mp4", "e1", 200); err != nil || out2 != out {
		t.Fatalf("poster cache: %v %v", out2, err)
	}
}
