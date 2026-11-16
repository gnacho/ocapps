package imgproxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// originServer starts a fake origin and returns it with the number of GET
// requests it received.
func originServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *int32) {
	t.Helper()
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			atomic.AddInt32(&gets, 1)
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &gets
}

// proxyRequest sends a signed request for rawURL through a front server
// wired to p, with the extra headers given.
func proxyRequest(t *testing.T, p *Proxy, rawURL string, headers map[string]string) *http.Response {
	t.Helper()
	front := httptest.NewServer(http.HandlerFunc(p.Serve))
	t.Cleanup(front.Close)

	req, err := http.NewRequest(http.MethodGet, front.URL+"?u="+url.QueryEscape(rawURL)+"&t="+p.Sign(rawURL), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestServeStreamsMedia(t *testing.T) {
	const media = "0123456789abcdef"
	origin, gets := originServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", "16")
		if r.Header.Get("Range") == "bytes=4-7" {
			w.Header().Set("Content-Range", "bytes 4-7/16")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte(media[4:8]))
			return
		}
		_, _ = w.Write([]byte(media))
	})
	p := newTestProxy(t)

	resp := proxyRequest(t, p, origin.URL+"/episode.mp3", map[string]string{"Range": "bytes=4-7"})
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want the 206 of the origin", resp.StatusCode)
	}
	if got := string(body); got != media[4:8] {
		t.Fatalf("body = %q, want the requested range", got)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if ar := resp.Header.Get("Accept-Ranges"); ar != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes for seeking", ar)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 4-7/16" {
		t.Fatalf("Content-Range = %q", cr)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "max-age=86400") {
		t.Fatalf("Cache-Control = %q, want a one day max-age", cc)
	}
	if n := atomic.LoadInt32(gets); n != 1 {
		t.Fatalf("origin GETs = %d, want 1 (media is streamed, not buffered twice)", n)
	}
}

func TestServeStreamsVideoWithoutRange(t *testing.T) {
	origin, _ := originServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("mp4-bytes"))
	})
	p := newTestProxy(t)

	resp := proxyRequest(t, p, origin.URL+"/clip.mp4", nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := string(body); got != "mp4-bytes" {
		t.Fatalf("body = %q", got)
	}
	if cl := resp.Header.Get("Content-Length"); cl == "" {
		t.Fatal("Content-Length must be forwarded")
	}
}

func TestServeMediaFailsWhenTheOriginFails(t *testing.T) {
	p := newTestProxy(t)
	// A dead origin: the probe fails before any streaming.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rawURL := srv.URL + "/gone.mp3"
	srv.Close()

	resp := proxyRequest(t, p, rawURL, nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 when the origin is unreachable", resp.StatusCode)
	}
}

func TestProbeFallsBackToARangeGet(t *testing.T) {
	var heads, gets int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			atomic.AddInt32(&heads, 1)
			w.WriteHeader(http.StatusMethodNotAllowed)
		case http.MethodGet:
			atomic.AddInt32(&gets, 1)
			if r.Header.Get("Range") != "bytes=0-0" {
				t.Errorf("Range = %q, want the minimal probe range", r.Header.Get("Range"))
			}
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("x"))
		}
	}))
	t.Cleanup(origin.Close)

	p := newTestProxy(t)
	ct, ok := p.probeContentType(context.Background(), origin.URL+"/a.png")
	if !ok || ct != "image/png" {
		t.Fatalf("probe = %q, %v, want image/png from the GET fallback", ct, ok)
	}
	if atomic.LoadInt32(&heads) != 1 || atomic.LoadInt32(&gets) != 1 {
		t.Fatalf("heads = %d, gets = %d, want one of each", heads, gets)
	}
}

func TestProbeRejectsUnusableOrigins(t *testing.T) {
	cases := []struct {
		name   string
		server http.HandlerFunc
	}{
		{"unreachable", nil},
		{"not found", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}},
		{"unusable method", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestProxy(t)
			var rawURL string
			if tc.server == nil {
				srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				rawURL = srv.URL + "/x.jpg"
				srv.Close()
			} else {
				origin, _ := originServer(t, tc.server)
				rawURL = origin.URL + "/x.jpg"
			}
			if _, ok := p.probeContentType(context.Background(), rawURL); ok {
				t.Fatal("the probe must reject an origin that cannot be served")
			}
		})
	}
}

func TestProbeRetriesAnOctetStream(t *testing.T) {
	// An origin that answers HEAD with octet-stream and then GET with the
	// real type: the first answer is not trusted, the second one classifies.
	var gets int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			return
		}
		atomic.AddInt32(&gets, 1)
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("mp4"))
	}))
	t.Cleanup(origin.Close)

	p := newTestProxy(t)
	ct, ok := p.probeContentType(context.Background(), origin.URL+"/v.mp4")
	if !ok || ct != "video/mp4" {
		t.Fatalf("probe = %q, %v, want the media type of the second answer", ct, ok)
	}

	resp := proxyRequest(t, p, origin.URL+"/v.mp4", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("Content-Type = %q, want the origin media type", ct)
	}
	if atomic.LoadInt32(&gets) == 0 {
		t.Fatal("the media download must happen")
	}
}

func TestServeImageErrors(t *testing.T) {
	t.Run("missing at the origin", func(t *testing.T) {
		origin, _ := originServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Type", "image/png")
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		})
		resp := proxyRequest(t, newTestProxy(t), origin.URL+"/a.png", nil)
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
	})

	t.Run("not an image after all", func(t *testing.T) {
		origin, _ := originServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Type", "image/png")
				w.WriteHeader(http.StatusOK)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>"))
		})
		resp := proxyRequest(t, newTestProxy(t), origin.URL+"/a.png", nil)
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d, want 415 when the body is not an image", resp.StatusCode)
		}
	})

	t.Run("empty body", func(t *testing.T) {
		origin, _ := originServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
		})
		resp := proxyRequest(t, newTestProxy(t), origin.URL+"/a.png", nil)
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 for an empty body", resp.StatusCode)
		}
	})
}

func TestServeImageRefetchesWhenTheCacheIsIncomplete(t *testing.T) {
	img := []byte("\x89PNG\r\n\x1a\nfake")
	origin, gets := originServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(img)
	})
	p := newTestProxy(t)
	rawURL := origin.URL + "/a.png"

	if resp := proxyRequest(t, p, rawURL, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("first status = %d", resp.StatusCode)
	}
	base := filepath.Join(p.dir, cachePath(rawURL))
	if _, err := os.Stat(base + ".ct"); err != nil {
		t.Fatalf("the content type must be cached: %v", err)
	}
	// Remove only the image: the metadata alone is not a cache hit.
	if err := os.Remove(base); err != nil {
		t.Fatalf("remove cached image: %v", err)
	}
	before := atomic.LoadInt32(gets)
	if resp := proxyRequest(t, p, rawURL, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d", resp.StatusCode)
	}
	if after := atomic.LoadInt32(gets); after != before+1 {
		t.Fatalf("origin GETs = %d, want one more download after the cache loss", after)
	}
}

func TestNewFailsOnAnUnusableDataDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("prepare file: %v", err)
	}
	if _, err := New(filepath.Join(file, "data"), slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("New must fail when the data directory cannot be created")
	}
}

func TestServeMediaFailsOnANonOKDownload(t *testing.T) {
	origin, _ := originServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "video/mp4")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	resp := proxyRequest(t, newTestProxy(t), origin.URL+"/v.mp4", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 when the media download fails", resp.StatusCode)
	}
}
