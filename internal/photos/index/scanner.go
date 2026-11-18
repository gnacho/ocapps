// Package index - incremental WebDAV scanner over OpenCloud.
package index

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/gnacho/ocapps/internal/common/webdav"
	"github.com/gnacho/ocapps/internal/photos/store"
	"github.com/gnacho/ocapps/internal/xmp"
)

type Scanner struct {
	dav   *webdav.Client // client OF ONE USER (owner session, H8)
	opts  webdav.Options // media classification by extension (SPEC §2.1)
	store *store.Store
	log   *slog.Logger

	// OnIndexChange, if not nil, is called when a scan that has changed the
	// index finishes (additions, removals or applied XMP metadata). The module
	// uses it to notify connected clients, nil = do not notify.
	OnIndexChange func(owner string)
}

// NewScanner creates the scanner for ONE owner: dav is the client of its
// session (web Bearer or app-token Basic, H8). The module builds one Scanner
// per scan with the session of the owner to scan.
func NewScanner(c *webdav.Client, opts webdav.Options, st *store.Store, log *slog.Logger) *Scanner {
	return &Scanner{dav: c, opts: opts, store: st, log: log}
}

// resolveHref turns a folder reference into an absolute server href for
// Propfind: an absolute href (returned by a previous listing) is used as is, a
// relative path ("Fotos", "" = space root) is resolved against the path of the
// webDavUrl of the space. It is equivalent to the resolution the old
// dav.ListFolder did.
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
			return "", fmt.Errorf("path traversal rejected: %q", folderRef)
		}
		base += "/" + url.PathEscape(seg)
	}
	return base + "/", nil
}

// scanPrefix is the prefix of the paths a scan covers (root included): it
// bounds the soft-delete so that it cannot delete assets outside the folder
// walked (issue #36). With an empty root it covers the whole space.
func scanPrefix(webdavURL, root string) string {
	u, err := url.Parse(webdavURL)
	if err != nil {
		return ""
	}
	base := strings.TrimRight(u.Path, "/")
	root = strings.Trim(root, "/")
	if root == "" {
		return base + "/"
	}
	return base + "/" + root + "/"
}

// minFailGuard is the minimum number of PROPFINDs for the sanity guard (>50%
// failed) to kick in: with fewer isolated failures the scan completes as
// before.
const minFailGuard = 10

// Space is the drive a scan walks (issue #145): the id after "$" in
// /dav/spaces/<storage>$<space>, its display name, its driveType and the
// webDavUrl of its root.
type Space struct {
	ID        string
	Name      string
	Type      string
	WebDAVURL string
}

