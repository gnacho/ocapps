//go:build tools

// Package tools pins in go.mod the dependencies of the modules ported
// in H2-H4 (SPEC §2.3) while their code does not exist in the repo yet. Without
// these blank imports, `go mod tidy` would drop the requires. Each import is
// removed once the real package that uses it lands in internal/.
package tools

import (
	_ "github.com/gen2brain/h265/heic"   // photos (HEIC) - H3
	_ "github.com/rwcarlsen/goexif/exif" // photos (EXIF, internal replace) - H3
	_ "golang.org/x/image/draw"          // photos (thumbs/phash) - H3
)
