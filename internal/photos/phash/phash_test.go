package phash

import (
	"image"
	"image/color"
	"testing"
)

func img(fill color.Gray) image.Image {
	m := image.NewGray(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			m.SetGray(x, y, fill)
		}
	}
	return m
}

func TestDHashAndHamming(t *testing.T) {
	a := DHash(img(color.Gray{Y: 10}))
	b := DHash(img(color.Gray{Y: 10}))
	if a != b {
		t.Fatalf("imágenes iguales -> hashes iguales: %s vs %s", a, b)
	}
	if d := Hamming(a, b); d != 0 {
		t.Fatalf("distancia misma imagen = %d", d)
	}
	if len(a) != 16 {
		t.Fatalf("hash no es 64 bits hex: %q", a)
	}
}

func TestGroup(t *testing.T) {
	hashes := map[int64]string{
		1: "0000000000000000",
		2: "0000000000000001", // ~1 de 1
		3: "ffffffffffffffff", // muy distinto
	}
	groups := Group(hashes, 8)
	if len(groups) != 1 || len(groups[0]) != 2 {
		t.Fatalf("grupos: %+v", groups)
	}
}
