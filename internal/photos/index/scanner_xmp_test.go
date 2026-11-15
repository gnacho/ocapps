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

// davFakeXMP serves the PROPFIND of a folder with the given entries and the
// GETs of the sidecars from the href->body map. It counts the GETs to check that
// the scanner does not download again a sidecar whose etag did not change.
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

// multistatusV is multistatus with a version suffix in the etag, so that the
// content of a sidecar can change without touching its href (as the real server
// would do when rewriting it).
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

// TestScanAppliesForeignSidecar: an .xmp written by another tool is projected
// onto the index on the next scan, without going through the API.
func TestScanAppliesForeignSidecar(t *testing.T) {
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
		t.Error("the favourite flag of the sidecar did not reach the index")
	}
	tags, err := st.AssetTags(ctx, "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tags, ",") != "mar,viaje" {
		t.Errorf("tags=%v, want [mar viaje]", tags)
	}

	// the photo without a sidecar is not touched
	b, err := st.AssetByPath(ctx, "alice", folder+"b.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if b.IsFavorite {
		t.Error("b.jpg has no sidecar and ended up as a favourite")
	}

	// second scan: same etag, not one extra download
	if err := sc.ScanSpace(ctx, "alice", srv.URL+"/dav/spaces/x", "Fotos"); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if n := atomic.LoadInt32(&gets); n != 1 {
		t.Errorf("sidecar GETs = %d, want 1 (the second scan must not download)", n)
	}
}

// TestScanSidecarWithoutRatingKeepsFavorite: a foreign sidecar that only brings
// tags cannot clear a local favourite. Only an explicit xmp:Rating decides, and
// an explicit 0 does clear it.
func TestScanSidecarWithoutRatingKeepsFavorite(t *testing.T) {
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
		t.Error("a sidecar without xmp:Rating cleared the local favourite")
	}
	tags, _ := st.AssetTags(ctx, "alice", id)
	if strings.Join(tags, ",") != "playa" {
		t.Errorf("tags=%v, want [playa]", tags)
	}

	// now an explicit rating 0: that one does unmark it
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
		t.Error("an explicit xmp:Rating 0 did not unmark the favourite")
	}
}
