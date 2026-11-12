// Package thumb — generación y caché en disco de miniaturas.
// Decodifica JPEG/PNG/GIF/WebP y HEIC/HEIF (decodificador HEVC en Go puro,
// sin CGo ni libvips: gen2brain/h265 registra el formato en image.Decode).
// Formatos sin decodificador (RAW) devuelven ErrUnsupported: la API sirve
// entonces el original o un placeholder.
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

	"github.com/gen2brain/h265/heic" // registra image.Decode para HEIC/HEIF
	"github.com/gnacho/ocapps/internal/common/webdav"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

var ErrUnsupported = errors.New("formato sin decodificador nativo")

// Service genera y cachea miniaturas. Es multi-owner (H8): NO guarda el
// cliente DAV — llega por parámetro con la credencial del owner de cada
// request. La caché en disco ES compartida entre usuarios sin riesgo de
// colisión: la cache key es sha1(href|etag|maxSize) y el href contiene el
// space-UUID del espacio personal del usuario
// (/dav/spaces/<uuid>$<owner>/...), así que dos usuarios NUNCA generan la
// misma clave para ficheros distintos (y para el mismo fichero físico del
// mismo espacio la miniatura es idéntica).
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
	// thumbCacheVersion invalida la caché cuando cambia la generación de
	// miniaturas (la v2 corrige la orientación): las v1 antiguas quedan como
	// ficheros basura sin alcanzar — no hace falta purgarlas.
	h := sha1.Sum([]byte(fmt.Sprintf("%s|%s|%s|%d", thumbCacheVersion, href, etag, maxSize)))
	return hex.EncodeToString(h[:]) + ".jpg"
}

// thumbCacheVersion es la sal de la cache key; súbela al cambiar cómo se
// genera la miniatura.
const thumbCacheVersion = "v2"

// VideoPoster extrae un fotograma del vídeo con ffmpeg y lo cachea como JPEG.
// Descarga el vídeo a un temporal (ffmpeg necesita poder buscar en el fichero).
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

// isHEIC huele el box ftyp del offset 4 (todo ISOBMFF válido lo lleva ahí;
// las brands heic/mif1 las maneja el decodificador de gen2brain/h265).
func isHEIC(data []byte) bool {
	return len(data) >= 8 && string(data[4:8]) == "ftyp"
}

// decode decodifica los bytes ya descargados. HEIC/HEIF va por heic.Decode
// con AutoRotate (aplica las transformaciones irot/imir/clap del contenedor:
// así guarda la rotación un iPhone); el resto por el registro de image.Decode.
func decode(data []byte) (image.Image, error) {
	if isHEIC(data) {
		return heic.Decode(bytes.NewReader(data), heic.Options{AutoRotate: true})
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, err
}

// Get devuelve la ruta del thumbnail en caché, generándolo si hace falta.
// maxSize es el lado largo. Se sirve como JPEG progresivo calidad 80.
// dav es el cliente de la sesión del owner del asset (H8).
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

	// límite defensivo: 60 MB por foto. Va a memoria: para el HEIC hay que
	// volver a leer los bytes (el decodificador busca por el contenedor) y
	// para el JPEG hacen falta para extraer la orientación EXIF.
	data, err := io.ReadAll(io.LimitReader(rc, 60<<20))
	if err != nil {
		return "", err
	}

	img, err := decode(data)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	// el HEIC ya sale orientado (AutoRotate aplica irot/imir/clap); el JPEG
	// hay que transformarlo según su orientación EXIF
	img = applyOrientation(img, jpegOrientation(data))

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
