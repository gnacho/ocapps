// Package phash - perceptual hash (dHash) to detect nearly identical photos.
// Pure Go: uses image.Decode (with HEIC registered by the blank import).
package phash

import (
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "github.com/gen2brain/h265/heic"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// DHash computes a 64-bit difference hash (hex) of an image.
func DHash(img image.Image) string {
	g := image.NewGray(image.Rect(0, 0, 9, 8))
	draw.CatmullRom.Scale(g, g.Bounds(), img, img.Bounds(), draw.Over, nil)
	var bits uint64
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			if g.GrayAt(x, y).Y > g.GrayAt(x+1, y).Y {
				bits |= 1 << (y*8 + x)
			}
		}
	}
	return fmt.Sprintf("%016x", bits)
}

// Hamming: distance between two 64-bit hex hashes.
func Hamming(a, b string) int {
	var x, y uint64
	if _, err := fmt.Sscanf(a, "%x", &x); err != nil {
		return 64
	}
	if _, err := fmt.Sscanf(b, "%x", &y); err != nil {
		return 64
	}
	d := 0
	for v := x ^ y; v != 0; v &= v - 1 {
		d++
	}
	return d
}

// Group groups hashes with distance <= maxDist. Returns groups of >= 2 ids.
func Group(hashes map[int64]string, maxDist int) [][]int64 {
	ids := make([]int64, 0, len(hashes))
	for id := range hashes {
		ids = append(ids, id)
	}
	seen := map[int64]bool{}
	var groups [][]int64
	for i := 0; i < len(ids); i++ {
		if seen[ids[i]] {
			continue
		}
		group := []int64{ids[i]}
		for j := i + 1; j < len(ids); j++ {
			if seen[ids[j]] {
				continue
			}
			if Hamming(hashes[ids[i]], hashes[ids[j]]) <= maxDist {
				group = append(group, ids[j])
				seen[ids[j]] = true
			}
		}
		if len(group) >= 2 {
			seen[ids[i]] = true
			groups = append(groups, group)
		}
	}
	return groups
}
