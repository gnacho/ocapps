// Package exif — worker que extrae EXIF con Range requests (primeros 256 KB,
// suficiente para la cabecera EXIF de la mayoría de JPEG/TIFF).
package exif

import (
	"bytes"
	"context"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"strconv"

	_ "github.com/gen2brain/h265/heic"
	"github.com/gnacho/ocapps/internal/common/webdav"
	"github.com/gnacho/ocapps/internal/photos/store"
	"github.com/rwcarlsen/goexif/exif"
	_ "golang.org/x/image/webp"
)

const rangeBytes = 256 * 1024

type Worker struct {
	dav   *webdav.Client
	store *store.Store
	log   *slog.Logger
}

func NewWorker(c *webdav.Client, st *store.Store, log *slog.Logger) *Worker {
	return &Worker{dav: c, store: st, log: log}
}

// Run procesa la cola de EXIF pendiente hasta que el contexto se cancele
// o no queden pendientes. Pensado para lanzarse tras cada scan.
func (w *Worker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		batch, err := w.store.PendingExif(ctx, 50)
		if err != nil {
			w.log.Error("pending exif", "err", err)
			return
		}
		if len(batch) == 0 {
			return
		}
		for _, a := range batch {
			if ctx.Err() != nil {
				return
			}
			if err := w.processOne(ctx, a.ID, a.Path); err != nil {
				w.log.Warn("exif falló", "path", a.Path, "err", err)
				// marca como procesado para no reintentar en bucle
				_ = w.store.SaveExif(ctx, a.ID, store.ExifResult{})
			}
		}
	}
}

func (w *Worker) processOne(ctx context.Context, id int64, href string) error {
	data, err := w.dav.GetRange(ctx, href, 0, rangeBytes)
	if err != nil {
		return err
	}
	res := store.ExifResult{}

	x, err := exif.Decode(bytes.NewReader(data))
	if err != nil {
		// sin EXIF (PNG, WebP...): no es error; al menos sacamos las dimensiones
		fillDims(&res, data)
		return w.store.SaveExif(ctx, id, res)
	}

	if tm, err := x.DateTime(); err == nil {
		res.TakenAt = &tm
	}
	if v, err := x.Get(exif.Make); err == nil {
		make_, _ := v.StringVal()
		model := ""
		if m, err := x.Get(exif.Model); err == nil {
			model, _ = m.StringVal()
		}
		res.Camera = trimJoin(make_, model)
	}
	// LensModel (0xA434) no está mapeado en esta versión de goexif; se omite.
	if v, err := x.Get(exif.ISOSpeedRatings); err == nil {
		if n, err := v.Int(0); err == nil {
			res.ISO = n
		}
	}
	if v, err := x.Get(exif.FNumber); err == nil {
		if num, den, err := v.Rat2(0); err == nil && den > 0 {
			res.Aperture = "f/" + trimFloat(float64(num)/float64(den))
		}
	}
	if v, err := x.Get(exif.ExposureTime); err == nil {
		if num, den, err := v.Rat2(0); err == nil && num > 0 {
			res.Shutter = "1/" + itoa(den/num)
		}
	}
	if v, err := x.Get(exif.FocalLength); err == nil {
		if num, den, err := v.Rat2(0); err == nil && den > 0 {
			res.Focal = trimFloat(float64(num)/float64(den)) + " mm"
		}
	}
	if v, err := x.Get(exif.PixelXDimension); err == nil {
		if n, err := v.Int(0); err == nil {
			res.Width = n
		}
	}
	if v, err := x.Get(exif.PixelYDimension); err == nil {
		if n, err := v.Int(0); err == nil {
			res.Height = n
		}
	}
	if lat, lon, err := x.LatLong(); err == nil {
		res.Lat, res.Lon = &lat, &lon
	}
	fillDims(&res, data)

	return w.store.SaveExif(ctx, id, res)
}

// fillDims: si el EXIF no trae dimensiones, las saca de la cabecera de la imagen
// (necesario para el layout justificado del timeline).
func fillDims(res *store.ExifResult, data []byte) {
	if res.Width != 0 && res.Height != 0 {
		return
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		res.Width, res.Height = cfg.Width, cfg.Height
	}
}

func trimJoin(a, b string) string {
	s := a
	if b != "" && b != a {
		if s != "" {
			s += " "
		}
		s += b
	}
	return s
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func itoa(i int64) string {
	return strconv.FormatInt(i, 10)
}
