// Package thumb — generación y caché en disco de miniaturas.
// Decodifica JPEG/PNG/GIF/WebP y HEIC/HEIF (decodificador HEVC en Go puro,
// sin CGo ni libvips: gen2brain/h265 registra el formato en image.Decode).
// Formatos sin decodificador (RAW) devuelven ErrUnsupported: la API sirve
// entonces el original o un placeholder.
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

	_ "github.com/gen2brain/h265/heic" // registra image.Decode para HEIC/HEIF
	"github.com/gnacho/ocapps/internal/common/webdav"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

var ErrUnsupported = errors.New("formato sin decodificador nativo")

type Service struct {
	dav      *webdav.Client
	cacheDir string
}

func New(c *webdav.Client, cacheDir string) (*Service, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	return &Service{dav: c, cacheDir: cacheDir}, nil
}

func cacheKey(href, etag string, maxSize int) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|%s|%d", href, etag, maxSize)))
	return hex.EncodeToString(h[:]) + ".jpg"
}

// VideoPoster extrae un fotograma del vídeo con ffmpeg y lo cachea como JPEG.
// Descarga el vídeo a un temporal (ffmpeg necesita poder buscar en el fichero).
func (s *Service) VideoPoster(ctx context.Context, href, etag string, maxSize int) (string, error) {
	key := "v" + cacheKey(href, etag, maxSize)
	out := filepath.Join(s.cacheDir, key[:2], key)
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}

	rc, _, err := s.dav.Download(ctx, href)
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

	tmpOut := out + ".tmp.jpg" // ffmpeg infiere el formato por la extensión
	// primer intento en el segundo 1; si el vídeo es más corto, desde el principio
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
	return "", errors.New("ffmpeg no pudo extraer un fotograma")
}

// Get devuelve la ruta del thumbnail en caché, generándolo si hace falta.
// maxSize es el lado largo. Se sirve como JPEG progresivo calidad 80.
func (s *Service) Get(ctx context.Context, href, etag string, maxSize int) (string, error) {
	key := cacheKey(href, etag, maxSize)
	out := filepath.Join(s.cacheDir, key[:2], key)
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}

	rc, _, err := s.dav.Download(ctx, href)
	if err != nil {
		return "", err
	}
	defer rc.Close()

	// límite defensivo: 60 MB por foto
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
