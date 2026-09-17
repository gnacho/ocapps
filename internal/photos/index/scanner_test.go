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

// multistatus genera un cuerpo PROPFIND con las entradas dadas ("dir/" para
// carpetas, "fichero" para fotos).
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
	write(self, true) // self (el scanner lo excluye)
	for _, e := range entries {
		write(e, strings.HasSuffix(e, "/"))
	}
	b.WriteString(`</d:multistatus>`)
	return b.String()
}

// davFake sirve PROPFINDs según el mapa path→(status, cuerpo); por defecto 404.
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

// TestScanAbortaSinSoftDeleteAnte401: con la credencial caducada el scan
// ABORTA propagando ErrUnauthorized y el índice queda INTACTO (H8 §4): sin
// este abort, el scan "completaría" con 0 vistos y soft-deletearía TODO.
func TestScanAbortaSinSoftDeleteAnte401(t *testing.T) {
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
		t.Fatalf("err=%v, quiero ErrUnauthorized", err)
	}
	// el índice sigue vivo
	stats, _ := st.Stats(ctx, "alice")
	if stats["assets"].(int64) != 1 {
		t.Fatalf("índice tocado tras 401: %v", stats)
	}
}

// TestScanGuardMitadFallos: si >50% de los PROPFINDs fallan (y hubo ≥10), el
// scan aborta SIN soft-delete (H8 §4).
func TestScanGuardMitadFallos(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	// asset previo cuyo etag NO aparecerá en el scan: sin el guard sería
	// soft-deleted
	if _, _, err := st.UpsertByETag(ctx, "alice", "/dav/spaces/x/Fotos/vieja.jpg", "e-old", "vieja.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}

	// raíz con 12 subcarpetas; 7 fallan con 500, 5 sirven una foto
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
	if err == nil || !strings.Contains(err.Error(), "PROPFINDs fallidos") {
		t.Fatalf("err=%v, quiero abort por >50%% fallos", err)
	}
	if errors.Is(err, webdav.ErrUnauthorized) {
		t.Fatal("un 500 no es ErrUnauthorized")
	}
	// el asset viejo NO se soft-deleted (índice intacto)
	stats, _ := st.Stats(ctx, "alice")
	if stats["assets"].(int64) != 6 { // 5 nuevas + la vieja viva
		t.Fatalf("índice tras guard: %v", stats)
	}
}

// TestScanUpsertsPorOwner: los upserts y el soft-delete van acotados al
// owner del scan (H8 §4).
func TestScanUpsertsPorOwner(t *testing.T) {
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
	// el mismo path existe una vez POR owner
	for _, owner := range []string{"alice", "bob"} {
		stats, _ := st.Stats(ctx, owner)
		if stats["assets"].(int64) != 1 {
			t.Fatalf("stats %s: %v", owner, stats)
		}
	}
	// un scan de carol (vacío) soft-deleted solo lo suyo (nada): alice y bob intactos
	if err := sc.ScanSpace(ctx, "carol", srv.URL+"/dav/spaces/y", "Fotos"); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		stats, _ := st.Stats(ctx, owner)
		if stats["assets"].(int64) != 1 {
			t.Fatalf("stats %s tras scan de carol: %v", owner, stats)
		}
	}
}
