package webdav

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
