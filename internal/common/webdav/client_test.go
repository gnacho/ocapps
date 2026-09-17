package webdav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOptionsClasificacion(t *testing.T) {
	o := DefaultOptions()
	for name, want := range map[string]bool{
		"/Fotos/IMG_0001.JPG":  true,
		"/Fotos/foto.heic":     true,
		"/Fotos/video.mov":     true,
		"/Fotos/clip.WEBM":     true,
		"/Fotos/nota.txt":      false,
		"/Fotos/sin-extension": false,
	} {
		if got := o.IsMedia(name); got != want {
			t.Errorf("IsMedia(%q): got %v, want %v", name, got, want)
		}
	}
	if !o.IsImage("a.png") || o.IsVideo("a.png") {
		t.Error("png debe ser imagen y no vídeo")
	}
	if !o.IsVideo("a.mp4") || o.IsImage("a.mp4") {
		t.Error("mp4 debe ser vídeo y no imagen")
	}
	// opciones personalizadas (salen del cliente al scanner, SPEC §2.1)
	custom := Options{ImageExts: []string{".arw"}}
	if !custom.IsImage("foto.ARW") || custom.IsMedia("foto.jpg") {
		t.Error("Options personalizadas no respetadas")
	}
}

func TestSpaceFileURL(t *testing.T) {
	c := New("https://internal:9200", "u", "t")
	got := c.SpaceFileURL("https://public.example.com/dav/spaces/personal$alice", "/Fotos/Viaje 2025/IMG 1.heic")
	want := "https://internal:9200/dav/spaces/personal$alice/Fotos/Viaje%202025/IMG%201.heic"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// webdavURL vacía → base + relPath
	if got := c.SpaceFileURL("", "/Fotos/a.jpg"); got != "https://internal:9200/Fotos/a.jpg" {
		t.Fatalf("fallback: %q", got)
	}
}

const multistatusXML = `<?xml version="1.0"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/dav/spaces/personal/Fotos/</d:href>
    <d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  </d:response>
  <d:response>
    <d:href>/dav/spaces/personal/Fotos/IMG.jpg</d:href>
    <d:propstat><d:prop>
      <d:resourcetype/>
      <d:getetag>"abc123"</d:getetag>
      <d:getlastmodified>Mon, 02 Jan 2006 15:04:05 GMT</d:getlastmodified>
      <d:getcontentlength>12345</d:getcontentlength>
      <d:getcontenttype>image/jpeg</d:getcontenttype>
    </d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  </d:response>
  <d:response>
    <d:href>/dav/spaces/personal/Fotos/Viajes/</d:href>
    <d:propstat><d:prop>
      <d:resourcetype><d:collection/></d:resourcetype>
    </d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  </d:response>
</d:multistatus>`

func TestPropfind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			t.Errorf("método: %s", r.Method)
		}
		if r.Header.Get("Depth") != "1" {
			t.Errorf("Depth: %q", r.Header.Get("Depth"))
		}
		u, p, ok := r.BasicAuth()
		if !ok || u != "alice" || p != "tok" {
			t.Errorf("auth: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprint(w, multistatusXML)
	}))
	defer srv.Close()

	c := New(srv.URL, "alice", "tok")
	entries, err := c.Propfind(context.Background(), "/dav/spaces/personal/Fotos/", 1)
	if err != nil {
		t.Fatalf("Propfind: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries: got %d, want 2 (self excluido)", len(entries))
	}
	var img, dir *Entry
	for i := range entries {
		switch entries[i].IsDir {
		case false:
			img = &entries[i]
		case true:
			dir = &entries[i]
		}
	}
	if img == nil || img.Href != "/dav/spaces/personal/Fotos/IMG.jpg" ||
		img.ETag != `"abc123"` || img.Size != 12345 || img.ContentType != "image/jpeg" {
		t.Fatalf("entry imagen: %+v", img)
	}
	if img.LastModified.IsZero() {
		t.Fatal("LastModified no parseado")
	}
	if dir == nil || dir.Href != "/dav/spaces/personal/Fotos/Viajes/" {
		t.Fatalf("entry dir: %+v", dir)
	}

	if _, err := c.Propfind(context.Background(), "/dav/../etc", 1); err == nil {
		t.Fatal("path traversal no rechazado")
	}
	if _, err := c.Propfind(context.Background(), "/dav/x", 2); err == nil {
		t.Fatal("depth>1 no rechazado")
	}
}

