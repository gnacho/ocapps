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

// --- helpers (sin fixtures binarios) ---

// labelGrid construye una imagen w×h donde cada píxel lleva un color único
// etiquetado con una letra (A, B, C...) y devuelve el mapeo color→letra.
func labelGrid(w, h int) (image.Image, map[color.RGBA]byte) {
	m := map[color.RGBA]byte{}
	// colores A..Z: canal R secuencial, G/B fijos, alpha opaco
	var palette []color.RGBA
	for i := 0; i < w*h; i++ {
		palette = append(palette, color.RGBA{R: uint8(i*7 + 1), G: 200, B: 100, A: 255})
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := palette[y*w+x]
			img.Set(x, y, c)
			m[c] = byte('A' + y*w + x)
		}
	}
	return img, m
}

// gridToStrings vuelca la imagen como filas de letras (según el mapeo).
func gridToStrings(t *testing.T, img image.Image, m map[color.RGBA]byte) []string {
	t.Helper()
	b := img.Bounds()
	rows := []string{}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := ""
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bb, a := img.At(x, y).RGBA()
			c := color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(bb >> 8), A: uint8(a >> 8)}
			label, ok := m[c]
			if !ok {
				t.Fatalf("píxel (%d,%d) con color %v sin etiqueta", x, y, c)
			}
			row += string(label)
		}
		rows = append(rows, row)
	}
	return rows
}

// TestApplyOrientation comprueba las 8 orientaciones EXIF sobre una imagen
// asimétrica 3×2 (los píxeles almacenados son A B C / D E F).
func TestApplyOrientation(t *testing.T) {
	src, m := labelGrid(3, 2)
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
		out := applyOrientation(src, o)
		rows := gridToStrings(t, out, m)
		if len(rows) != len(want) {
			t.Errorf("orientación %d: %v, quiero %v", o, rows, want)
			continue
		}
		for i, row := range rows {
			if row != want[i] {
				t.Errorf("orientación %d: fila %d = %q, quiero %q", o, i, row, want[i])
			}
		}
	}
}

// TestApplyOrientationIdentity: la orientación 1 devuelve la imagen tal cual
// (sin copia) y los valores fuera de rango cuentan como 1.
func TestApplyOrientationIdentity(t *testing.T) {
	src, _ := labelGrid(3, 2)
	if got := applyOrientation(src, 1); got != image.Image(src) {
		t.Error("orientación 1: no debe copiar la imagen")
	}
	for _, o := range []int{0, -1, 9} {
		if got := applyOrientation(src, o); got != image.Image(src) {
			t.Errorf("orientación %d: debe tratarla como 1", o)
		}
	}
}

// jpegOrientationOf devuelve los bytes de un JPEG w×h (codificado por
// image/jpeg) con un APP1 EXIF insertado tras el SOI; la longitud del
// segmento va en big-endian, como manda el JPEG.
func jpegOrientationOf(t *testing.T, o, w, h int) []byte {
	t.Helper()
	src, _ := labelGrid(w, h)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	base := buf.Bytes()

	// TIFF EXIF mínimo (little-endian) con la única etiqueta Orientation:
	// cabecera (8) + nº de entradas (2) + entrada 0x0112 (12, valor SHORT en
	// línea en el offset 18) + puntero "siguiente IFD" (4)
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

	// el APP1 se mete entre el SOI y el resto del stream
	out := append([]byte{}, base[:2]...)
	out = append(out, seg...)
	out = append(out, body...)
	return append(out, base[2:]...)
}

// TestJpegOrientation lee la orientación del APP1 sintético.
func TestJpegOrientation(t *testing.T) {
	for _, o := range []int{1, 2, 6, 8} {
		if got := jpegOrientation(jpegOrientationOf(t, o, 3, 2)); got != o {
			t.Errorf("orientación = %d, quiero %d", got, o)
		}
	}
	if got := jpegOrientation([]byte("no es un jpeg")); got != 1 {
		t.Errorf("no-JPEG: orientación = %d, quiero 1", got)
	}
}

// bytesServer sirve unos bytes fijos por HTTP (webdav.Download hace un GET
// sencillo; no hace falta Range).
func bytesServer(t *testing.T, data []byte) *webdav.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return webdav.New(srv.URL, "u", "t")
}

// TestGetRotatedJPEG: un JPEG guardado con orientación 6 (lateral) produce
// una miniatura con los lados intercambiados — el thumbnail ya no sale tumbado.
func TestGetRotatedJPEG(t *testing.T) {
	// almacenado 40×30; con orientación 6 la vista es 30×40 y el lado largo
	// queda en 32 → miniatura 24×32 (sin orientar sería 32×24)
	data := jpegOrientationOf(t, 6, 40, 30)
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := svc.Get(context.Background(), bytesServer(t, data), "/a.jpg", "e1", 32)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	w, h := jpegDims(t, out)
	if w != 24 || h != 32 {
		t.Errorf("miniatura %dx%d, quiero 24x32", w, h)
	}
	// segunda llamada: cacheada (misma ruta)
	if out2, err := svc.Get(context.Background(), bytesServer(t, data), "/a.jpg", "e1", 32); err != nil || out2 != out {
		t.Errorf("caché: %v %v", out2, err)
	}
}

// TestGetUprightJPEG: orientación 1 mantiene las proporciones de siempre.
func TestGetUprightJPEG(t *testing.T) {
	data := jpegOrientationOf(t, 1, 40, 30)
	svc, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	out, err := svc.Get(context.Background(), bytesServer(t, data), "/a.jpg", "e1", 32)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if w, h := jpegDims(t, out); w != 32 || h != 24 {
		t.Errorf("miniatura %dx%d, quiero 32x24", w, h)
	}
}

// TestGetHEIC: el camino HEIC por Get (sniff ftyp → heic.Decode con
// AutoRotate) sigue funcionando y escalando.
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
	dav := bytesServer(t, buf.Bytes())
	out, err := svc.Get(context.Background(), dav, "/a.heic", "e1", 256)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if w, h := jpegDims(t, out); w != 256 || h != 160 {
		t.Errorf("miniatura %dx%d, quiero 256x160", w, h)
	}
}

func jpegDims(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("DecodeConfig: %v", err)
	}
	return cfg.Width, cfg.Height
}
