package exif

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/gen2brain/h265/heic"
	"github.com/gnacho/ocapps/internal/common/webdav"
	"github.com/gnacho/ocapps/internal/photos/store"
	"github.com/rwcarlsen/goexif/exif"
)

// --- in-memory EXIF TIFF construction (no binary fixtures) ---

// tifTag is an IFD entry, with ifd >= 0 the value is a pointer to the i-th
// sub-IFD (the builder resolves the absolute offset).
type tifTag struct {
	tag  uint16
	typ  uint16
	data []byte
	ifd  int
}

var tifElemSize = map[uint16]int{1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 7: 1, 9: 4, 10: 8}

// buildTiff serialises IFDs into a little-endian EXIF TIFF (II*, 42). ifds[0]
// is IFD0, the entries end up sorted by tag and the values that do not fit in
// the 4 bytes of the IFD go to an area after the table, aligned to 2.
// No image data: only the directory structure that goexif reads.
func buildTiff(ifds ...[]tifTag) []byte {
	offs := make([]int, len(ifds))
	pos := 8 // after the II*42 header plus the IFD0 offset
	sizes := make([]int, len(ifds))
	for i, tags := range ifds {
		vals := 0
		for _, t := range tags {
			if t.ifd < 0 && len(t.data) > 4 {
				vals += len(t.data)
				if vals%2 != 0 {
					vals++
				}
			}
		}
		sizes[i] = 2 + len(tags)*12 + 4 + vals
		offs[i] = pos
		pos += sizes[i]
	}

	out := &bytes.Buffer{}
	out.WriteString("II")
	must(binary.Write(out, binary.LittleEndian, uint16(42)))
	must(binary.Write(out, binary.LittleEndian, uint32(offs[0])))

	for i, tags := range ifds {
		sorted := append([]tifTag(nil), tags...)
		sort.Slice(sorted, func(a, b int) bool { return sorted[a].tag < sorted[b].tag })
		must(binary.Write(out, binary.LittleEndian, uint16(len(sorted))))
		// value area of this IFD, right after its table
		valAt := offs[i] + 2 + len(sorted)*12 + 4
		vals := &bytes.Buffer{}
		for _, t := range sorted {
			typ, data := t.typ, t.data
			if t.ifd >= 0 {
				// pointer to a sub-IFD: LONG with count 1
				typ, data = 4, make([]byte, 4)
				binary.LittleEndian.PutUint32(data, uint32(offs[t.ifd]))
			}
			must(binary.Write(out, binary.LittleEndian, t.tag))
			must(binary.Write(out, binary.LittleEndian, typ))
			must(binary.Write(out, binary.LittleEndian, uint32(len(data)/tifElemSize[typ])))
			if len(data) <= 4 {
				// inline: the bytes go at the start of the field
				var f [4]byte
				copy(f[:], data)
				out.Write(f[:])
				continue
			}
			must(binary.Write(out, binary.LittleEndian, uint32(valAt+vals.Len())))
			vals.Write(data)
			if vals.Len()%2 != 0 {
				vals.WriteByte(0)
			}
		}
		must(binary.Write(out, binary.LittleEndian, uint32(0))) // no next IFD
		out.Write(vals.Bytes())
	}
	return out.Bytes()
}

func ascii(s string) []byte { return append([]byte(s), 0) }

// ascii20 serialises an EXIF DateTime ("YYYY:MM:DD HH:MM:SS\0", 20 bytes).
func ascii20(s string) []byte {
	b := make([]byte, 20)
	copy(b, s)
	return b
}

func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, v)
	return b
}

func rat(num, den uint32) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[0:], num)
	binary.LittleEndian.PutUint32(b[4:], den)
	return b
}

func rat3(a, b, c uint32) []byte {
	return append(rat(a, 1), append(rat(b, 1), rat(c, 1)...)...)
}

// tiffFixture holds the fields the worker fills in the synthetic EXIF.
type tiffFixture struct {
	Make, Model            string
	Orientation            int
	DateTime               string
	ISO, PixelX, PixelY    int
	FNumNum, FNumDen       int
	ExpoNum, ExpoDen       int
	FocalNum, FocalDen     int
	LatDeg, LatMin, LatSec int
	LonDeg, LonMin, LonSec int
	LatRef, LonRef         string
}

