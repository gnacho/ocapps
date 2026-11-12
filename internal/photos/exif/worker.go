// Package exif — worker que extrae EXIF con Range requests (primeros 256 KB,
// suficiente para la cabecera EXIF de la mayoría de JPEG/TIFF). Los
// HEIC/HEIF llevan el item Exif en cualquier punto del contenedor: si no está
// en el prefijo se reintenta con rangos escalonados (1 MB, 4 MB) y, si sigue
// sin aparecer, el asset se registra como sin EXIF (comportamiento de siempre).
package exif

import (
	"bytes"
	"context"
	"errors"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"strconv"

	"github.com/gen2brain/h265/heic"
	"github.com/gnacho/ocapps/internal/common/webdav"
	"github.com/gnacho/ocapps/internal/photos/store"
	"github.com/rwcarlsen/goexif/exif"
	_ "golang.org/x/image/webp"
)

const rangeBytes = 256 * 1024

// heicRanges son los prefijos que probamos para extraer el item Exif de un
// HEIC: el primero es el estándar y los dos siguientes el reescalado si el
// item cae más allá de 256 KB.
var heicRanges = []int64{rangeBytes, 1 << 20, 4 << 20}

type Worker struct {
	store *store.Store
	log   *slog.Logger
}

// NewWorker: el worker es stateless respecto al usuario (H8); el cliente DAV
// del owner llega por parámetro a Run.
func NewWorker(st *store.Store, log *slog.Logger) *Worker {
	return &Worker{store: st, log: log}
}

// Run procesa la cola de EXIF pendiente DEL OWNER hasta que el contexto se
// cancele o no queden pendientes. Pensado para lanzarse tras cada scan con el
// cliente DAV de la sesión del owner. Devuelve nil al terminar (o por ctx);
// si la credencial está caducada (webdav.ErrUnauthorized) ABORTA el lote y lo
// propaga: el scheduler pospone el trabajo a la próxima actividad del
// usuario en vez de quemar toda la cola de EXIF contra un 401.
func (w *Worker) Run(ctx context.Context, owner string, dav *webdav.Client) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		batch, err := w.store.PendingExif(ctx, owner, 50)
		if err != nil {
			w.log.Error("pending exif", "owner", owner, "err", err)
			return nil
		}
		if len(batch) == 0 {
			return nil
		}
		for _, a := range batch {
			if ctx.Err() != nil {
				return nil
			}
			if err := w.processOne(ctx, dav, a.ID, a.Path); err != nil {
				if errors.Is(err, webdav.ErrUnauthorized) {
					return err
				}
				w.log.Warn("exif falló", "owner", owner, "path", a.Path, "err", err)
				// marca como procesado para no reintentar en bucle
				_ = w.store.SaveExif(ctx, a.ID, store.ExifResult{})
			}
		}
	}
}

func (w *Worker) processOne(ctx context.Context, dav *webdav.Client, id int64, href string) error {
	data, err := dav.GetRange(ctx, href, 0, rangeBytes)
	if err != nil {
		return err
	}
	res := store.ExifResult{}

	x, data, err := w.decodeExif(ctx, dav, href, data)
	if err != nil {
		if errors.Is(err, webdav.ErrUnauthorized) {
			return err
		}
		// sin EXIF (PNG, WebP, HEIC sin item Exif o truncado tras el
		// reescalado...): no es error; al menos sacamos las dimensiones
		fillDims(&res, data)
		return w.store.SaveExif(ctx, id, res)
	}

	parseExif(x, &res)
	fillDims(&res, data)
	// orientación 5–8 (variantes de transposición): las dimensiones de
	// visualización quedan transpuestas respecto a las almacenadas
	if o := exifOrientation(x); o >= 5 && o <= 8 {
		res.Width, res.Height = res.Height, res.Width
	}

	return w.store.SaveExif(ctx, id, res)
}

// decodeExif devuelve el EXIF parseado y el prefijo con el que se hizo (para
// fillDims; en HEIC puede ser mayor que el descargado al principio). Devuelve
// error cuando no hay EXIF legible.
func (w *Worker) decodeExif(ctx context.Context, dav *webdav.Client, href string, data []byte) (*exif.Exif, []byte, error) {
	if !isHEIC(data) {
		// JPEG: el APP1 con el EXIF no pasa de 64 KB, el prefijo de 256 KB
		// sobra; goexif también parsea TIFF crudo
		x, err := exif.Decode(bytes.NewReader(data))
		return x, data, err
	}
	tiff, prefix, err := w.heicExifPayload(ctx, dav, href)
	if err != nil {
		return nil, data, err
	}
	data = prefix
	// goexif parsea el stream TIFF crudo sin problema, así que la extracción
	// de campos es idéntica a la del JPEG
	x, err := exif.Decode(bytes.NewReader(tiff))
	if err != nil {
		return nil, data, err
	}
	return x, data, nil
}

// heicExifPayload extrae el payload TIFF del item Exif del contenedor HEIC.
// heic.RawExif aliasa el buffer de entrada sin copiarlo: el payload es
// solo-lectura (nada debe mutar el prefijo ni el resultado). Con ErrNoExif el
// fichero no lleva EXIF y se devuelve tal cual; con cualquier otro error
// (normalmente prefijo truncado) se reintenta con los rangos escalonados de
// heicRanges y, si el último también falla, se propaga el error.
func (w *Worker) heicExifPayload(ctx context.Context, dav *webdav.Client, href string) ([]byte, []byte, error) {
	var last error
	for _, n := range heicRanges {
		data, err := dav.GetRange(ctx, href, 0, n)
		if err != nil {
			return nil, nil, err
		}
		tiff, err := heic.RawExif(bytes.NewReader(data))
		switch {
		case err == nil:
			return tiff, data, nil
		case errors.Is(err, heic.ErrNoExif):
			return nil, data, err
		default:
			last = err
		}
	}
	w.log.Warn("no se pudo extraer el EXIF del HEIC", "path", href, "err", last)
	return nil, nil, last
}

// parseExif rellena res con los campos cámara/exposición/geo del EXIF (vale
// igual para JPEG y para el stream TIFF crudo de un HEIC).
func parseExif(x *exif.Exif, res *store.ExifResult) {
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
}

// isHEIC huele el box ftyp del offset 4 (todo ISOBMFF válido lo lleva ahí;
// las brands heic/mif1 las maneja el decodificador de gen2brain/h265).
func isHEIC(data []byte) bool {
	return len(data) >= 8 && string(data[4:8]) == "ftyp"
}

// exifOrientation devuelve la orientación EXIF (1–8); cualquier valor fuera
// de rango o ausente cuenta como 1 (sin transformación).
func exifOrientation(x *exif.Exif) int {
	if v, err := x.Get(exif.Orientation); err == nil {
		if n, err := v.Int(0); err == nil && n >= 1 && n <= 8 {
			return n
		}
	}
	return 1
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
