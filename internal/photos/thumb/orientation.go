package thumb

// EXIF orientation applied to the generated thumbnails. image/jpeg does NOT
// apply the orientation of the APP1 segment on its own, so a JPEG stored
// rotated (the usual way a phone writes a portrait photo: stored landscape
// plus orientation 6) produced a thumbnail lying on its side while the browser
// showed the original upright. The HEIC path does not go through here:
// heic.Decode with AutoRotate already applies irot/imir/clap, and applying the
// Exif orientation on top of it would rotate the photo twice.

import (
	"bytes"
	"image"

	"github.com/rwcarlsen/goexif/exif"
)

// OrientationAutoRotate is the EXIF value meaning "no transformation".
const OrientationAutoRotate = 1

// Orientation reads the EXIF orientation of a JPEG (the APP1 segment lives at
// the beginning of the file). It returns 1, meaning "leave it as it is", when
// there is no readable EXIF, no orientation tag or a value out of range.
func Orientation(data []byte) int {
	x, err := exif.Decode(bytes.NewReader(data))
	if err != nil {
		return OrientationAutoRotate
	}
	v, err := x.Get(exif.Orientation)
	if err != nil {
		return OrientationAutoRotate
	}
	n, err := v.Int(0)
	if err != nil || n < 1 || n > 8 {
		return OrientationAutoRotate
	}
	return n
}

// ApplyOrientation returns img transformed according to the EXIF orientation
// (1 no change, 2 mirror horizontally, 3 rotate 180, 4 mirror vertically,
// 5 transpose, 6 rotate 90 clockwise, 7 transverse, 8 rotate 90
// counter-clockwise). 5 to 8 swap the sides. Values out of range leave the
// image untouched.
func ApplyOrientation(img image.Image, o int) image.Image {
	if o < 2 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
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

// SwapsSides reports whether the orientation rotates the photo 90 degrees, so
// the visible width and height are the stored ones the other way round. The
// EXIF worker uses it to store the size the user sees: the timeline lays the
// photos out with it.
func SwapsSides(o int) bool {
	return o >= 5 && o <= 8
}
