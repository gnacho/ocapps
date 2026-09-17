// Package webdav: cliente WebDAV/Graph de OpenCloud generalizado (port de
// ocphotos internal/dav, SPEC §2.1). Patrón validado por PhotoSort: Graph
// para descubrir espacios, PROPFIND para recorrer, GET/Range para contenido.
// Auth: usuario + app-token (Basic, New) o access token OIDC (Bearer,
// NewBearer — multi-tenant de photos, H8 §6.2).
//
// Las listas de extensiones image/video NO viven en el cliente: salen a
// Options (las consume el scanner de photos, §2.1).
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

// ErrUnauthorized: el servidor rechazó la credencial (401/403). Va envuelto
// con %w en el error de la operación; el scanner/worker de photos lo
// distinguen con errors.Is para abortar sin tocar el índice (H8).
var ErrUnauthorized = errors.New("webdav: credencial rechazada (401/403)")

// sharedHTTP es el *http.Client de todos los Client: en multi-tenant se
// crea un Client por sesión de usuario y compartir el transporte evita
// fugas de pools de conexiones (H8).
var sharedHTTP = &http.Client{Timeout: 120 * time.Second}

const propfindBody = `<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:">
  <d:prop>
    <d:resourcetype/><d:getetag/><d:getlastmodified/>
    <d:getcontentlength/><d:getcontenttype/>
  </d:prop>
</d:propfind>`

// Options: clasificación de medios por extensión (minúsculas, con punto).
// La consume el scanner de photos; el cliente es agnóstico.
type Options struct {
	ImageExts []string
	VideoExts []string
}

// DefaultOptions devuelve las extensiones históricas de ocphotos.
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

// IsImage/IsVideo/IsMedia clasifican un nombre de fichero o href por su
// extensión (case-insensitive).
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

// Entry es un recurso DAV devuelto por Propfind.
type Entry struct {
	Href         string
	IsDir        bool
	ETag         string
	LastModified time.Time
	Size         int64
	ContentType  string
}

// Drive es un espacio Graph del usuario.
type Drive struct {
	ID        string
	Name      string
	DriveType string
	WebDAVURL string
}

type Client struct {
	base string
	http *http.Client
	// authorize fija la credencial de la petición (Basic o Bearer). Nunca se
	// persiste: vive solo en memoria mientras viva el Client (H8 §6.2).
	authorize func(req *http.Request)
}

// New crea el cliente contra la raíz del servidor OpenCloud con auth Basic
// (usuario + app-token de OpenCloud).
func New(baseURL, user, appToken string) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: sharedHTTP,
		authorize: func(req *http.Request) {
			req.SetBasicAuth(user, appToken)
		},
	}
}

// NewBearer crea el cliente con auth Bearer (access token OIDC de la sesión
// web de OpenCloud). Es el cliente por usuario del photos multi-tenant (H8):
// el token solo vive en memoria y caduca con la sesión.
func NewBearer(baseURL, token string) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: sharedHTTP,
		authorize: func(req *http.Request) {
			req.Header.Set("Authorization", "Bearer "+token)
		},
	}
}

// statusErr envuelve ErrUnauthorized ante un 401/403 para que el llamador
// (scanner/worker de photos) pueda abortar sin soft-deletes (H8).
func statusErr(op, what, status string, code int) error {
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return fmt.Errorf("%s %s: %s: %w", op, what, status, ErrUnauthorized)
	}
	return fmt.Errorf("%s %s: %s", op, what, status)
}

// FileURL resuelve un href DAV (ruta absoluta del servidor) a URL completa.
func (c *Client) FileURL(href string) string {
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href
	}
	return c.base + href
}

// MeID devuelve el id del usuario de la credencial del cliente (Basic o
// Bearer). Photos lo usa para resolver el owner multi-tenant (H8).
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

// ListDrives llama a GET /graph/v1.0/me/drives y devuelve los espacios del
// usuario.
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

// SpaceFileURL construye la URL interna de un fichero a partir del webDavUrl
// del espacio y de su ruta relativa ("/Fotos/IMG.heic"), usando la base
// configurada (permite usar 127.0.0.1 en vez de la URL pública).
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

// Propfind hace PROPFIND sobre href con la profundidad pedida (0 = solo el
// recurso, 1 = un nivel). href es una ruta absoluta del servidor
// ("/dav/spaces/...") o una URL completa; se excluye la entrada del propio
// recurso en depth 1 (comportamiento heredado de ListFolder).
func (c *Client) Propfind(ctx context.Context, href string, depth int) ([]Entry, error) {
	if depth < 0 || depth > 1 {
		return nil, fmt.Errorf("depth no soportado: %d (solo 0 o 1)", depth)
	}
	if strings.Contains(href, "..") {
		return nil, fmt.Errorf("path traversal rechazado: %q", href)
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
		return nil, fmt.Errorf("recurso no encontrado: %s", href)
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
	for _, r := range ms.Responses {
		// excluye la entrada del propio recurso
		if selfPath != nil && r.Href == selfPath.Path {
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

// GetRange descarga n bytes a partir del offset off (p. ej. la cabecera EXIF
// sin bajar el fichero entero). off=0, n=64Ki descarga los primeros 64 KiB.
func (c *Client) GetRange(ctx context.Context, href string, off, n int64) ([]byte, error) {
	if off < 0 || n <= 0 {
		return nil, fmt.Errorf("rango inválido: off=%d n=%d", off, n)
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

// DownloadRange hace un GET pasando la cabecera Range (streaming de vídeo).
// Devuelve el body, las cabeceras y el código de estado (200 o 206).
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

// Download abre un stream del fichero completo (caller cierra el body).
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