func buildExifTiff(t tiffFixture) []byte {
	ifd0 := []tifTag{
		{0x010F, 2, ascii(t.Make), -1},
		{0x0110, 2, ascii(t.Model), -1},
		{0x0112, 3, u16(uint16(t.Orientation)), -1},
		{0x0132, 2, ascii20(t.DateTime), -1},
		{0x8769, 4, nil, 1}, // ExifIFDPointer
		{0x8825, 4, nil, 2}, // GPSIFDPointer
	}
	exifIfd := []tifTag{
		{0x829A, 5, rat(uint32(t.ExpoNum), uint32(t.ExpoDen)), -1},
		{0x829D, 5, rat(uint32(t.FNumNum), uint32(t.FNumDen)), -1},
		{0x8827, 3, u16(uint16(t.ISO)), -1},
		{0x920A, 5, rat(uint32(t.FocalNum), uint32(t.FocalDen)), -1},
		{0xA002, 3, u16(uint16(t.PixelX)), -1},
		{0xA003, 3, u16(uint16(t.PixelY)), -1},
	}
	gpsIfd := []tifTag{
		{0x0001, 2, ascii(t.LatRef), -1},
		{0x0002, 5, rat3(uint32(t.LatDeg), uint32(t.LatMin), uint32(t.LatSec)), -1},
		{0x0003, 2, ascii(t.LonRef), -1},
		{0x0004, 5, rat3(uint32(t.LonDeg), uint32(t.LonMin), uint32(t.LonSec)), -1},
	}
	return buildTiff(ifd0, exifIfd, gpsIfd)
}

var exifFix = tiffFixture{
	Make: "Go", Model: "TestCam", Orientation: 6,
	DateTime: "2024:05:06 07:08:09",
	ISO:      200, PixelX: 6, PixelY: 4,
	FNumNum: 14, FNumDen: 10, // f/1.4
	ExpoNum: 1, ExpoDen: 125, // 1/125 s
	FocalNum: 24, FocalDen: 1,
	LatDeg: 37, LatMin: 46, LatSec: 30, LatRef: "N", // 37.775
	LonDeg: 122, LonMin: 25, LonSec: 12, LonRef: "W", // -122.42
}

// app1Exif wraps the TIFF in a JPEG APP1 segment ("Exif\0\0" plus payload,
// the segment length goes in big-endian, as JPEG mandates).
func app1Exif(tiff []byte) []byte {
	body := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:4], uint16(len(body)+2))
	return append(seg, body...)
}

// resOfAsset rebuilds the ExifResult from the store row.
func resOfAsset(a store.Asset) store.ExifResult {
	res := store.ExifResult{
		Camera: a.Camera, ISO: a.ISO,
		Aperture: a.Aperture, Shutter: a.Shutter, Focal: a.Focal,
		Width: a.Width, Height: a.Height, Lat: a.Lat, Lon: a.Lon,
	}
	if a.TakenAt != 0 {
		tm := time.Unix(a.TakenAt, 0)
		res.TakenAt = &tm
	}
	return res
}

func assertFields(t *testing.T, res store.ExifResult) {
	t.Helper()
	if res.Camera != "Go TestCam" {
		t.Errorf("Camera = %q, want %q", res.Camera, "Go TestCam")
	}
	if res.ISO != 200 {
		t.Errorf("ISO = %d, want 200", res.ISO)
	}
	if res.Aperture != "f/1.4" {
		t.Errorf("Aperture = %q, want f/1.4", res.Aperture)
	}
	if res.Shutter != "1/125" {
		t.Errorf("Shutter = %q, want 1/125", res.Shutter)
	}
	if res.Focal != "24 mm" {
		t.Errorf("Focal = %q, want 24 mm", res.Focal)
	}
	if res.Lat == nil || res.Lon == nil {
		t.Fatalf("GPS missing: lat=%v lon=%v", res.Lat, res.Lon)
	}
	if math.Abs(*res.Lat-37.775) > 1e-9 || math.Abs(*res.Lon+122.42) > 1e-9 {
		t.Errorf("GPS = %v,%v, want 37.775,-122.42", *res.Lat, *res.Lon)
	}
	if res.TakenAt == nil || res.TakenAt.Format("2006-01-02 15:04:05") != "2024-05-06 07:08:09" {
		t.Errorf("TakenAt = %v, want 2024-05-06 07:08:09", res.TakenAt)
	}
}

// --- tests ---

// TestParseExifFields checks the field extraction (shared by the JPEG and HEIC
// paths) on a synthetic EXIF TIFF.
func TestParseExifFields(t *testing.T) {
	x, err := exif.Decode(bytes.NewReader(buildExifTiff(exifFix)))
	if err != nil {
		t.Fatalf("exif.Decode(raw TIFF): %v", err)
	}
	res := store.ExifResult{}
	parseExif(x, &res)
	assertFields(t, res)
}

