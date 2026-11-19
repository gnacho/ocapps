package index

import (
	"bytes"
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
		_, _ = fmt.Fprint(w, body)
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
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	if _, _, err := st.UpsertByETag(ctx, "alice", "/dav/spaces/x/Fotos/a.jpg", "e1", "a.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}

	srv := davFake(t, func(path string) (int, string) { return http.StatusUnauthorized, "" })
	sc := newTestScanner(t, srv, st)
	err = sc.ScanSpace(ctx, "alice", Space{ID: "x", Name: "x", WebDAVURL: srv.URL + "/dav/spaces/x"}, "Fotos")
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
	defer func() { _ = st.Close() }()
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
	err = sc.ScanSpace(ctx, "alice", Space{ID: "x", Name: "x", WebDAVURL: srv.URL + "/dav/spaces/x"}, "Fotos")
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
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	srv := davFake(t, func(path string) (int, string) {
		if path == "/dav/spaces/x/Fotos/" {
			return http.StatusMultiStatus, multistatus("/dav/spaces/x/Fotos/", "/dav/spaces/x/Fotos/a.jpg")
		}
		return http.StatusNotFound, ""
	})
	sc := newTestScanner(t, srv, st)
	for _, owner := range []string{"alice", "bob"} {
		if err := sc.ScanSpace(ctx, owner, Space{ID: "x", Name: "x", WebDAVURL: srv.URL + "/dav/spaces/x"}, "Fotos"); err != nil {
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
	if err := sc.ScanSpace(ctx, "carol", Space{ID: "y", Name: "y", WebDAVURL: srv.URL + "/dav/spaces/y"}, "Fotos"); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		stats, _ := st.Stats(ctx, owner)
		if stats["assets"].(int64) != 1 {
			t.Fatalf("stats %s after carol's scan: %v", owner, stats)
		}
	}
}

// TestResolveHref: the scan root is a path inside the space, and "/" (or "")
// means the root of the space, not the root of the server. Absolute hrefs
// returned by a listing of the same space are used as is.
func TestResolveHref(t *testing.T) {
	const url = "https://cloud.example.com/dav/spaces/x"
	cases := []struct{ root, want string }{
		{"", "/dav/spaces/x/"},
		{"/", "/dav/spaces/x/"},
		{"Fotos", "/dav/spaces/x/Fotos/"},
		{"/Fotos/", "/dav/spaces/x/Fotos/"},
		{"Fotos/2024", "/dav/spaces/x/Fotos/2024/"},
		{"/dav/spaces/x/Fotos/2024/", "/dav/spaces/x/Fotos/2024/"},
		{"/dav/spaces/x", "/dav/spaces/x"},
	}
	for _, c := range cases {
		got, err := resolveHref(url, c.root)
		if err != nil {
			t.Errorf("resolveHref(%q): %v", c.root, err)
			continue
		}
		if got != c.want {
			t.Errorf("resolveHref(%q) = %q, want %q", c.root, got, c.want)
		}
	}
	if _, err := resolveHref(url, "../otro"); err == nil {
		t.Error("a path traversal must be rejected")
	}
	// a space root never resolves to the server root
	if got, _ := resolveHref(url, "/"); got == "/" {
		t.Error("the space root resolved to the server root")
	}
}

func TestScanPrefix(t *testing.T) {
	cases := []struct{ root, want string }{
		{"Fotos", "/dav/spaces/x/Fotos/"},
		{"/Fotos/", "/dav/spaces/x/Fotos/"},
		{"/", "/dav/spaces/x/"},
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

	if err := sc.ScanSpace(ctx, "alice", Space{ID: "x", Name: "x", WebDAVURL: srv.URL + "/dav/spaces/x"}, "Fotos"); err != nil {
		t.Fatal(err)
	}
	if notices != 1 {
		t.Fatalf("notifications after the first scan = %d, want 1", notices)
	}

	// Second identical scan: nothing changes, there is no notification.
	if err := sc.ScanSpace(ctx, "alice", Space{ID: "x", Name: "x", WebDAVURL: srv.URL + "/dav/spaces/x"}, "Fotos"); err != nil {
		t.Fatal(err)
	}
	if notices != 1 {
		t.Fatalf("notifications after a scan with no changes = %d, want 1", notices)
	}
}

// TestScanTwoSpaces: the scanner walks the personal space AND a project space
// (issue #145): each asset keeps the space it was found in, so the UI can tell
// them apart and filter by it.
func TestScanTwoSpaces(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	srv := davFake(t, func(path string) (int, string) {
		switch path {
		case "/dav/spaces/p/Fotos/":
			return http.StatusMultiStatus, multistatus("/dav/spaces/p/Fotos/", "/dav/spaces/p/Fotos/a.jpg")
		case "/dav/spaces/k/Fotos/":
			return http.StatusMultiStatus, multistatus("/dav/spaces/k/Fotos/", "/dav/spaces/k/Fotos/b.jpg")
		}
		return http.StatusNotFound, ""
	})
	sc := newTestScanner(t, srv, st)
	spaces := []Space{
		{ID: "p1", Name: "Personal", Type: "personal", WebDAVURL: srv.URL + "/dav/spaces/p"},
		{ID: "k1", Name: "Familia", Type: "project", WebDAVURL: srv.URL + "/dav/spaces/k"},
	}
	for _, sp := range spaces {
		if err := sc.ScanSpace(ctx, "alice", sp, "Fotos"); err != nil {
			t.Fatalf("scan %s: %v", sp.Name, err)
		}
	}

	all, err := st.AssetsPage(ctx, "alice", 1<<62-1, 1<<62-1, 10, false, false, "", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("assets: %d %v", len(all), err)
	}
	bySpace := map[string]string{}
	for _, a := range all {
		bySpace[a.SpaceID] = a.SpaceName
	}
	if bySpace["p1"] != "Personal" || bySpace["k1"] != "Familia" {
		t.Errorf("spaces stored = %v", bySpace)
	}
	listed, err := st.Spaces(ctx, "alice")
	if err != nil || len(listed) != 2 {
		t.Fatalf("spaces: %+v %v", listed, err)
	}
}

// TestScanSkipsSpaceWithoutRoot: a space that does not have the configured root
// folder (a project space without "Fotos", say) is skipped. Its already indexed
// assets are NOT soft-deleted: the scan of that space did not happen, so it
// cannot conclude that they disappeared.
func TestScanSkipsSpaceWithoutRoot(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	// an asset of the project space indexed by an earlier scan
	if _, _, err := st.UpsertInSpace(ctx, "alice", "/dav/spaces/k/Fotos/vieja.jpg", "e1", "vieja.jpg", "image", time.Now(), 100,
		store.Space{ID: "k1", Name: "Familia", Type: "project"}); err != nil {
		t.Fatal(err)
	}

	srv := davFake(t, func(path string) (int, string) { return http.StatusNotFound, "" })
	sc := newTestScanner(t, srv, st)
	space := Space{ID: "k1", Name: "Familia", Type: "project", WebDAVURL: srv.URL + "/dav/spaces/k"}
	if err := sc.ScanSpace(ctx, "alice", space, "Fotos"); err != nil {
		t.Fatalf("scan of a space without root must not fail: %v", err)
	}
	stats, err := st.Stats(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if stats["assets"].(int64) != 1 {
		t.Fatalf("the existing asset was touched: %v", stats)
	}
}

// TestScanWholeSpace: with the root set to "/" the scan walks the whole space,
// so a library spread over several top folders (gestion/, publico/) is indexed
// without picking one of them (issue #145: with a single global root folder the
// spaces that do not have it were skipped).
func TestScanWholeSpace(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	srv := davFake(t, func(path string) (int, string) {
		switch path {
		case "/dav/spaces/p/":
			return http.StatusMultiStatus, multistatus("/dav/spaces/p/", "/dav/spaces/p/gestion/", "/dav/spaces/p/publico/")
		case "/dav/spaces/p/gestion/":
			return http.StatusMultiStatus, multistatus("/dav/spaces/p/gestion/", "/dav/spaces/p/gestion/a.jpg")
		case "/dav/spaces/p/publico/":
			return http.StatusMultiStatus, multistatus("/dav/spaces/p/publico/", "/dav/spaces/p/publico/b.jpg")
		}
		return http.StatusNotFound, ""
	})
	sc := newTestScanner(t, srv, st)
	space := Space{ID: "p1", Name: "Personal", Type: "personal", WebDAVURL: srv.URL + "/dav/spaces/p"}
	if err := sc.ScanSpace(ctx, "alice", space, "/"); err != nil {
		t.Fatalf("scan: %v", err)
	}

	all, err := st.AssetsPage(ctx, "alice", 1<<62-1, 1<<62-1, 10, false, false, "", "")
	if err != nil || len(all) != 2 {
		t.Fatalf("assets = %d, err=%v", len(all), err)
	}
	spaces, _ := st.Spaces(ctx, "alice")
	if len(spaces) != 1 || spaces[0].Assets != 2 {
		t.Errorf("spaces = %+v", spaces)
	}
}

// TestScanLogsItsCost: a scan that takes hours must say where the time went
// (issue #71). The progress line carries the assets, the folders and the time
// spent in the DAV listings and in the index writes.
func TestScanLogsItsCost(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	// 1000 photos in one folder: enough for the progress line
	entries := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		entries = append(entries, fmt.Sprintf("/dav/spaces/x/Fotos/%04d.jpg", i))
	}
	srv := davFake(t, func(path string) (int, string) {
		if path == "/dav/spaces/x/Fotos/" {
			return http.StatusMultiStatus, multistatus("/dav/spaces/x/Fotos/", entries...)
		}
		return http.StatusNotFound, ""
	})

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	sc := NewScanner(webdav.NewBearer(srv.URL, "tok"), webdav.DefaultOptions(), st, logger)
	space := Space{ID: "x", Name: "x", WebDAVURL: srv.URL + "/dav/spaces/x"}
	if err := sc.ScanSpace(ctx, "alice", space, "Fotos"); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if !strings.Contains(out, "scan progress") || !strings.Contains(out, "assets=1000") {
		t.Errorf("no progress line: %s", out)
	}
	if !strings.Contains(out, "propfind=") || !strings.Contains(out, "upsert=") {
		t.Errorf("the completion line does not split the cost: %s", out)
	}
}

// TestScanDoesNotRevisitAFolderWithEscapedName: the self entry of a PROPFIND
// comes percent-encoded; if the client does not recognise it, the scanner
// queues the same folder again and walks the tree in a loop until the timeout
// (issue #72: 2 hours of scan and one core of OpenCloud for an 85 second walk).
func TestScanDoesNotRevisitAFolderWithEscapedName(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	folder := "/dav/spaces/x/Dise%C3%B1o/"
	var listings int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" || r.URL.Path != "/dav/spaces/x/Diseño/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		if r.Header.Get("Depth") == "0" { // root probe
			_, _ = fmt.Fprint(w, multistatus(folder))
			return
		}
		listings++
		_, _ = fmt.Fprint(w, multistatus(folder, folder+"logo.png"))
	}))
	t.Cleanup(srv.Close)

	sc := NewScanner(webdav.NewBearer(srv.URL, "tok"), webdav.DefaultOptions(), st, slog.Default())
	space := Space{ID: "x", Name: "x", WebDAVURL: srv.URL + "/dav/spaces/x"}
	if err := sc.ScanSpace(ctx, "alice", space, "Diseño"); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if listings != 1 {
		t.Errorf("the folder was listed %d times, want 1", listings)
	}
	stats, _ := st.Stats(ctx, "alice")
	if stats["assets"].(int64) != 1 {
		t.Errorf("assets = %v, want 1", stats["assets"])
	}
}
