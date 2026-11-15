package index

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/webdav"
	"github.com/gnacho/ocapps/internal/photos/store"
)

// multistatus builds a PROPFIND body with the given entries ("dir/" for
// folders, "file" for photos).
func multistatus(self string, entries ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">`)
	write := func(href string, dir bool) {
		b.WriteString("<d:response><d:href>" + href + "</d:href><d:propstat><d:prop><d:resourcetype>")
		if dir {
			b.WriteString("<d:collection/>")
		}
		b.WriteString(`</d:resourcetype><d:getetag>"etag-` + strings.TrimSuffix(href, "/") + `"</d:getetag>` +
			`<d:getlastmodified>Mon, 02 Jan 2006 15:04:05 GMT</d:getlastmodified>` +
			`<d:getcontentlength>100</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
	}
	write(self, true) // self (the scanner excludes it)
	for _, e := range entries {
		write(e, strings.HasSuffix(e, "/"))
	}
	b.WriteString(`</d:multistatus>`)
	return b.String()
}

// davFake serves PROPFINDs according to the path->(status, body) map, 404 by
// default.
func davFake(t *testing.T, handler func(path string) (int, string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			http.NotFound(w, r)
			return
		}
		code, body := handler(r.URL.Path)
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(code)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestScanner(t *testing.T, srv *httptest.Server, st *store.Store) *Scanner {
	t.Helper()
	return NewScanner(webdav.NewBearer(srv.URL, "tok"), webdav.DefaultOptions(), st, slog.Default())
}

// TestScanAbortsWithoutSoftDeleteOn401: with an expired credential the scan
// ABORTS propagating ErrUnauthorized and the index stays INTACT (H8 §4):
// without this abort, the scan would "complete" with 0 seen and soft-delete
// EVERYTHING.
func TestScanAbortsWithoutSoftDeleteOn401(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, _, err := st.UpsertByETag(ctx, "alice", "/dav/spaces/x/Fotos/a.jpg", "e1", "a.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}

	srv := davFake(t, func(path string) (int, string) { return http.StatusUnauthorized, "" })
	sc := newTestScanner(t, srv, st)
	err = sc.ScanSpace(ctx, "alice", srv.URL+"/dav/spaces/x", "Fotos")
	if !errors.Is(err, webdav.ErrUnauthorized) {
		t.Fatalf("err=%v, want ErrUnauthorized", err)
	}
	// the index is still alive
	stats, _ := st.Stats(ctx, "alice")
	if stats["assets"].(int64) != 1 {
		t.Fatalf("index touched after 401: %v", stats)
	}
}

// TestScanGuardHalfFailures: if more than 50% of the PROPFINDs fail (and there
// were at least 10), the scan aborts WITHOUT a soft-delete (H8 §4).
func TestScanGuardHalfFailures(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	// previous asset whose etag will NOT show up in the scan: without the
	// guard it would be soft-deleted
	if _, _, err := st.UpsertByETag(ctx, "alice", "/dav/spaces/x/Fotos/vieja.jpg", "e-old", "vieja.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}

	// root with 12 subfolders, 7 fail with 500, 5 serve one photo
	dirs := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		dirs = append(dirs, fmt.Sprintf("/dav/spaces/x/Fotos/d%02d/", i))
	}
	srv := davFake(t, func(path string) (int, string) {
		if path == "/dav/spaces/x/Fotos/" {
			return http.StatusMultiStatus, multistatus("/dav/spaces/x/Fotos/", dirs...)
		}
		for i, d := range dirs {
			if path == d {
				if i < 7 {
					return http.StatusInternalServerError, ""
				}
				return http.StatusMultiStatus, multistatus(d, d+"foto.jpg")
			}
		}
		return http.StatusNotFound, ""
	})
	sc := newTestScanner(t, srv, st)
	err = sc.ScanSpace(ctx, "alice", srv.URL+"/dav/spaces/x", "Fotos")
	if err == nil || !strings.Contains(err.Error(), "PROPFINDs failed") {
		t.Fatalf("err=%v, want an abort for >50%% failures", err)
	}
	if errors.Is(err, webdav.ErrUnauthorized) {
		t.Fatal("a 500 is not ErrUnauthorized")
	}
	// the old asset was NOT soft-deleted (index intact)
	stats, _ := st.Stats(ctx, "alice")
	if stats["assets"].(int64) != 6 { // 5 new plus the old one alive
		t.Fatalf("index after the guard: %v", stats)
	}
}

// TestScanUpsertsPerOwner: the upserts and the soft-delete are scoped to the
// owner of the scan (H8 §4).
func TestScanUpsertsPerOwner(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	srv := davFake(t, func(path string) (int, string) {
		if path == "/dav/spaces/x/Fotos/" {
			return http.StatusMultiStatus, multistatus("/dav/spaces/x/Fotos/", "/dav/spaces/x/Fotos/a.jpg")
		}
		return http.StatusNotFound, ""
	})
	sc := newTestScanner(t, srv, st)
	for _, owner := range []string{"alice", "bob"} {
		if err := sc.ScanSpace(ctx, owner, srv.URL+"/dav/spaces/x", "Fotos"); err != nil {
			t.Fatalf("scan %s: %v", owner, err)
		}
	}
	// the same path exists once PER owner
	for _, owner := range []string{"alice", "bob"} {
		stats, _ := st.Stats(ctx, owner)
		if stats["assets"].(int64) != 1 {
			t.Fatalf("stats %s: %v", owner, stats)
		}
	}
	// a scan of carol (empty) soft-deleted only her own (nothing): alice and bob intact
	if err := sc.ScanSpace(ctx, "carol", srv.URL+"/dav/spaces/y", "Fotos"); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		stats, _ := st.Stats(ctx, owner)
		if stats["assets"].(int64) != 1 {
			t.Fatalf("stats %s after carol's scan: %v", owner, stats)
		}
	}
}

func TestScanPrefix(t *testing.T) {
	cases := []struct{ root, want string }{
		{"Fotos", "/dav/spaces/x/Fotos/"},
		{"/Fotos/", "/dav/spaces/x/Fotos/"},
		{"", "/dav/spaces/x/"},
	}
	for _, c := range cases {
		if got := scanPrefix("https://drive.example.com/dav/spaces/x", c.root); got != c.want {
			t.Errorf("scanPrefix(root=%q): got %q, want %q", c.root, got, c.want)
		}
	}
	// A path outside the prefix does not fall within the scope of the scan.
	if strings.HasPrefix("/dav/spaces/x/gestion/a.jpg", scanPrefix("https://drive.example.com/dav/spaces/x", "Fotos")) {
		t.Error("a path of another folder falls within the scope of Fotos")
	}
}

// TestScanNotifiesChanges: the OnIndexChange hook is called when the scan
// changes the index (additions or removals) and NOT when there is nothing new.
// It is what triggers the notification to connected clients (SSE), so a false
// negative would leave open tabs without seeing the new photos.
func TestScanNotifiesChanges(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	srv := davFake(t, func(path string) (int, string) {
		if path == "/dav/spaces/x/Fotos/" {
			return http.StatusMultiStatus, multistatus("/dav/spaces/x/Fotos/", "/dav/spaces/x/Fotos/a.jpg")
		}
		return http.StatusNotFound, ""
	})

	notices := 0
	sc := newTestScanner(t, srv, st)
	sc.OnIndexChange = func(owner string) {
		if owner != "alice" {
			t.Errorf("owner of the notification = %q", owner)
		}
		notices++
	}

	if err := sc.ScanSpace(ctx, "alice", srv.URL+"/dav/spaces/x", "Fotos"); err != nil {
		t.Fatal(err)
	}
	if notices != 1 {
		t.Fatalf("notifications after the first scan = %d, want 1", notices)
	}

	// Second identical scan: nothing changes, there is no notification.
	if err := sc.ScanSpace(ctx, "alice", srv.URL+"/dav/spaces/x", "Fotos"); err != nil {
		t.Fatal(err)
	}
	if notices != 1 {
		t.Fatalf("notifications after a scan with no changes = %d, want 1", notices)
	}
}