// test HEIC with the EXIF embedded as a container item.
func buildHEIC(t *testing.T) []byte {
	t.Helper()
	src := image.NewRGBA(image.Rect(0, 0, 320, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 320; x++ {
			src.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := heic.Encode(&buf, src, heic.EncodeOptions{Exif: buildExifTiff(exifFix)}); err != nil {
		t.Fatalf("encode HEIC: %v", err)
	}
	return buf.Bytes()
}

// davServer serves the bytes over HTTP with Range support (limited: the
// content choice is made by the callback with the requested range), count
// accumulates the requests, to verify the escalation.
func davServer(t *testing.T, serve func(end int64) []byte) (*webdav.Client, *int) {
	t.Helper()
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		end := int64(0)
		if rng := r.Header.Get("Range"); rng != "" {
			var a, b int64
			if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &a, &b); err == nil {
				end = b
			}
		}
		data := serve(end)
		w.Header().Set("Content-Type", "image/heic")
		w.Header().Set("Content-Range", "bytes 0-"+itoa(int64(len(data))-1)+"/"+itoa(int64(len(data))))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return webdav.New(srv.URL, "u", "t"), &count
}

// storeWithAsset opens a test store with an asset without EXIF pointing to href.
func storeWithAsset(t *testing.T, href string, size int64) (*store.Store, int64) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := st.UpsertByETag(context.Background(), "u", href, "e1", href, "image", time.Now(), size)
	if err != nil {
		t.Fatal(err)
	}
	return st, id
}

// TestProcessOneHEIC runs the full worker path against a HEIC with EXIF: the
// Exif item of the container ends up in the result (GPS, camera, ISO).
func TestProcessOneHEIC(t *testing.T) {
	data := buildHEIC(t)
	dav, _ := davServer(t, func(end int64) []byte { return data })

	st, id := storeWithAsset(t, "/fotos/heic", int64(len(data)))
	w := NewWorker(st, slog.New(slog.DiscardHandler))
	if err := w.processOne(context.Background(), dav, id, "/fotos/heic"); err != nil {
		t.Fatalf("processOne: %v", err)
	}
	a, err := st.AssetByID(context.Background(), "u", id)
	if err != nil {
		t.Fatal(err)
	}
	assertFields(t, resOfAsset(a))
}

// TestProcessOneJPEG does the same through the JPEG path: the APP1 with EXIF
// has not changed its behaviour.
func TestProcessOneJPEG(t *testing.T) {
	// minimal JPEG with the APP1 EXIF embedded after the SOI
	data := append([]byte{0xFF, 0xD8}, app1Exif(buildExifTiff(exifFix))...)
	data = append(data, 0xFF, 0xD9)
	dav, _ := davServer(t, func(end int64) []byte { return data })

	st, id := storeWithAsset(t, "/fotos/jpg", int64(len(data)))
	w := NewWorker(st, slog.New(slog.DiscardHandler))
	if err := w.processOne(context.Background(), dav, id, "/fotos/jpg"); err != nil {
		t.Fatalf("processOne: %v", err)
	}
	a, err := st.AssetByID(context.Background(), "u", id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Camera != "Go TestCam" || a.ISO != 200 {
		t.Errorf("JPEG EXIF: camera=%q iso=%d, want Go TestCam/200", a.Camera, a.ISO)
	}
}

// TestHEICExifEscalation: a truncated prefix (the Exif item of the container
// beyond the first 256 KB) triggers the range escalation and the EXIF ends up
// in the result.
func TestHEICExifEscalation(t *testing.T) {
	data := buildHEIC(t)
	// the first two ranges (256 KB and 1 MB) return a truncated ftyp, the
	// parsing fails but NOT with ErrNoExif, the 4 MB one returns the full file
	dav, count := davServer(t, func(end int64) []byte {
		if end < 1<<20 {
			return data[:8]
		}
		return data
	})

	st, id := storeWithAsset(t, "/fotos/heic", int64(len(data)))
	w := NewWorker(st, slog.New(slog.DiscardHandler))
	if err := w.processOne(context.Background(), dav, id, "/fotos/heic"); err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if *count < 3 {
		t.Errorf("no full escalation: %d requests", *count)
	}
	a, err := st.AssetByID(context.Background(), "u", id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Lat == nil || a.Lon == nil {
		t.Error("GPS missing after the escalation")
	}
}

// TestProcessOneNoExif: a PNG still ends up with exif_done set and dimensions.
func TestProcessOneNoExif(t *testing.T) {
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 32, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), A: 255})
		}
	}
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	dav, _ := davServer(t, func(end int64) []byte { return data })

	st, id := storeWithAsset(t, "/fotos/png", int64(len(data)))
	w := NewWorker(st, slog.New(slog.DiscardHandler))
	if err := w.processOne(context.Background(), dav, id, "/fotos/png"); err != nil {
		t.Fatalf("processOne: %v", err)
	}
	a, err := st.AssetByID(context.Background(), "u", id)
	if err != nil {
		t.Fatal(err)
	}
	if !a.ExifDone || a.Width != 32 || a.Height != 16 {
		t.Errorf("PNG: done=%v dims=%dx%d, want true 32x16", a.ExifDone, a.Width, a.Height)
	}
	if a.Camera != "" || a.Lat != nil {
		t.Errorf("PNG: must carry no EXIF: camera=%q lat=%v", a.Camera, a.Lat)
	}
}

