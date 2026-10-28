package index

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/photos/store"
)

// davFakeXMP sirve el PROPFIND de una carpeta con las entradas dadas y los GET
// de los sidecars del mapa href→cuerpo. Cuenta los GET para comprobar que el
// scanner no vuelve a descargar un sidecar cuyo etag no cambió.
func davFakeXMP(t *testing.T, folder, version string, entries []string, sidecars map[string]string, gets *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PROPFIND":
			if r.URL.Path != folder {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprint(w, multistatusV(folder, version, entries...))
		case "GET":
			body, ok := sidecars[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			atomic.AddInt32(gets, 1)
			w.Header().Set("Content-Type", "application/rdf+xml")
			_, _ = fmt.Fprint(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// multistatusV es multistatus con un sufijo de versión en el etag, para poder
// cambiar el contenido de un sidecar sin tocar su href (como haría el
// servidor real al reescribirlo).
func multistatusV(self, version string, entries ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">`)
	write := func(href string, dir bool) {
		b.WriteString("<d:response><d:href>" + href + "</d:href><d:propstat><d:prop><d:resourcetype>")
		if dir {
			b.WriteString("<d:collection/>")
		}
		b.WriteString(`</d:resourcetype><d:getetag>"etag-` + strings.TrimSuffix(href, "/") + "-" + version + `"</d:getetag>` +
			`<d:getlastmodified>Mon, 02 Jan 2006 15:04:05 GMT</d:getlastmodified>` +
			`<d:getcontentlength>100</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`)
	}
	write(self, true)
	for _, e := range entries {
		write(e, strings.HasSuffix(e, "/"))
	}
	b.WriteString(`</d:multistatus>`)
	return b.String()
}

const sidecarFavorite = `<?xpacket begin="" id="W5M0MpCehiHzreSzNTczkc9d"?>
<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="Lightroom">
 <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
  <rdf:Description rdf:about="" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:xmp="http://ns.adobe.com/xap/1.0/">
   <dc:subject><rdf:Bag><rdf:li>viaje</rdf:li><rdf:li>mar</rdf:li></rdf:Bag></dc:subject>
   <xmp:Rating>5</xmp:Rating>
  </rdf:Description>
 </rdf:RDF>
</x:xmpmeta>`

// TestScanAplicaSidecarAjeno: un .xmp escrito por otra herramienta se proyecta
// sobre el índice en el siguiente scan, sin pasar por la API.
func TestScanAplicaSidecarAjeno(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	const folder = "/dav/spaces/x/Fotos/"
	var gets int32
	srv := davFakeXMP(t, folder, "v1",
		[]string{folder + "a.jpg", folder + "a.jpg.xmp", folder + "b.jpg"},
		map[string]string{folder + "a.jpg.xmp": sidecarFavorite}, &gets)

	sc := newTestScanner(t, srv, st)
	if err := sc.ScanSpace(ctx, "alice", srv.URL+"/dav/spaces/x", "Fotos"); err != nil {
		t.Fatalf("scan: %v", err)
	}

	a, err := st.AssetByPath(ctx, "alice", folder+"a.jpg")
	if err != nil {
		t.Fatalf("asset a: %v", err)
	}
	if !a.IsFavorite {
		t.Error("el favorito del sidecar no llegó al índice")
	}
	tags, err := st.AssetTags(ctx, "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tags, ",") != "mar,viaje" {
		t.Errorf("tags=%v, quiero [mar viaje]", tags)
	}

	// la foto sin sidecar no se toca
	b, err := st.AssetByPath(ctx, "alice", folder+"b.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if b.IsFavorite {
		t.Error("b.jpg no tiene sidecar y quedó favorita")
	}

	// segundo scan: mismo etag, ni una descarga más
	if err := sc.ScanSpace(ctx, "alice", srv.URL+"/dav/spaces/x", "Fotos"); err != nil {
		t.Fatalf("segundo scan: %v", err)
	}
	if n := atomic.LoadInt32(&gets); n != 1 {
		t.Errorf("GETs de sidecar = %d, quiero 1 (el segundo scan no debe descargar)", n)
	}
}

// TestScanSidecarSinRatingNoBorraFavorito: un sidecar ajeno que solo trae
// etiquetas no puede vaciar un favorito local. Solo un xmp:Rating explícito
// decide, y un 0 explícito sí borra.
func TestScanSidecarSinRatingNoBorraFavorito(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	const folder = "/dav/spaces/x/Fotos/"

	id, _, err := st.UpsertByETag(ctx, "alice", folder+"a.jpg", "e1", "a.jpg", "image", time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetFavorite(ctx, "alice", id, true); err != nil {
		t.Fatal(err)
	}

	noRating := `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
 <rdf:Description rdf:about="" xmlns:dc="http://purl.org/dc/elements/1.1/">
  <dc:subject><rdf:Bag><rdf:li>playa</rdf:li></rdf:Bag></dc:subject>
 </rdf:Description></rdf:RDF></x:xmpmeta>`

	var gets int32
	srv := davFakeXMP(t, folder, "v1",
		[]string{folder + "a.jpg", folder + "a.jpg.xmp"},
		map[string]string{folder + "a.jpg.xmp": noRating}, &gets)
	sc := newTestScanner(t, srv, st)
	if err := sc.ScanSpace(ctx, "alice", srv.URL+"/dav/spaces/x", "Fotos"); err != nil {
		t.Fatalf("scan: %v", err)
	}

	a, err := st.AssetByID(ctx, "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if !a.IsFavorite {
		t.Error("un sidecar sin xmp:Rating borró el favorito local")
	}
	tags, _ := st.AssetTags(ctx, "alice", id)
	if strings.Join(tags, ",") != "playa" {
		t.Errorf("tags=%v, quiero [playa]", tags)
	}

	// ahora un rating 0 explícito: eso sí desmarca
	zeroRating := strings.Replace(noRating, `xmlns:dc="http://purl.org/dc/elements/1.1/"`,
		`xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:xmp="http://ns.adobe.com/xap/1.0/"`, 1)
	zeroRating = strings.Replace(zeroRating, `</rdf:Description>`, `<xmp:Rating>0</xmp:Rating></rdf:Description>`, 1)
	srv2 := davFakeXMP(t, folder, "v2",
		[]string{folder + "a.jpg", folder + "a.jpg.xmp"},
		map[string]string{folder + "a.jpg.xmp": zeroRating}, &gets)
	if err := newTestScanner(t, srv2, st).ScanSpace(ctx, "alice", srv2.URL+"/dav/spaces/x", "Fotos"); err != nil {
		t.Fatalf("scan: %v", err)
	}
	a, err = st.AssetByID(ctx, "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if a.IsFavorite {
		t.Error("xmp:Rating 0 explícito no desmarcó el favorito")
	}
}
