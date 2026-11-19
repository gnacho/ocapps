// Package thumb - thumbnail generation and on-disk cache.
// Decodes JPEG/PNG/GIF/WebP and HEIC/HEIF (pure Go HEVC decoder,
// no CGo and no libvips: gen2brain/h265 registers the format in image.Decode).
// Formats without a decoder (RAW) return ErrUnsupported: the API then serves
// the original or a placeholder.
package thumb

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gen2brain/h265/heic"
	"github.com/gnacho/ocapps/internal/common/webdav"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

var ErrUnsupported = errors.New("format without a native decoder")

// Service generates and caches thumbnails. It is multi-owner (H8): it does
// NOT keep the DAV client - it arrives as a parameter with the credential of
// the owner of each request. The on-disk cache IS shared between users with no
// risk of collision: the cache key is sha1(href|etag|maxSize) and the href
// contains the space UUID of the user's personal space
// (/dav/spaces/<uuid>$<owner>/...), so two users NEVER generate the same key
// for different files (and for the same physical file of the same space the
// thumbnail is identical).
type Service struct {
	cacheDir string
}

func New(cacheDir string) (*Service, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	return &Service{cacheDir: cacheDir}, nil
}

// cacheVersion salts the cache key: bump it whenever the generation of a
// thumbnail changes, so the entries produced by the previous code are not
// served (v2 applies the EXIF orientation, the v1 thumbnails are sideways).
const cacheVersion = "v2"

func cacheKey(href, etag string, maxSize int) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|%s|%s|%d", cacheVersion, href, etag, maxSize)))
	return hex.EncodeToString(h[:]) + ".jpg"
}

// VideoPoster extracts a frame from the video with ffmpeg and caches it as JPEG.
// Downloads the video to a temporary file (ffmpeg needs to seek inside the file).
func (s *Service) VideoPoster(ctx context.Context, dav *webdav.Client, href, etag string, maxSize int) (string, error) {
	key := "v" + cacheKey(href, etag, maxSize)
	out := filepath.Join(s.cacheDir, key[:2], key)
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}

	rc, _, err := dav.Download(ctx, href)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	tmpIn := out + ".in"
	f, err := os.Create(tmpIn)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, io.LimitReader(rc, 2<<30)); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpIn)
		return "", err
	}
	// the input file has to be complete before ffmpeg reads it: a Close that
	// fails means the copy did not reach the disk
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpIn)
		return "", err
	}
	defer func() { _ = os.Remove(tmpIn) }()

	tmpOut := out + ".tmp.jpg" // ffmpeg infers the format from the extension
	// first attempt at second 1, if the video is shorter, from the beginning
	for _, ss := range []string{"1", "0"} {
		cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-ss", ss, "-i", tmpIn,
			"-frames:v", "1", "-vf", fmt.Sprintf("scale=%d:-1", maxSize), tmpOut)
		if err := cmd.Run(); err == nil {
			if st, err := os.Stat(tmpOut); err == nil && st.Size() > 0 {
				return out, os.Rename(tmpOut, out)
			}
		}
		_ = os.Remove(tmpOut)
	}
	return "", errors.New("ffmpeg could not extract a frame")
}

// isHEIC sniffs the ftyp box at offset 4: every valid ISOBMFF file carries it
// there, and the heic/mif1 brands are handled by the gen2brain/h265 decoder.
func isHEIC(data []byte) bool {
	return len(data) >= 8 && string(data[4:8]) == "ftyp"
}

// decode turns the downloaded bytes into an image. HEIC/HEIF goes through
// heic.Decode with AutoRotate (the container carries the rotation of a phone
// photo in irot/imir/clap); the rest goes through the image.Decode registry.
func decode(data []byte) (image.Image, error) {
	if isHEIC(data) {
		return heic.Decode(bytes.NewReader(data), heic.Options{AutoRotate: true})
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, err
}

// Get returns the path of the cached thumbnail, generating it when needed.
// maxSize is the long side. It is served as a quality 80 progressive JPEG.
// dav is the client of the session of the asset owner (H8).
func (s *Service) Get(ctx context.Context, dav *webdav.Client, href, etag string, maxSize int) (string, error) {
	key := cacheKey(href, etag, maxSize)
	out := filepath.Join(s.cacheDir, key[:2], key)
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}

	rc, _, err := dav.Download(ctx, href)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()

	// defensive limit: 60 MB per photo. The bytes are kept because the HEIC
	// decoder seeks inside the container and the JPEG needs them to read the
	// EXIF orientation.
	data, err := io.ReadAll(io.LimitReader(rc, 60<<20))
	if err != nil {
		return "", err
	}
	img, err := decode(data)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	// HEIC comes out already rotated (AutoRotate applies irot/imir/clap); the
	// rest carries the rotation in the EXIF orientation, which the decoder
	// ignores.
	if !isHEIC(data) {
		img = ApplyOrientation(img, Orientation(data))
	}

	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > maxSize || h > maxSize {
		if w >= h {
			h = h * maxSize / w
			w = maxSize
		} else {
			w = w * maxSize / h
			h = maxSize
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	tmp := out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	err = jpeg.Encode(f, dst, &jpeg.Options{Quality: 80})
	// the thumbnail is only renamed into the cache when it is complete on disk:
	// a Close that fails after a good Encode means a truncated file
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return out, os.Rename(tmp, out)
}
