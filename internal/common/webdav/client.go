// Package webdav: generalized OpenCloud WebDAV/Graph client (port of ocphotos
// internal/dav, SPEC §2.1). Pattern validated by PhotoSort: Graph to discover
// spaces, PROPFIND to walk, GET/Range for content.
// Auth: user + app-token (Basic, New) or OIDC access token (Bearer, NewBearer -
// photos multi-tenant, H8 §6.2).
//
// The image/video extension lists do NOT live in the client: they come out in
// Options (consumed by the photos scanner, §2.1).
package webdav

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// ErrUnauthorized: the server rejected the credential (401/403). It is wrapped
// with %w in the operation error, the photos scanner/worker tell it apart with
// errors.Is to abort without touching the index (H8).
var ErrUnauthorized = errors.New("webdav: credential rejected (401/403)")

// sharedHTTP is the *http.Client of every Client: in multi-tenant mode one
// Client is created per user session and sharing the transport avoids leaking
// connection pools (H8).
var sharedHTTP = &http.Client{Timeout: 120 * time.Second}

const propfindBody = `<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:">
  <d:prop>
    <d:resourcetype/><d:getetag/><d:getlastmodified/>
    <d:getcontentlength/><d:getcontenttype/>
  </d:prop>
</d:propfind>`

// Options: media classification by extension (lowercase, with a dot).
// It is consumed by the photos scanner, the client is agnostic.
type Options struct {
	ImageExts []string
	VideoExts []string
}

// DefaultOptions returns the historical ocphotos extensions.
func DefaultOptions() Options {
	return Options{
		ImageExts: []string{".jpg", ".jpeg", ".png", ".heic", ".heif", ".webp", ".gif", ".tiff", ".dng"},
		VideoExts: []string{".mp4", ".mov", ".m4v", ".webm", ".3gp"},
	}
}

func (o Options) sets() (img, vid map[string]bool) {
	img = make(map[string]bool, len(o.ImageExts))
	for _, e := range o.ImageExts {
		img[strings.ToLower(e)] = true
	}
	vid = make(map[string]bool, len(o.VideoExts))
	for _, e := range o.VideoExts {
		vid[strings.ToLower(e)] = true
	}
	return img, vid
}

// IsImage/IsVideo/IsMedia classify a file name or href by its extension
// (case-insensitive).
func (o Options) IsImage(name string) bool {
	img, _ := o.sets()
	return img[strings.ToLower(path.Ext(name))]
}

func (o Options) IsVideo(name string) bool {
	_, vid := o.sets()
	return vid[strings.ToLower(path.Ext(name))]
}

func (o Options) IsMedia(name string) bool {
	return o.IsImage(name) || o.IsVideo(name)
}

// Entry is a DAV resource returned by Propfind.
type Entry struct {
	Href         string
	IsDir        bool
	ETag         string
	LastModified time.Time
	Size         int64
	ContentType  string
}

// Drive is a Graph space of the user.
type Drive struct {
	ID        string
	Name      string
	DriveType string
	WebDAVURL string
}

type Client struct {
	base string
	http *http.Client
	// authorize sets the request credential (Basic or Bearer). It is never
	// persisted: it only lives in memory as long as the Client lives (H8 §6.2).
	authorize func(req *http.Request)
}

// New creates the client against the OpenCloud server root with Basic auth
// (user + OpenCloud app-token).
func New(baseURL, user, appToken string) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: sharedHTTP,
		authorize: func(req *http.Request) {
			req.SetBasicAuth(user, appToken)
		},
	}
}

// NewBearer creates the client with Bearer auth (OIDC access token of the
// OpenCloud web session). It is the per user client of the multi-tenant photos
// module (H8): the token only lives in memory and expires with the session.
func NewBearer(baseURL, token string) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: sharedHTTP,
		authorize: func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+token)
		},
	}
}

