// Package favicon: fetches, caches and serves the favicon of each feed.
// Cache in DATA_DIR/favicons/{url_hash}.ext, the API v1.3 endpoint
// GET /favicon/{feedUrlHash} reads it (hash = md5 of the feed URL).
package favicon

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gnacho/ocapps/internal/common/netguard"
)

const (
	maxFaviconBytes = 512 << 10 // 512 KB
	fetchTimeout    = 10 * time.Second
	userAgent       = "ocnews/0.5 (+https://github.com/gnacho/ocnews)"
)

type Cache struct {
	dir    string
	client *http.Client
	log    *slog.Logger
}

func NewCache(dir string, log *slog.Logger) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create favicons cache: %w", err)
	}
	return &Cache{dir: dir, client: netguard.Client(fetchTimeout), log: log}, nil
}

// Hash returns the hex md5 of a feed URL (endpoint identifier).
func Hash(feedURL string) string {
	sum := md5.Sum([]byte(feedURL))
	return fmt.Sprintf("%x", sum)
}

// Fetch downloads the favicon of the feed site (best-effort, errors
// are only logged). It tries /favicon.ico from the origin of the site link.
// The cache is indexed by the hash of the FEED URL (the one the endpoint uses).
func (c *Cache) Fetch(ctx context.Context, feedURL, siteLink string) {
	icoURL, ok := faviconURL(siteLink)
	if !ok {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, icoURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.client.Do(req)
	if err != nil {
		c.log.Debug("favicon not downloaded", "url", icoURL, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFaviconBytes))
	if err != nil || len(body) == 0 {
		return
	}
	ctype := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.HasPrefix(ctype, "image/") {
		return // error HTML or other content, not valid as an icon
	}
	_ = os.WriteFile(c.pathFor(Hash(feedURL)+ext(ctype)), body, 0o600)
}

// Has says whether there is already a cached favicon for the given hash.
func (c *Cache) Has(hash string) bool {
	for _, ext := range []string{".ico", ".png", ".svg", ".jpg", ".jpeg", ".gif", ".webp"} {
		if _, err := os.Stat(c.pathFor(hash + ext)); err == nil {
			return true
		}
	}
	return false
}

// Serve responds with the cached favicon for the given hash, or 404.
func (c *Cache) Serve(w http.ResponseWriter, hash string) {
	for _, ext := range []string{".ico", ".png", ".svg", ".jpg", ".jpeg", ".gif", ".webp"} {
		p := c.pathFor(hash + ext)
		if b, err := os.ReadFile(p); err == nil {
			w.Header().Set("Content-Type", contentType(ext))
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(b)
			return
		}
	}
	http.NotFound(w, nil)
}

func (c *Cache) pathFor(name string) string {
	return filepath.Join(c.dir, name)
}

// faviconURL derives https://host/favicon.ico from the site link.
func faviconURL(siteLink string) (string, bool) {
	if siteLink == "" {
		return "", false
	}
	u, err := url.Parse(siteLink)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return u.Scheme + "://" + u.Host + "/favicon.ico", true
}

func ext(ctype string) string {
	switch {
	case strings.Contains(ctype, "svg"):
		return ".svg"
	case strings.Contains(ctype, "png"):
		return ".png"
	case strings.Contains(ctype, "jpeg") || strings.Contains(ctype, "jpg"):
		return ".jpg"
	case strings.Contains(ctype, "gif"):
		return ".gif"
	case strings.Contains(ctype, "webp"):
		return ".webp"
	default:
		return ".ico"
	}
}

func contentType(extn string) string {
	switch extn {
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/x-icon"
	}
}