// ScanSpace walks the space OF ONE OWNER from root (for example "Fotos", "" =
// the whole space). Incremental by etag: upsert only of what changed,
// soft-delete (scoped to the prefix of the space) of what disappeared.
// 70k photos is roughly a few thousand PROPFINDs: several minutes for the
// first scan, seconds for the incremental ones (same number of requests, but
// upserts roughly 0).
//
// Index protections (H8):
//   - If any PROPFIND returns webdav.ErrUnauthorized the scan ABORTS
//     propagating the error WITHOUT running SoftDeleteExcept: with an expired
//     token the scan would "complete" with 0 seen and delete the whole index.
//   - Sanity guard: if more than 50% of the PROPFINDs fail (and there were at
//     least 10), it aborts as well with no soft-delete.
func (s *Scanner) ScanSpace(ctx context.Context, owner string, space Space, root string) error {
	start := time.Now()
	webdavURL := space.WebDAVURL
	queue := []string{root}
	seen := map[string]bool{}
	var scanned, changed, attempts, failures, xmpApplied int

	// A space without the configured root folder (for example "Fotos" on a
	// project space): the scan is skipped instead of walking nothing and
	// soft-deleting the prefix of a folder that does not exist. Only the radix
	// of the space is walked when root is empty.
	if root != "" {
		href, err := resolveHref(webdavURL, root)
		if err != nil {
			return err
		}
		if _, err := s.dav.Propfind(ctx, href, 0); err != nil {
			if errors.Is(err, webdav.ErrUnauthorized) {
				return err
			}
			s.log.Info("space skipped: root folder not reachable", "owner", owner, "space", space.Name, "root", root, "err", err)
			return nil
		}
	}

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
			s.log.Warn("invalid folder, skipped", "owner", owner, "path", cur, "err", err)
			continue
		}
		attempts++
		entries, err := s.dav.Propfind(ctx, href, 1)
		if err != nil {
			if errors.Is(err, webdav.ErrUnauthorized) {
				// expired credential: abort NOW and leave the index untouched
				return fmt.Errorf("scan of %q aborted (no soft-delete): %w", owner, err)
			}
			failures++
			s.log.Warn("propfind failed, skipped", "owner", owner, "path", cur, "err", err)
			continue
		}
		// The .xmp sidecars come in the same listing as the photos, so
		// knowing which ones exist costs no extra request. They are
		// downloaded separately, and only when their etag changed since the
		// last scan.
		sidecars := make(map[string]webdav.Entry)
		for _, e := range entries {
			if e.IsDir {
				queue = append(queue, e.Href) // absolute href, resolveHref uses it as is
				continue
			}
			if strings.HasSuffix(strings.ToLower(e.Href), ".xmp") {
				p, _ := url.PathUnescape(e.Href)
				sidecars[p[:len(p)-4]] = e
			}
		}
		for _, e := range entries {
			if e.IsDir {
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
			id, ch, err := s.store.UpsertInSpace(ctx, owner, p, e.ETag, path.Base(p), mt, mtime, e.Size,
				store.Space{ID: space.ID, Name: space.Name, Type: space.Type})
			if err != nil {
				s.log.Error("upsert", "owner", owner, "path", p, "err", err)
				continue
			}
			if ch {
				changed++
			}
			if sc, ok := sidecars[p]; ok && s.applyXMP(ctx, owner, id, sc) {
				xmpApplied++
			}
		}
	}

	if attempts >= minFailGuard && failures > attempts/2 {
		return fmt.Errorf("scan of %q aborted (no soft-delete): %d/%d PROPFINDs failed",
			owner, failures, attempts)
	}

	removed, err := s.store.SoftDeleteExcept(ctx, owner, scanPrefix(webdavURL, root), seen)
	if err != nil {
		return err
	}
	s.log.Info("scan completed",
		"owner", owner, "scanned", scanned, "changed", changed, "removed", removed,
		"xmp", xmpApplied,
		"duration", time.Since(start).Round(time.Second))
	if changed > 0 || removed > 0 || xmpApplied > 0 {
		if s.OnIndexChange != nil {
			s.OnIndexChange(owner)
		}
	}
	return nil
}

// applyXMP projects the XMP sidecar of an asset onto the index, if it changed
// since the last scan. It is applied conservatively so as not to destroy local
// metadata with a foreign sidecar: the favourite flag is only touched when the
// package carries xmp:Rating, and the tags are only replaced when it carries at
// least one. It returns true if anything changed in the index.
func (s *Scanner) applyXMP(ctx context.Context, owner string, id int64, sidecar webdav.Entry) bool {
	prev, err := s.store.SidecarETag(ctx, owner, id)
	if err != nil {
		s.log.Warn("xmp: could not read the applied etag", "owner", owner, "id", id, "err", err)
		return false
	}
	if prev != "" && prev == sidecar.ETag {
		return false
	}
	data, found, err := s.dav.GetSmall(ctx, sidecar.Href)
	if err != nil {
		s.log.Warn("xmp: could not read the sidecar", "owner", owner, "href", sidecar.Href, "err", err)
		return false
	}
	if !found {
		return false
	}
	meta := xmp.Extract(data)
	a, err := s.store.AssetByID(ctx, owner, id)
	if err != nil {
		s.log.Warn("xmp: could not read the asset", "owner", owner, "id", id, "err", err)
		return false
	}
	applied := false
	if meta.HasRating && a.IsFavorite != meta.Favorite {
		if err := s.store.SetFavorite(ctx, owner, id, meta.Favorite); err != nil {
			s.log.Warn("xmp: could not apply the favourite flag", "owner", owner, "id", id, "err", err)
			return applied
		}
		applied = true
	}
	if len(meta.Tags) > 0 {
		cur, err := s.store.AssetTags(ctx, owner, id)
		if err != nil {
			s.log.Warn("xmp: could not read the tags", "owner", owner, "id", id, "err", err)
			return applied
		}
		if !sameTags(cur, meta.Tags) {
			if err := s.store.SetTags(ctx, owner, id, meta.Tags); err != nil {
				s.log.Warn("xmp: could not apply the tags", "owner", owner, "id", id, "err", err)
				return applied
			}
			applied = true
		}
	}
	if err := s.store.SetSidecarETag(ctx, owner, id, sidecar.ETag); err != nil {
		s.log.Warn("xmp: could not store the sidecar etag", "owner", owner, "id", id, "err", err)
	}
	return applied
}

// sameTags compares two sets of tags regardless of order (the index returns
// them sorted and the sidecar brings them in its writing order).
func sameTags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, t := range a {
		seen[t] = true
	}
	for _, t := range b {
		if !seen[t] {
			return false
		}
	}
	return true
}
