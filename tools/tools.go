//go:build tools

// Package tools fija en go.mod las dependencias de los módulos que se portan
// en H2-H4 (SPEC §2.3) mientras su código aún no existe en el repo. Sin estos
// blank imports, `go mod tidy` eliminaría los requires. Cada import se borra
// cuando el paquete real que lo usa aterriza en internal/.
package tools

import (
	_ "github.com/PuerkitoBio/goquery"      // news (scraper) — H4
	_ "github.com/andybalholm/cascadia"     // news — H4
	_ "github.com/gen2brain/h265/heic"      // photos (HEIC) — H3
	_ "github.com/go-shiori/go-readability" // news (full-content) — H4
	_ "github.com/microcosm-cc/bluemonday"  // news (sanitize) — H4
	_ "github.com/mmcdole/gofeed"           // news — H4
	_ "github.com/rwcarlsen/goexif/exif"    // photos (EXIF, replace interno) — H3
	_ "golang.org/x/crypto/bcrypt"          // news (auth local) — H4
	_ "golang.org/x/image/draw"             // photos (thumbs/phash) — H3
)
