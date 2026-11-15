package thumb

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	"github.com/gen2brain/h265/heic"
)

// TestImageDecodeHEIC checks that the HEIC decoder is registered in
// image.Decode (via the init of gen2brain/h265/heic). It encodes a HEIC in
// memory and decodes it again, with no binary fixtures.
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
		t.Fatal("encode HEIC: empty output")
	}

	img, format, err := image.Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("image.Decode(HEIC): %v", err)
	}
	if format != "heic" {
		t.Fatalf("format = %q, want heic", format)
	}
	if b := img.Bounds(); b.Dx() != 320 || b.Dy() != 200 {
		t.Fatalf("dimensions = %dx%d, want 320x200", b.Dx(), b.Dy())
	}
}
