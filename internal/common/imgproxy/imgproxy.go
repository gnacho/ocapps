// Package imgproxy: signed image proxy (merge of
// ocnews/internal/imgproxy and ocnotes/internal/imgproxy). OpenCloud hosts
// serve CSP img-src 'self' (no external domains), so the frontends cannot load
// external images directly. This proxy downloads them and serves them from the
// domain itself. The browser <img> tag CANNOT carry auth headers, so the route
// is public but the URL is signed with HMAC-SHA256 (persistent secret per
// module): only URLs signed by the server itself are proxied. It mitigates
// SSRF (netguard transport) and abuse.
//
// SIGNATURE COMPATIBILITY: the algorithm is exactly the one of
// ocnews/ocnotes (hex(HMAC-SHA256(secret, url))[:32]), URLs signed by the old
// services keep validating as long as the imgsecret file is kept.
// One instance PER MODULE, each one with its imgsecret and imgcache in its own
// data subdir (SPEC §2.1): signatures stay isolated per module as today.
package imgproxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gnacho/ocapps/internal/common/cred"
	"github.com/gnacho/ocapps/internal/common/netguard"
)

const (
	maxImageBytes = 8 << 20 // 8 MB
	fetchTimeout  = 15 * time.Second
	userAgent     = "ocapps/0.1 (+https://github.com/gnacho/ocapps)"
)

type Proxy struct {
	secret []byte
	dir    string // on disk cache
	client *http.Client
	log    *slog.Logger
}

// New loads (or generates) the secret in <dataDir>/imgsecret and prepares the
// cache in <dataDir>/imgcache. A corrupt imgsecret (<32 bytes) is an error
// (fail loud, SPEC Q3) instead of regenerating it and silently breaking
// signatures.
func New(dataDir string, log *slog.Logger) (*Proxy, error) {
	secret, err := cred.LoadOrCreateSecret(filepath.Join(dataDir, "imgsecret"))
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(dataDir, "imgcache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Proxy{
		secret: secret,
		dir:    dir,
		client: netguard.Client(fetchTimeout),
		log:    log,
	}, nil
}

// NewAllowLocal is like New but the transport allows loopback. Tests only.
func NewAllowLocal(dataDir string, log *slog.Logger) (*Proxy, error) {
	p, err := New(dataDir, log)
	if err != nil {
		return nil, err
	}
	p.client = netguard.ClientAllowLocal(fetchTimeout)
	return p, nil
}

// Sign returns the HMAC signature (hex, 32 chars) of an image URL.
// FIXED ALGORITHM: hex(HMAC-SHA256(secret, url))[:32] - do not touch it
// (compatibility with the signatures issued by ocnews/ocnotes, see
// imgproxy_test.go).
func (p *Proxy) Sign(rawURL string) string {
	mac := hmac.New(sha256.New, p.secret)
	mac.Write([]byte(rawURL))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// ProxiedURL builds the public proxy URL for a signed URL.
// u travels fully url.QueryEscape'd (including %->%25) so that q.Get("u")
// recovers EXACTLY the URL that was signed (port of ocnotes).
func ProxiedURL(base, rawURL, sig string) string {
	return base + "img?u=" + url.QueryEscape(rawURL) + "&t=" + sig
}

func (p *Proxy) valid(rawURL, sig string) bool {
	return hmac.Equal([]byte(p.Sign(rawURL)), []byte(sig))
}

func cachePath(rawURL string) string {
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])
}

