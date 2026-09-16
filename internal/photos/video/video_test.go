package video

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gnacho/ocapps/internal/common/webdav"
)

func TestHLSTranscode(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg no instalado")
	}
	dir := t.TempDir()
	vid := filepath.Join(dir, "v.mp4")
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10",
		"-t", "2", "-c:v", "libx264", "-pix_fmt", "yuv420p", vid)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generar vídeo: %v (%s)", err, out)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, vid)
	}))
	defer srv.Close()

	tr, err := New(webdav.New(srv.URL, "u", "t"), filepath.Join(dir, "hls"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := tr.HLS(context.Background(), "/v.mp4", "e1")
	if err != nil {
		t.Fatalf("HLS: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(out, "index.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "#EXTM3U") || !strings.Contains(string(b), ".ts") {
		t.Fatalf("playlist inesperada:\n%s", b)
	}
	segs, _ := filepath.Glob(filepath.Join(out, "seg*.ts"))
	if len(segs) == 0 {
		t.Fatal("sin segmentos .ts")
	}
	// cacheado: segunda llamada devuelve el mismo dir
	if out2, err := tr.HLS(context.Background(), "/v.mp4", "e1"); err != nil || out2 != out {
		t.Fatalf("caché HLS: %v %v", out2, err)
	}
}
