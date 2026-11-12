package thumb

// Orientación EXIF aplicada a las miniaturas: image/jpeg nunca aplica la
// orientación del APP1 por su cuenta, así que un JPEG guardado girado (la
// forma habitual de un iPhone) sale de lado sin esta transformación. El HEIC
// no pasa por aquí: heic.Decode con AutoRotate ya aplica irot/imir/clap, y
// duplicar la orientación del item Exif lo giraría dos veces.

import (
	"bytes"
	"image"

	"github.com/rwcarlsen/goexif/exif"
)

// jpegOrientation lee la orientación EXIF de un JPEG (el APP1 va al principio
// del fichero y no pasa de 64 KB). Sin EXIF legible o con orientación 1
// devuelve 1: sin transformación.
func jpegOrientation(data []byte) int {
	x, err := exif.Decode(bytes.NewReader(data))
	if err != nil {
		return 1
	}
	v, err := x.Get(exif.Orientation)
	if err != nil {
		return 1
	}
	n, err := v.Int(0)
	if err != nil || n < 2 || n > 8 {
		return 1
	}
	return n
}

// applyOrientation aplica la transformación de la orientación EXIF 2–8 sobre
// img. La 1 no toca nada; las demás producen una copia RGBA reordenada píxel
// a píxel (el escalado previo a la codificación la trabaja después, así que
// la conversión a RGBA no pierde nada relevante). El píxel almacenado en
// (x, y) va al destino (dx, dy) según el semáforo del estándar: 2 espejo
// horizontal, 3 giro 180, 4 espejo vertical, 5 transposición por la diagonal
// principal, 6 giro 90° horario, 7 transposición por la inversa y 8 giro
// 90° antihorario.
func applyOrientation(img image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	// 5–8 son transposiciones: el resultado queda con los lados cambiados
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