// Serve responds with the image (24h on disk cache) or streams audio/video
// (with Range support for seeking, no on disk cache). Invalid signature -> 403.
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	u, sig := q.Get("u"), q.Get("t")
	if u == "" || !p.valid(u, sig) {
		http.Error(w, "invalid signature", http.StatusForbidden)
		return
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		http.Error(w, "scheme not allowed", http.StatusForbidden)
		return
	}

	// HEAD to the origin: by content-type we decide image (cache) or media (stream)
	ct, ok := p.probeContentType(r.Context(), u)
	if !ok {
		http.Error(w, "not available", http.StatusBadGateway)
		return
	}
	switch {
	case strings.HasPrefix(ct, "image/"):
		p.serveImage(w, r, u, ct)
	case strings.HasPrefix(ct, "audio/"), strings.HasPrefix(ct, "video/"):
		p.serveMedia(w, r, u, ct)
	default:
		http.Error(w, "type not allowed", http.StatusUnsupportedMediaType)
	}
}

// probeContentType discovers the content-type of the origin. HEAD first, if
// the origin does not support HEAD (405/501), GET with Range 0-0 and close
// right away.
func (p *Proxy) probeContentType(ctx context.Context, u string) (string, bool) {
	for _, probe := range []struct {
		method string
		rng    string
	}{
		{http.MethodHead, ""},
		{http.MethodGet, "bytes=0-0"},
	} {
		req, err := http.NewRequestWithContext(ctx, probe.method, u, nil)
		if err != nil {
			return "", false
		}
		req.Header.Set("User-Agent", userAgent)
		if probe.rng != "" {
			req.Header.Set("Range", probe.rng)
		}
		resp, err := p.client.Do(req)
		if err != nil {
			p.log.Debug("imgproxy probe failed", "url", u, "err", err)
			return "", false
		}
		ct := strings.ToLower(resp.Header.Get("Content-Type"))
		code := resp.StatusCode
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
		_ = resp.Body.Close()
		if code == http.StatusOK || code == http.StatusPartialContent {
			if ct != "" && ct != "application/octet-stream" {
				return ct, true
			}
			// octet-stream or empty with 200: it may be media without the right
			// mime, we let it through and serveMedia/serveImage evaluates again
			if code == http.StatusOK && ct == "" {
				return "application/octet-stream", true
			}
		}
	}
	return "", false
}

// serveImage: image with on disk cache (original behavior).
func (p *Proxy) serveImage(w http.ResponseWriter, r *http.Request, u, ct string) {
	base := filepath.Join(p.dir, cachePath(u))
	if cachedCT, err := os.ReadFile(base + ".ct"); err == nil {
		if img, err := os.ReadFile(base); err == nil {
			p.respond(w, string(cachedCT), img)
			return
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
	if err != nil {
		http.Error(w, "invalid url", http.StatusForbidden)
		return
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := p.client.Do(req)
	if err != nil {
		http.Error(w, "not available", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "not available", http.StatusBadGateway)
		return
	}
	if ct2 := strings.ToLower(resp.Header.Get("Content-Type")); !strings.HasPrefix(ct2, "image/") {
		http.Error(w, "not an image", http.StatusUnsupportedMediaType)
		return
	}
	img, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes))
	if err != nil || len(img) == 0 {
		http.Error(w, "not available", http.StatusBadGateway)
		return
	}
	_ = os.WriteFile(base, img, 0o600)
	_ = os.WriteFile(base+".ct", []byte(ct), 0o600)
	p.respond(w, ct, img)
}

// serveMedia: audio/video streaming with Range passthrough (the browser
// <video> needs 206 to seek). No full buffer and no on disk cache.
func (p *Proxy) serveMedia(w http.ResponseWriter, r *http.Request, u, ct string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, u, nil)
	if err != nil {
		http.Error(w, "invalid url", http.StatusForbidden)
		return
	}
	req.Header.Set("User-Agent", userAgent)
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		p.log.Debug("imgproxy media failed", "url", u, "err", err)
		http.Error(w, "not available", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		http.Error(w, "not available", http.StatusBadGateway)
		return
	}

	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Accept-Ranges", "bytes")
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		h.Set("Content-Range", cr)
	}
	h.Set("Cache-Control", "public, max-age=86400")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		h.Set("Content-Length", cl)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) respond(w http.ResponseWriter, ct string, img []byte) {
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(img)
}
