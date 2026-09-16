// Package index — scanner WebDAV incremental sobre OpenCloud.
package index

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/gnacho/ocapps/internal/common/webdav"
	"github.com/gnacho/ocapps/internal/photos/store"
)

type Scanner struct {
	dav   *webdav.Client
	opts  webdav.Options // clasificación de medios por extensión (SPEC §2.1)
	store *store.Store
	log   *slog.Logger
}

func NewScanner(c *webdav.Client, opts webdav.Options, st *store.Store, log *slog.Logger) *Scanner {
	return &Scanner{dav: c, opts: opts, store: st, log: log}
}

// resolveHref convierte una referencia de carpeta en href absoluto del
// servidor para Propfind: un href absoluto (devuelto por un listado anterior)
// se usa tal cual; una ruta relativa ("Fotos", "" = raíz del espacio) se
// resuelve contra el path del webDavUrl del espacio. Equivale a la resolución
// que hacía el antiguo dav.ListFolder.
func resolveHref(webdavURL, folderRef string) (string, error) {
	if strings.HasPrefix(folderRef, "/") {
		return folderRef, nil
	}
	base := ""
	if u, err := url.Parse(webdavURL); err == nil {
		base = strings.TrimRight(u.Path, "/")
	}
	for _, seg := range strings.Split(strings.Trim(folderRef, "/"), "/") {
		if seg == "" {
			continue
		}
		if seg == ".." {
			return "", fmt.Errorf("path traversal rechazado: %q", folderRef)
		}
		base += "/" + url.PathEscape(seg)
	}
	return base + "/", nil
}

// ScanSpace recorre un espacio desde root (p. ej. "Fotos"; "" = todo el espacio).
// Incremental por etag: upsert solo de lo cambiado; soft-delete de lo desaparecido.
// 70k fotos ≈ unos pocos miles de PROPFINDs: varios minutos el primer scan,
// segundos los incrementales (mismo número de peticiones, pero upserts ≈ 0).
func (s *Scanner) ScanSpace(ctx context.Context, webdavURL, root string) error {
	start := time.Now()
	queue := []string{root}
	seen := map[string]bool{}
	var scanned, changed int

	for len(queue) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		cur := queue[0]
		queue = queue[1:]

		href, err := resolveHref(webdavURL, cur)
		if err != nil {
			s.log.Warn("carpeta inválida, se omite", "path", cur, "err", err)
			continue
		}
		entries, err := s.dav.Propfind(ctx, href, 1)
		if err != nil {
			s.log.Warn("propfind falló, se omite", "path", cur, "err", err)
			continue
		}
		for _, e := range entries {
			if e.IsDir {
				queue = append(queue, e.Href) // href absoluto; resolveHref lo usa tal cual
				continue
			}
			if !s.opts.IsMedia(e.Href) {
				continue
			}
			scanned++
			seen[e.ETag] = true
			mt := "image"
			if s.opts.IsVideo(e.Href) {
				mt = "video"
			}
			mtime := e.LastModified
			if mtime.IsZero() {
				mtime = time.Now()
			}
			p, _ := url.PathUnescape(e.Href)
			_, ch, err := s.store.UpsertByETag(ctx, p, e.ETag, path.Base(p), mt, mtime, e.Size)
			if err != nil {
				s.log.Error("upsert", "path", p, "err", err)
				continue
			}
			if ch {
				changed++
			}
		}
	}

	removed, err := s.store.SoftDeleteExcept(ctx, seen)
	if err != nil {
		return err
	}
	s.log.Info("scan completado",
		"escaneados", scanned, "cambiados", changed, "eliminados", removed,
		"duracion", time.Since(start).Round(time.Second))
	return nil
}