// statusErr wraps ErrUnauthorized on a 401/403 so that the caller
// (photos scanner/worker) can abort without soft-deletes (H8).
func statusErr(op, what, status string, code int) error {
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return fmt.Errorf("%s %s: %s: %w", op, what, status, ErrUnauthorized)
	}
	return fmt.Errorf("%s %s: %s", op, what, status)
}

// FileURL resolves a DAV href (absolute server path) to a full URL.
func (c *Client) FileURL(href string) string {
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	return c.base + escapeHref(href)
}

// escapeHref escapes the characters that break a URL but respects what already
// comes percent-encoded from PROPFIND. Without this, a file named
// "Bug #14_1.png" cuts the URL at the '#' (the rest is parsed as a fragment)
// and the server responds 404: neither thumbnail nor EXIF.
func escapeHref(href string) string {
	var b strings.Builder
	b.Grow(len(href) + 8)
	for i := 0; i < len(href); i++ {
		ch := href[i]
		switch ch {
		case '#':
			b.WriteString("%23")
		case '?':
			b.WriteString("%3F")
		case ' ':
			b.WriteString("%20")
		case '%':
			// an already valid '%' (two hex digits behind) is left as is
			if i+2 < len(href) && isHex(href[i+1]) && isHex(href[i+2]) {
				b.WriteByte(ch)
			} else {
				b.WriteString("%25")
			}
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// MeID returns the user id of the client credential (Basic or Bearer). Photos
// uses it to resolve the multi-tenant owner (H8).
func (c *Client) MeID(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/graph/v1.0/me", nil)
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", statusErr("graph", "/me", resp.Status, resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// ListDrives calls GET /graph/v1.0/me/drives and returns the spaces of the
// user.
func (c *Client) ListDrives(ctx context.Context) ([]Drive, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/graph/v1.0/me/drives", nil)
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr("graph", "/me/drives", resp.Status, resp.StatusCode)
	}
	var out struct {
		Value []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			DriveType string `json:"driveType"`
			Root      struct {
				WebDAVURL string `json:"webDavUrl"`
			} `json:"root"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	drives := make([]Drive, 0, len(out.Value))
	for _, d := range out.Value {
		drives = append(drives, Drive{d.ID, d.Name, d.DriveType, d.Root.WebDAVURL})
	}
	return drives, nil
}

// SpaceFileURL builds the internal URL of a file from the webDavUrl of the
// space and its relative path ("/Fotos/IMG.heic"), using the configured base
// (it allows using 127.0.0.1 instead of the public URL).
func (c *Client) SpaceFileURL(webdavURL, relPath string) string {
	u, err := url.Parse(webdavURL)
	if err != nil || u.Path == "" {
		return c.base + relPath
	}
	segs := strings.Split(strings.Trim(relPath, "/"), "/")
	esc := make([]string, 0, len(segs))
	for _, s := range segs {
		if s != "" {
			esc = append(esc, url.PathEscape(s))
		}
	}
	return c.base + strings.TrimRight(u.Path, "/") + "/" + strings.Join(esc, "/")
}

type multistatus struct {
	Responses []struct {
		Href     string `xml:"href"`
		PropStat []struct {
			Prop struct {
				ResourceType struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"resourcetype"`
				ETag         string `xml:"getetag"`
				LastModified string `xml:"getlastmodified"`
				Length       int64  `xml:"getcontentlength"`
				ContentType  string `xml:"getcontenttype"`
			} `xml:"prop"`
			Status string `xml:"status"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// Propfind runs PROPFIND on href with the requested depth (0 = the resource
// only, 1 = one level). href is an absolute server path ("/dav/spaces/...") or
// a full URL, the entry of the resource itself is excluded in depth 1
// (behavior inherited from ListFolder).
func (c *Client) Propfind(ctx context.Context, href string, depth int) ([]Entry, error) {
	if depth < 0 || depth > 1 {
		return nil, fmt.Errorf("unsupported depth: %d (only 0 or 1)", depth)
	}
	if strings.Contains(href, "..") {
		return nil, fmt.Errorf("path traversal rejected: %q", href)
	}
	u := c.FileURL(href)

	req, _ := http.NewRequestWithContext(ctx, "PROPFIND", u, bytes.NewBufferString(propfindBody))
	c.authorize(req)
	req.Header.Set("Depth", fmt.Sprintf("%d", depth))
	req.Header.Set("Content-Type", "application/xml")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("resource not found: %s", href)
	}
	if resp.StatusCode >= 300 {
		return nil, statusErr("propfind", href, resp.Status, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var ms multistatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return nil, err
	}

	selfPath, _ := url.Parse(u)
	entries := make([]Entry, 0, len(ms.Responses))
	// The server answers with the hrefs percent-encoded and url.Parse keeps the
	// request path decoded, so comparing them raw only works for plain ASCII
	// names. A folder with a space or an accent ("Diseño/") did not match, its
	// own entry was returned as a child and the scanner queued the folder
	// again: an endless walk of the same tree (issue #72).
	self := ""
	selfEscaped := ""
	if selfPath != nil {
		self = selfPath.Path
		selfEscaped = selfPath.EscapedPath()
	}
	for _, r := range ms.Responses {
		// exclude the entry of the resource itself
		if r.Href == self || r.Href == selfEscaped {
			continue
		}
		if unescaped, err := url.PathUnescape(r.Href); err == nil && unescaped == self {
			continue
		}
		if len(r.PropStat) == 0 || !strings.Contains(r.PropStat[0].Status, "200") {
			continue
		}
		p := r.PropStat[0].Prop
		mtime, _ := time.Parse(time.RFC1123, p.LastModified)
		entries = append(entries, Entry{
			Href:         r.Href,
			IsDir:        p.ResourceType.Collection != nil,
			ETag:         p.ETag,
			LastModified: mtime,
			Size:         p.Length,
			ContentType:  p.ContentType,
		})
	}
	return entries, nil
}

// GetRange downloads n bytes starting at offset off (e.g. the EXIF header
// without downloading the whole file). off=0, n=64Ki downloads the first
// 64 KiB.
func (c *Client) GetRange(ctx context.Context, href string, off, n int64) ([]byte, error) {
	if off < 0 || n <= 0 {
		return nil, fmt.Errorf("invalid range: off=%d n=%d", off, n)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.FileURL(href), nil)
	c.authorize(req)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return nil, statusErr("range", href, resp.Status, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// DownloadRange does a GET passing the Range header (video streaming).
// It returns the body, the headers and the status code (200 or 206).
func (c *Client) DownloadRange(ctx context.Context, href, rangeHeader string) (io.ReadCloser, http.Header, int, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.FileURL(href), nil)
	c.authorize(req)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	if resp.StatusCode >= 400 {
		_ = resp.Body.Close()
		return nil, nil, resp.StatusCode, statusErr("range", href, resp.Status, resp.StatusCode)
	}
	return resp.Body, resp.Header, resp.StatusCode, nil
}

// Download opens a stream of the whole file (the caller closes the body).
func (c *Client) Download(ctx context.Context, href string) (io.ReadCloser, string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.FileURL(href), nil)
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, "", statusErr("download", href, resp.Status, resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	return resp.Body, ct, nil
}

// Put writes a small file to WebDAV (XMP sidecars). It replaces the content if
// it already exists.
func (c *Client) Put(ctx context.Context, href, contentType string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.FileURL(href), bytes.NewReader(body))
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return statusErr("put", href, resp.Status, resp.StatusCode)
	}
	return nil
}

// GetSmall reads a small file completely. A 404 returns found=false without an
// error: it is the normal response when the sidecar does not exist yet.
func (c *Client) GetSmall(ctx context.Context, href string) (data []byte, found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.FileURL(href), nil)
	if err != nil {
		return nil, false, err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode >= 300 {
		return nil, false, statusErr("get", href, resp.Status, resp.StatusCode)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}
