package thumb

import (
	"bytes"
	"context"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gen2brain/h265/heic"
	"github.com/gnacho/ocapps/internal/common/webdav"
)

// labelledGrid builds a w×h image where every pixel has a unique colour, and
// returns the colour -> letter mapping: the tests read the result as text.
func labelledGrid(w, h int) (image.Image, map[color.RGBA]byte) {
	labels := map[color.RGBA]byte{}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{R: uint8((y*w+x)*7 + 1), G: 200, B: 100, A: 255}
			img.Set(x, y, c)
			labels[c] = byte('A' + y*w + x)
		}
	}
	return img, labels
}

// gridRows dumps the image as rows of letters.
func gridRows(t *testing.T, img image.Image, labels map[color.RGBA]byte) []string {
	t.Helper()
	b := img.Bounds()
	rows := []string{}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := ""
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bb, a := img.At(x, y).RGBA()
			c := color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(bb >> 8), A: uint8(a >> 8)}
			label, ok := labels[c]
			if !ok {
				t.Fatalf("pixel (%d,%d) with an unknown colour %v", x, y, c)
			}
			row += string(label)
		}
		rows = append(rows, row)
	}
	return rows
}

// TestApplyOrientation checks the eight EXIF orientations against an
// asymmetric 3×2 image (stored pixels are A B C / D E F).
func TestApplyOrientation(t *testing.T) {
	src, labels := labelledGrid(3, 2)
	for o, want := range map[int][]string{
		1: {"ABC", "DEF"},
		2: {"CBA", "FED"},
		3: {"FED", "CBA"},
		4: {"DEF", "ABC"},
		5: {"AD", "BE", "CF"},
		6: {"DA", "EB", "FC"},
		7: {"FC", "EB", "DA"},
		8: {"CF", "BE", "AD"},
	} {
		rows := gridRows(t, ApplyOrientation(src, o), labels)
		if len(rows) != len(want) {
			t.Errorf("orientation %d: %v, want %v", o, rows, want)
			continue
		}
		for i := range want {
			if rows[i] != want[i] {
				t.Errorf("orientation %d: row %d = %q, want %q", o, i, rows[i], want[i])
			}
		}
	}
}

// TestApplyOrientationIdentity: 1 (and anything out of range) returns the very
// same image, no copy involved.
func TestApplyOrientationIdentity(t *testing.T) {
	src, _ := labelledGrid(3, 2)
	if got := ApplyOrientation(src, 1); got != image.Image(src) {
		t.Error("orientation 1 must not copy the image")
	}
	for _, o := range []int{0, -1, 9} {
		if got := ApplyOrientation(src, o); got != image.Image(src) {
			t.Errorf("orientation %d must behave like 1", o)
		}
	}
}

// TestSwapsSides: 5 to 8 turn the photo 90 degrees, so the visible size is the
// stored one the other way round.
func TestSwapsSides(t *testing.T) {
	for o, want := range map[int]bool{1: false, 2: false, 3: false, 4: false, 5: true, 6: true, 7: true, 8: true, 0: false, 9: false} {
		if got := SwapsSides(o); got != want {
			t.Errorf("SwapsSides(%d) = %v, want %v", o, got, want)
		}
	}
}

// jpegWithOrientation returns a w×h JPEG with an EXIF APP1 segment carrying the
// orientation, inserted after the SOI. The segment length is big-endian, as the
// JPEG format mandates.
func jpegWithOrientation(t *testing.T, o, w, h int) []byte {
	t.Helper()
	src, _ := labelledGrid(w, h)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	base := buf.Bytes()

	// minimal little-endian TIFF with the single Orientation tag:
	// header (8) + entry count (2) + tag 0x0112 (12, SHORT value inline) +
	// next-IFD pointer (4)
	tiff := []byte{
		'I', 'I', 42, 0, 8, 0, 0, 0,
		1, 0,
		0x12, 0x01, 3, 0, 1, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0,
	}
	binary.LittleEndian.PutUint16(tiff[18:], uint16(o))
	body := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:4], uint16(len(body)+2))

	out := append([]byte{}, base[:2]...)
	out = append(out, seg...)
	out = append(out, body...)
	return append(out, base[2:]...)
}

// TestOrientation reads the orientation from the synthetic APP1.
func TestOrientation(t *testing.T) {
	for _, o := range []int{1, 2, 6, 8} {
		if got := Orientation(jpegWithOrientation(t, o, 3, 2)); got != o {
			t.Errorf("orientation = %d, want %d", got, o)
		}
	}
	if got := Orientation([]byte("not a jpeg")); got != 1 {
		t.Errorf("non-JPEG: orientation = %d, want 1", got)
	}
}

// bytesServer serves fixed bytes over HTTP (webdav.Download is a plain GET).
func bytesServer(t *testing.T, data []byte) *webdav.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return webdav.New(srv.URL, "u", "t")
}

// TestGetRotatedJPEG: a JPEG stored with orientation 6 (a phone portrait) gives
// a thumbnail with the sides swapped, so it is no longer served lying on its
// side while the browser shows the original upright.
func TestGetRotatedJPEG(t *testing.T) {
	// stored 40×30: with orientation 6 the visible photo is 30×40 and the long
	// side becomes 32, so the thumbnail is 24×32 (without orientation: 32×24)
	data := jpegWithOrientation(t, 6, 40, 30)
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := svc.Get(context.Background(), bytesServer(t, data), "/a.jpg", "e1", 32)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if w, h := jpegDims(t, out); w != 24 || h != 32 {
		t.Errorf("thumbnail %dx%d, want 24x32", w, h)
	}
	// second call: cached (same path)
	if out2, err := svc.Get(context.Background(), bytesServer(t, data), "/a.jpg", "e1", 32); err != nil || out2 != out {
		t.Errorf("cache: %v %v", out2, err)
	}
}

// TestGetUprightJPEG: orientation 1 keeps the usual proportions.
func TestGetUprightJPEG(t *testing.T) {
	data := jpegWithOrientation(t, 1, 40, 30)
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := svc.Get(context.Background(), bytesServer(t, data), "/a.jpg", "e1", 32)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if w, h := jpegDims(t, out); w != 32 || h != 24 {
		t.Errorf("thumbnail %dx%d, want 32x24", w, h)
	}
}

// TestGetHEIC: the HEIC path (ftyp sniff → heic.Decode with AutoRotate) keeps
// working and scaling.
func TestGetHEIC(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 320, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 320; x++ {
			src.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := heic.Encode(&buf, src); err != nil {
		t.Fatal(err)
	}
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := svc.Get(context.Background(), bytesServer(t, buf.Bytes()), "/a.heic", "e1", 256)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if w, h := jpegDims(t, out); w != 256 || h != 160 {
		t.Errorf("thumbnail %dx%d, want 256x160", w, h)
	}
}

// TestCacheKeyVersion: the salt changes the key, so the thumbnails generated by
// code that did not apply the orientation are not served any more.
func TestCacheKeyVersion(t *testing.T) {
	key := cacheKey("/a.jpg", "e1", 400)
	if key == cacheKey("/b.jpg", "e1", 400) || key == cacheKey("/a.jpg", "e2", 400) || key == cacheKey("/a.jpg", "e1", 200) {
		t.Error("the key must depend on the href, the etag and the size")
	}
	if cacheVersion != "v2" {
		t.Errorf("cacheVersion = %q: bumping it is what invalidates the old thumbnails", cacheVersion)
	}
	if len(key) != 44 { // sha1 hex + ".jpg"
		t.Errorf("unexpected key shape: %q", key)
	}
}

func jpegDims(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	return cfg.Width, cfg.Height
}
