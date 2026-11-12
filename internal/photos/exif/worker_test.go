package exif

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
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

// --- construcción de TIFF EXIF en memoria (sin fixtures binarios) ---

// tifTag es una entrada de un IFD; con ifd >= 0 el valor es un pointer al
// sub-IFD i-ésimo (el builder resuelve el offset absoluto).
type tifTag struct {
	tag  uint16
	typ  uint16
	data []byte
	ifd  int
}

var tifElemSize = map[uint16]int{1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 7: 1, 9: 4, 10: 8}

// buildTiff serializa IFDs en un TIFF EXIF little-endian (II*, 42). ifds[0]
// es el IFD0; las entradas quedan ordenadas por tag y los valores que no
// caben en los 4 bytes del IFD van a un área tras la tabla, alineada a 2.
// Sin datos de imagen: solo la estructura de directorios que lee goexif.
func buildTiff(ifds ...[]tifTag) []byte {
	offs := make([]int, len(ifds))
	pos := 8 // tras la cabecera II*42 + offset del IFD0
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
		// área de valores de este IFD, justo tras su tabla
		valAt := offs[i] + 2 + len(sorted)*12 + 4
		vals := &bytes.Buffer{}
		for _, t := range sorted {
			typ, data := t.typ, t.data
			if t.ifd >= 0 {
				// pointer a sub-IFD: LONG con count 1
				typ, data = 4, make([]byte, 4)
				binary.LittleEndian.PutUint32(data, uint32(offs[t.ifd]))
			}
			must(binary.Write(out, binary.LittleEndian, t.tag))
			must(binary.Write(out, binary.LittleEndian, typ))
			must(binary.Write(out, binary.LittleEndian, uint32(len(data)/tifElemSize[typ])))
			if len(data) <= 4 {
				// en línea: los bytes van al principio del campo
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
		must(binary.Write(out, binary.LittleEndian, uint32(0))) // sin IFD siguiente
		out.Write(vals.Bytes())
	}
	return out.Bytes()
}

func ascii(s string) []byte { return append([]byte(s), 0) }

// ascii20 serializa un DateTime EXIF ("YYYY:MM:DD HH:MM:SS\0", 20 bytes).
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

// tiffFixture son los campos que rellena el worker en el EXIF sintético.
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

// app1Exif envuelve el TIFF en un segmento JPEG APP1 ("Exif\0\0" + payload;
// la longitud del segmento va en big-endian, como manda el JPEG).
func app1Exif(tiff []byte) []byte {
	body := append([]byte("Exif\x00\x00"), tiff...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:4], uint16(len(body)+2))
	return append(seg, body...)
}

// resOfAsset reconstruye el ExifResult desde la fila del store.
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
		t.Errorf("Camera = %q, quiero %q", res.Camera, "Go TestCam")
	}
	if res.ISO != 200 {
		t.Errorf("ISO = %d, quiero 200", res.ISO)
	}
	if res.Aperture != "f/1.4" {
		t.Errorf("Aperture = %q, quiero f/1.4", res.Aperture)
	}
	if res.Shutter != "1/125" {
		t.Errorf("Shutter = %q, quiero 1/125", res.Shutter)
	}
	if res.Focal != "24 mm" {
		t.Errorf("Focal = %q, quiero 24 mm", res.Focal)
	}
	if res.Lat == nil || res.Lon == nil {
		t.Fatalf("GPS ausente: lat=%v lon=%v", res.Lat, res.Lon)
	}
	if math.Abs(*res.Lat-37.775) > 1e-9 || math.Abs(*res.Lon+122.42) > 1e-9 {
		t.Errorf("GPS = %v,%v, quiero 37.775,-122.42", *res.Lat, *res.Lon)
	}
	if res.TakenAt == nil || res.TakenAt.Format("2006-01-02 15:04:05") != "2024-05-06 07:08:09" {
		t.Errorf("TakenAt = %v, quiero 2024-05-06 07:08:09", res.TakenAt)
	}
}

// --- tests ---