func TestGetRangeConOffset(t *testing.T) {
	data := []byte("0123456789abcdef")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var off, end int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &off, &end); err != nil {
			t.Errorf("Range: %q", r.Header.Get("Range"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[off : end+1])
	}))
	defer srv.Close()

	c := New(srv.URL, "u", "t")
	got, err := c.GetRange(context.Background(), "/dav/f.bin", 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "4567" {
		t.Fatalf("got %q, want %q", got, "4567")
	}
	if _, err := c.GetRange(context.Background(), "/dav/f.bin", -1, 4); err == nil {
		t.Fatal("offset negativo no rechazado")
	}
	if _, err := c.GetRange(context.Background(), "/dav/f.bin", 0, 0); err == nil {
		t.Fatal("n=0 no rechazado")
	}
}

func TestDownloadYMeID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/graph/v1.0/me":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"id-alice"}`)
		case "/dav/f.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			fmt.Fprint(w, "jpeg-bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "u", "t")
	id, err := c.MeID(context.Background())
	if err != nil || id != "id-alice" {
		t.Fatalf("MeID: %q %v", id, err)
	}
	body, ct, err := c.Download(context.Background(), "/dav/f.jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	b, _ := io.ReadAll(body)
	if string(b) != "jpeg-bytes" || ct != "image/jpeg" {
		t.Fatalf("Download: %q %q", b, ct)
	}
}

// TestNewBearerAuth: NewBearer manda "Authorization: Bearer <token>" en TODAS
// las operaciones (Graph, PROPFIND, GET, Range) y nunca Basic (H8 §2).
func TestNewBearerAuth(t *testing.T) {
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Method+" "+r.Header.Get("Authorization"))
		switch {
		case r.URL.Path == "/graph/v1.0/me":
			fmt.Fprint(w, `{"id":"u1"}`)
		case r.URL.Path == "/graph/v1.0/me/drives":
			fmt.Fprint(w, `{"value":[]}`)
		case r.Method == "PROPFIND":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprint(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"/>`)
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
			fmt.Fprint(w, "datos")
		}
	}))
	defer srv.Close()

	c := NewBearer(srv.URL, "oidc-token-123")
	ctx := context.Background()
	if _, err := c.MeID(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListDrives(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Propfind(ctx, "/dav/spaces/x/", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetRange(ctx, "/dav/spaces/x/a.jpg", 0, 4); err != nil {
		t.Fatal(err)
	}
	rc, _, err := c.Download(ctx, "/dav/spaces/x/a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	rc2, _, _, err := c.DownloadRange(ctx, "/dav/spaces/x/a.jpg", "bytes=0-3")
	if err != nil {
		t.Fatal(err)
	}
	rc2.Close()

	if len(auths) != 6 {
		t.Fatalf("peticiones: %d (%v)", len(auths), auths)
	}
	for _, a := range auths {
		if want := " Bearer oidc-token-123"; !strings.HasSuffix(a, want) {
			t.Fatalf("auth %q: quiero Bearer, no Basic", a)
		}
	}
}

// TestErrUnauthorized: un 401/403 de cualquier operación envuelve
// ErrUnauthorized (errors.Is) para que el scanner/worker aborten sin
// soft-delete (H8 §2). Un 404 de PROPFIND NO es ErrUnauthorized.
func TestErrUnauthorized(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		c := NewBearer(srv.URL, "caducado")
		ctx := context.Background()
		if _, err := c.MeID(ctx); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("MeID %d: %v", code, err)
		}
		if _, err := c.ListDrives(ctx); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("ListDrives %d: %v", code, err)
		}
		if _, err := c.Propfind(ctx, "/dav/x/", 1); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("Propfind %d: %v", code, err)
		}
		if _, err := c.GetRange(ctx, "/dav/x/a", 0, 4); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("GetRange %d: %v", code, err)
		}
		if _, _, err := c.Download(ctx, "/dav/x/a"); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("Download %d: %v", code, err)
		}
		if _, _, _, err := c.DownloadRange(ctx, "/dav/x/a", ""); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("DownloadRange %d: %v", code, err)
		}
		srv.Close()
	}

	// 404 en PROPFIND no es un problema de credenciales
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := New(srv.URL, "u", "t")
	if _, err := c.Propfind(context.Background(), "/dav/noexiste/", 1); err == nil || errors.Is(err, ErrUnauthorized) {
		t.Fatalf("404 no debe ser ErrUnauthorized: %v", err)
	}
}
