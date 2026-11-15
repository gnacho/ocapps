// Package thumb - thumbnail generation and on-disk cache.
// Decodes JPEG/PNG/GIF/WebP and HEIC/HEIF (pure Go HEVC decoder,
// no CGo and no libvips: gen2brain/h265 registers the format in image.Decode).
// Formats without a decoder (RAW) return ErrUnsupported: the API then serves
// the original or a placeholder.
package thumb

import (
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

	_ "github.com/gen2brain/h265/heic" // registers image.Decode for HEIC/HEIF
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

func cacheKey(href, etag string, maxSize int) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|%s|%d", href, etag, maxSize)))
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
	defer rc.Close()

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", err
	}
	tmpIn := out + ".in"
	f, err := os.Create(tmpIn)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, io.LimitReader(rc, 2<<30)); err != nil {
		f.Close()
		os.Remove(tmpIn)
		return "", err
	}
	f.Close()
	defer os.Remove(tmpIn)

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
		os.Remove(tmpOut)
	}
	return "", errors.New("ffmpeg could not extract a frame")
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
	defer rc.Close()

	// defensive limit: 60 MB per photo
	img, _, err := image.Decode(io.LimitReader(rc, 60<<20))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnsupported, err)
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
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	return out, os.Rename(tmp, out)
}
