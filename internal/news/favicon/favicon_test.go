package favicon

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testCache returns a cache over a temporary directory with a logger that
// writes nowhere, and the directory itself.
func testCache(t *testing.T) (*Cache, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := NewCache(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	return c, dir
}

func TestHash(t *testing.T) {
	// md5 of the URL, the identifier the API v1.3 endpoint uses.
	if got, want := Hash("https://example.com/feed"), "ca7a0f39fd6a95f6ea1b35f37579f6ef"; got != want {
		t.Fatalf("Hash = %q, want %q", got, want)
	}
	if Hash("a") == Hash("b") {
		t.Fatal("Hash of different URLs must differ")
	}
}

func TestNewCacheCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "favicons")
	c, err := NewCache(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	if c.dir != dir {
		t.Fatalf("cache dir = %q, want %q", c.dir, dir)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat cache dir: %v", err)
	}
	if !st.IsDir() {
		t.Fatal("cache dir is not a directory")
	}
	// 0700: only the service user may read the icons.
	if perm := st.Mode().Perm(); perm != 0o700 {
		t.Fatalf("cache dir permissions = %o, want 700", perm)
	}
}

func TestNewCacheFailsOnUnusablePath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("prepare file: %v", err)
	}
	if _, err := NewCache(filepath.Join(file, "sub"), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		return // expected: the parent is a file
	}
	t.Fatal("NewCache must fail when the directory cannot be created")
}

func TestFaviconURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"empty", "", "", false},
		{"no scheme", "example.com", "", false},
		{"ftp", "ftp://example.com/feed", "", false},
		{"no host", "https:///feed", "", false},
		{"http", "http://example.com/section/feed.xml", "http://example.com/favicon.ico", true},
		{"https with port", "https://example.com:8443/x", "https://example.com:8443/favicon.ico", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := faviconURL(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("faviconURL(%q) = %q, %v, want %q, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// ext is called with an already lowercased Content-Type (see Fetch).
func TestExt(t *testing.T) {
	cases := map[string]string{
		"image/svg+xml":  ".svg",
		"image/png":      ".png",
		"image/jpeg":     ".jpg",
		"image/jpg":      ".jpg",
		"image/gif":      ".gif",
		"image/webp":     ".webp",
		"image/x-icon":   ".ico",
		"text/html":      ".ico",
		"application/oc": ".ico",
	}
	for ctype, want := range cases {
		if got := ext(ctype); got != want {
			t.Fatalf("ext(%q) = %q, want %q", ctype, got, want)
		}
	}
}

func TestContentType(t *testing.T) {
	cases := map[string]string{
		".svg":  "image/svg+xml",
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".gif":  "image/gif",
		".webp": "image/webp",
		".ico":  "image/x-icon",
		".zzz":  "image/x-icon",
	}
	for extn, want := range cases {
		if got := contentType(extn); got != want {
			t.Fatalf("contentType(%q) = %q, want %q", extn, got, want)
		}
	}
}

// fetchServer returns a server that answers /favicon.ico with the body, the
// content type and the status code given, and records the hits.
func fetchServer(t *testing.T, status int, ctype string, body []byte) (*httptest.Server, *int, *http.Request) {
	t.Helper()
	var hits int
	var last http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		last = *r
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &last
}

func TestFetchStoresIcon(t *testing.T) {
	c, _ := testCache(t)
	icon := []byte("\x89PNG\r\n\x1a\nfake")
	srv, hits, last := fetchServer(t, http.StatusOK, "image/png", icon)
	c.client = srv.Client()

	c.Fetch(context.Background(), "https://example.com/feed", srv.URL+"/blog")

	if *hits != 1 {
		t.Fatalf("requests = %d, want 1", *hits)
	}
	if got := last.URL.Path; got != "/favicon.ico" {
		t.Fatalf("requested path = %q, want /favicon.ico", got)
	}
	if ua := last.Header.Get("User-Agent"); ua != userAgent {
		t.Fatalf("User-Agent = %q, want %q", ua, userAgent)
	}
	cached := c.pathFor(Hash("https://example.com/feed") + ".png")
	got, err := os.ReadFile(cached)
	if err != nil {
		t.Fatalf("read cached icon: %v", err)
	}
	if !bytes.Equal(got, icon) {
		t.Fatalf("cached icon = %q, want %q", got, icon)
	}
	if !c.Has(Hash("https://example.com/feed")) {
		t.Fatal("Has must report the cached icon")
	}
}

func TestFetchIgnoresWhatCannotBeAnIcon(t *testing.T) {
	cases := []struct {
		name   string
		status int
		ctype  string
		body   []byte
	}{
		{"error status", http.StatusNotFound, "image/png", []byte("x")},
		{"html error page", http.StatusOK, "text/html", []byte("<html>nope</html>")},
		{"no content type", http.StatusOK, "", []byte("x")},
		{"empty body", http.StatusOK, "image/png", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := testCache(t)
			srv, hits, _ := fetchServer(t, tc.status, tc.ctype, tc.body)
			c.client = srv.Client()

			c.Fetch(context.Background(), "https://example.com/feed", srv.URL+"/blog")

			if *hits != 1 {
				t.Fatalf("requests = %d, want 1", *hits)
			}
			if c.Has(Hash("https://example.com/feed")) {
				t.Fatal("nothing must be cached")
			}
		})
	}
}

func TestFetchSkipsUnusableSiteLink(t *testing.T) {
	c, _ := testCache(t)
	srv, hits, _ := fetchServer(t, http.StatusOK, "image/png", []byte("x"))
	c.client = srv.Client()

	for _, link := range []string{"", "ftp://example.com/feed", "not a url", "https://"} {
		c.Fetch(context.Background(), "https://example.com/feed", link)
	}
	if *hits != 0 {
		t.Fatalf("requests = %d, want 0", *hits)
	}
}

func TestFetchStopsOnCanceledContext(t *testing.T) {
	c, _ := testCache(t)
	srv, _, _ := fetchServer(t, http.StatusOK, "image/png", []byte("x"))
	c.client = srv.Client()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Fetch(ctx, "https://example.com/feed", srv.URL+"/blog")

	if c.Has(Hash("https://example.com/feed")) {
		t.Fatal("nothing must be cached when the request is canceled")
	}
}

func TestHasChecksEveryExtension(t *testing.T) {
	c, _ := testCache(t)
	const hash = "abc123"
	if c.Has(hash) {
		t.Fatal("empty cache must not report a favicon")
	}
	if err := os.WriteFile(c.pathFor(hash+".webp"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write icon: %v", err)
	}
	if !c.Has(hash) {
		t.Fatal("Has must find the icon whatever its extension is")
	}
	if c.Has("other-hash") {
		t.Fatal("Has must not report an icon of another feed")
	}
}

func TestServe(t *testing.T) {
	c, _ := testCache(t)
	const hash = "deadbeef"
	icon := []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")
	if err := os.WriteFile(c.pathFor(hash+".svg"), icon, 0o600); err != nil {
		t.Fatalf("write icon: %v", err)
	}

	rec := httptest.NewRecorder()
	c.Serve(rec, hash)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("Content-Type = %q, want image/svg+xml", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=86400") {
		t.Fatalf("Cache-Control = %q, want a one day max-age", cc)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, icon) {
		t.Fatalf("body = %q, want %q", got, icon)
	}
}

func TestServeMissingIcon(t *testing.T) {
	c, _ := testCache(t)
	rec := httptest.NewRecorder()
	c.Serve(rec, "nothing-here")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestServeFallsBackToICOContentType(t *testing.T) {
	c, _ := testCache(t)
	if err := os.WriteFile(c.pathFor("ico-only.ico"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write icon: %v", err)
	}
	rec := httptest.NewRecorder()
	c.Serve(rec, "ico-only")
	if ct := rec.Header().Get("Content-Type"); ct != "image/x-icon" {
		t.Fatalf("Content-Type = %q, want image/x-icon", ct)
	}
}