// TestParseExifFields comprueba la extracción de campos (compartida por los
// caminos JPEG y HEIC) sobre un TIFF EXIF sintético.
func TestParseExifFields(t *testing.T) {
	x, err := exif.Decode(bytes.NewReader(buildExifTiff(exifFix)))
	if err != nil {
		t.Fatalf("exif.Decode(TIFF crudo): %v", err)
	}
	res := store.ExifResult{}
	parseExif(x, &res)
	assertFields(t, res)
}

// TestParseExifOrientationSwap comprueba el intercambio de dimensiones para
// las orientaciones 5–8 (transposiciones) y el no-intercambio para 1–4.
func TestParseExifOrientationSwap(t *testing.T) {
	for _, tc := range []struct {
		o        int
		w, h     int
		wantSwap bool
	}{
		{1, 6, 4, false},
		{2, 6, 4, false},
		{3, 6, 4, false},
		{4, 6, 4, false},
		{5, 6, 4, true},
		{6, 6, 4, true},
		{7, 6, 4, true},
		{8, 6, 4, true},
	} {
		fx := exifFix
		fx.Orientation = tc.o
		x, err := exif.Decode(bytes.NewReader(buildExifTiff(fx)))
		if err != nil {
			t.Fatalf("orientación %d: %v", tc.o, err)
		}
		res := store.ExifResult{}
		parseExif(x, &res)
		fillDims(&res, nil) // sin cabecera de imagen: quedan los del EXIF
		if o := exifOrientation(x); o >= 5 && o <= 8 {
			res.Width, res.Height = res.Height, res.Width
		}
		w, h := tc.w, tc.h
		if tc.wantSwap {
			w, h = h, w
		}
		if res.Width != w || res.Height != h {
			t.Errorf("orientación %d: %dx%d, quiero %dx%d", tc.o, res.Width, res.Height, w, h)
		}
	}
}

// HEIC de prueba con el EXIF incrustado como item del contenedor.
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

// davServer sirve los bytes por HTTP con soporte de Range (limitado: la
// elección de contenido la hace el callback con el rango pedido); count
// acumula las peticiones, para verificar el reescalado.
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

// storeWithAsset abre un store de prueba con un asset sin EXIF apuntando a href.
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

// TestProcessOneHEIC lanza el camino completo del worker contra un HEIC con
// EXIF: el item Exif del contenedor acaba en el resultado (GPS, cámara, ISO).
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

// TestProcessOneJPEG repite lo mismo por el camino JPEG: el APP1 con EXIF no
// ha cambiado de comportamiento.
func TestProcessOneJPEG(t *testing.T) {
	// JPEG mínimo con el APP1 EXIF incrustado tras el SOI
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
		t.Errorf("EXIF JPEG: camera=%q iso=%d, quiero Go TestCam/200", a.Camera, a.ISO)
	}
}

// TestHEICExifEscalation: un prefijo truncado (el item Exif del contenedor
// fuera de los primeros 256 KB) dispara el reescalado de rangos y el EXIF
// acaba en el resultado.
func TestHEICExifEscalation(t *testing.T) {
	data := buildHEIC(t)
	// los dos primeros rangos (256 KB y 1 MB) devuelven un ftyp truncado —
	// el parseo falla pero NO con ErrNoExif—; el de 4 MB, el fichero completo
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
		t.Errorf("no hubo reescalado completo: %d peticiones", *count)
	}
	a, err := st.AssetByID(context.Background(), "u", id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Lat == nil || a.Lon == nil {
		t.Error("GPS ausente tras el reescalado")
	}
}

// TestProcessOneNoExif: un PNG sigue acabando en exif_done con dimensiones.
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
		t.Errorf("PNG: done=%v dims=%dx%d, quiero true 32x16", a.ExifDone, a.Width, a.Height)
	}
	if a.Camera != "" || a.Lat != nil {
		t.Errorf("PNG: no debe llevar EXIF: camera=%q lat=%v", a.Camera, a.Lat)
	}
}

// must: helper para fixture bytes; un fallo de escritura en memoria es un bug
// del test, no del código bajo prueba.
func must(err error) {
	if err != nil {
		panic(err)
	}
}