// must: helper for fixture bytes, a write failure in memory is a bug of the
// test, not of the code under test.
func must(err error) {
	if err != nil {
		panic(err)
	}
}

// tiffWith builds a little-endian TIFF/EXIF block with the given tags, so the
// tests do not need binary fixtures.
func tiffWith(tags []struct {
	id    uint16
	typ   uint16
	count uint32
	value uint32
}) []byte {
	hdr := []byte{'I', 'I', 42, 0, 8, 0, 0, 0}
	n := len(tags)
	body := make([]byte, 2+n*12+4)
	binary.LittleEndian.PutUint16(body[0:], uint16(n))
	for i, tg := range tags {
		e := body[2+i*12:]
		binary.LittleEndian.PutUint16(e[0:], tg.id)
		binary.LittleEndian.PutUint16(e[2:], tg.typ)
		binary.LittleEndian.PutUint32(e[4:], tg.count)
		if tg.typ == 3 { // SHORT lives in the first two bytes of the value
			binary.LittleEndian.PutUint16(e[8:], uint16(tg.value))
		} else {
			binary.LittleEndian.PutUint32(e[8:], tg.value)
		}
	}
	return append(hdr, body...)
}

// TestParseExifSwapsDimsOfRotatedPhotos: a photo stored landscape with
// orientation 6 is displayed portrait, so the index must carry the size the
// user sees (the timeline lays the grid out with it). Regression guard for the
// thumbnails that used to come out sideways.
func TestParseExifSwapsDimsOfRotatedPhotos(t *testing.T) {
	cases := []struct {
		orientation int
		wantW       int
		wantH       int
	}{
		{1, 4000, 2252},
		{6, 2252, 4000},
		{8, 2252, 4000},
		{3, 4000, 2252},
	}
	for _, c := range cases {
		tiff := tiffWith([]struct {
			id    uint16
			typ   uint16
			count uint32
			value uint32
		}{
			{0x0112, 3, 1, uint32(c.orientation)}, // Orientation (SHORT)
			{0xA002, 4, 1, 4000},                  // PixelXDimension (LONG)
			{0xA003, 4, 1, 2252},                  // PixelYDimension (LONG)
		})
		var buf bytes.Buffer
		if err := jpegWithExif(tiff, &buf); err != nil {
			t.Fatal(err)
		}
		x, err := exif.Decode(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatalf("orientation %d: exif.Decode: %v", c.orientation, err)
		}
		res := store.ExifResult{}
		parseExif(x, &res)
		if res.Width != c.wantW || res.Height != c.wantH {
			t.Errorf("orientation %d: %dx%d, want %dx%d", c.orientation, res.Width, res.Height, c.wantW, c.wantH)
		}
	}
}

// jpegWithExif wraps a TIFF/EXIF block in the APP1 segment of a tiny JPEG.
func jpegWithExif(tiff []byte, out *bytes.Buffer) error {
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		}
	}
	var base bytes.Buffer
	if err := jpeg.Encode(&base, img, nil); err != nil {
		return err
	}
	b := base.Bytes()
	body := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:4], uint16(len(body)+2))
	out.Write(b[:2])
	out.Write(seg)
	out.Write(body)
	out.Write(b[2:])
	return nil
}

// TestFailedReadKeepsTheMetadata: a photo whose header cannot be read (a 404, a
// transient error) is marked as processed so it is not retried in a loop, but
// the camera and the dimensions of the row survive (issue #75: the backfill
// re-reads every asset, and a glitch must not erase what is already indexed).
func TestFailedReadKeepsTheMetadata(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	now := time.Now()
	id, _, err := st.UpsertByETag(ctx, "alice", "/dav/spaces/s/Fotos/a.jpg", "e1", "a.jpg", "image", now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveExif(ctx, id, store.ExifResult{TakenAt: &now, Camera: "Canon", Width: 6000, Height: 4000}); err != nil {
		t.Fatal(err)
	}
	// pending again, as the backfill leaves it
	if _, err := st.ResetExif(ctx, "alice"); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	w := NewWorker(st, slog.Default())
	if err := w.Run(ctx, "alice", webdav.New(srv.URL, "u", "t")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	a, err := st.AssetByID(ctx, "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Camera != "Canon" || a.Width != 6000 || a.Height != 4000 {
		t.Errorf("a failed read erased the metadata: %+v", a)
	}
	if !a.ExifDone {
		t.Error("the asset was not marked as processed: it would be retried in a loop")
	}
}
