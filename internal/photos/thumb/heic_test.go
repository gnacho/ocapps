package thumb

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	"github.com/gen2brain/h265/heic"
)

// TestImageDecodeHEIC comprueba que el decodificador HEIC queda registrado en
// image.Decode (via init de gen2brain/h265/heic). Codifica un HEIC en memoria y
// lo vuelve a decodificar, sin fixtures binarios.
func TestImageDecodeHEIC(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 320, 200))
	for y := 0; y < 200; y++ {
		for x := 0; x < 320; x++ {
			src.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := heic.Encode(&buf, src); err != nil {
		t.Fatalf("encode HEIC: %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("encode HEIC: salida vacía")
	}

	img, format, err := image.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("image.Decode(HEIC): %v", err)
	}
	if format != "heic" {
		t.Fatalf("formato = %q, quiero heic", format)
	}
	if b := img.Bounds(); b.Dx() != 320 || b.Dy() != 200 {
		t.Fatalf("dimensiones = %dx%d, quiero 320x200", b.Dx(), b.Dy())
	}
}
