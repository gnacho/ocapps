// Package exif - worker that extracts EXIF with Range requests (first 256 KB,
// enough for the EXIF header of most JPEG/TIFF files). HEIC/HEIF carry the Exif
// item anywhere in the container: if it is not in the prefix it retries with
// escalating ranges (1 MB, 4 MB) and, if it still does not show up, the asset
// is recorded as having no EXIF (the usual behaviour).
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
	"github.com/gnacho/ocapps/internal/photos/thumb"
	"github.com/rwcarlsen/goexif/exif"
	_ "golang.org/x/image/webp"
)

const rangeBytes = 256 * 1024

// heicRanges are the prefixes tried to extract the Exif item of a HEIC: the
// first one is the standard one and the next two the escalation used when the
// item falls beyond 256 KB.
var heicRanges = []int64{rangeBytes, 1 << 20, 4 << 20}

type Worker struct {
	store *store.Store
	log   *slog.Logger
}

// NewWorker: the worker is stateless with respect to the user (H8), the DAV
// client of the owner arrives as a parameter to Run.
func NewWorker(st *store.Store, log *slog.Logger) *Worker {
	return &Worker{store: st, log: log}
}

// Run processes the pending EXIF queue OF THE OWNER until the context is
// cancelled or nothing is pending. Meant to be launched after each scan with
// the DAV client of the owner's session. It returns nil when done (or on ctx),
// if the credential has expired (webdav.ErrUnauthorized) it ABORTS the batch
// and propagates it: the scheduler postpones the work until the next user
// activity instead of burning the whole EXIF queue against a 401.
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
				w.log.Warn("exif failed", "owner", owner, "path", a.Path, "err", err)
				// mark it as processed so it is not retried in a loop, but keep
				// what the row already had: a failed read must not erase the
				// camera, the dimensions or the location of an indexed photo
				_ = w.store.MarkExifDone(ctx, a.ID)
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
		// no EXIF (PNG, WebP, HEIC without an Exif item or truncated after
		// the escalation...): not an error, at least we take the dimensions
		fillDims(&res, data)
		return w.store.SaveExif(ctx, id, res)
	}

	parseExif(x, &res)
	fillDims(&res, data)
	return w.store.SaveExif(ctx, id, res)
}

// decodeExif returns the parsed EXIF and the prefix it was parsed from (for
// fillDims, on HEIC it may be larger than the one downloaded at the start). It
// returns an error when there is no readable EXIF.
func (w *Worker) decodeExif(ctx context.Context, dav *webdav.Client, href string, data []byte) (*exif.Exif, []byte, error) {
	if !isHEIC(data) {
		// JPEG: the APP1 with the EXIF does not go beyond 64 KB, the 256 KB
		// prefix is more than enough, goexif also parses raw TIFF
		x, err := exif.Decode(bytes.NewReader(data))
		return x, data, err
	}
	tiff, prefix, err := w.heicExifPayload(ctx, dav, href)
	if err != nil {
		return nil, data, err
	}
	data = prefix
	// goexif parses the raw TIFF stream with no problem, so the field
	// extraction is identical to the JPEG one
	x, err := exif.Decode(bytes.NewReader(tiff))
	if err != nil {
		return nil, data, err
	}
	return x, data, nil
}

// heicExifPayload extracts the TIFF payload of the Exif item of the HEIC
// container. heic.RawExif aliases the input buffer without copying it: the
// payload is read-only (nothing must mutate the prefix or the result). With
// ErrNoExif the file carries no EXIF and it is returned as is, with any other
// error (usually a truncated prefix) it retries with the escalating ranges of
// heicRanges and, if the last one also fails, the error is propagated.
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
	w.log.Warn("could not extract EXIF from HEIC", "path", href, "err", last)
	return nil, nil, last
}

// parseExif fills res with the camera/exposure/geo fields of the EXIF (valid
// both for JPEG and for the raw TIFF stream of a HEIC).
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
	// LensModel (0xA434) is not mapped in this version of goexif, it is skipped.
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
	// The dimensions of the EXIF are the stored ones. A photo with orientation
	// 6 (a phone portrait) is displayed 90 degrees turned, so the size the user
	// sees is the other way round: the timeline lays the grid out with it.
	if v, err := x.Get(exif.Orientation); err == nil {
		if n, err := v.Int(0); err == nil && thumb.SwapsSides(n) {
			res.Width, res.Height = res.Height, res.Width
		}
	}
}

// isHEIC sniffs the ftyp box at offset 4 (every valid ISOBMFF carries it there,
// the heic/mif1 brands are handled by the gen2brain/h265 decoder).
func isHEIC(data []byte) bool {
	return len(data) >= 8 && string(data[4:8]) == "ftyp"
}

// fillDims: if the EXIF brings no dimensions, they are taken from the image
// header (needed for the justified layout of the timeline).
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
