package feed

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/news/store"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

func TestParseRSS(t *testing.T) {
	f, items, err := Parse(loadFixture(t, "rss.xml"))
	if err != nil {
		t.Fatalf("parse rss: %v", err)
	}
	if f.Title != "Example Blog" || f.Link != "https://ejemplo.example" {
		t.Errorf("bad feed meta: %+v", f)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, I have %d", len(items))
	}

	first := items[0]
	if first.GUID != "urn:ejemplo:1" || first.GUIDHash == "" {
		t.Errorf("bad guid: %+v", first)
	}
	if first.Title != "First entry" {
		t.Errorf("title: %q", first.Title)
	}
	if first.Author != "ana@example.com (Ana Test)" && first.Author == "" {
		t.Errorf("unexpected empty author: %q", first.Author)
	}
	if first.URL != "https://ejemplo.example/1" {
		t.Errorf("url: %q", first.URL)
	}
	// content:encoded wins over description
	wantBody := "<p>Full <b>body</b> of the first</p>"
	if first.Body != wantBody {
		t.Errorf("body: %q (expected content:encoded)", first.Body)
	}
	if first.EnclosureLink == nil || *first.EnclosureLink != "https://ejemplo.example/audio/1.mp3" {
		t.Errorf("enclosure: %+v", first.EnclosureLink)
	}
	if first.EnclosureMime == nil || *first.EnclosureMime != "audio/mpeg" {
		t.Errorf("enclosure mime: %+v", first.EnclosureMime)
	}
	if first.MediaThumbnail == nil || *first.MediaThumbnail != "https://ejemplo.example/img/1.jpg" {
		t.Errorf("media:thumbnail: %+v", first.MediaThumbnail)
	}
	// Mon, 02 Jan 2006 15:04:05 -0700 = 1136214245
	if first.PubDate != time.Date(2006, 1, 2, 15, 4, 5, 0, time.FixedZone("MST", -7*3600)).Unix() {
		t.Errorf("pubDate: %d", first.PubDate)
	}

	second := items[1]
	if second.EnclosureLink != nil || second.MediaThumbnail != nil {
		t.Errorf("item without enclosure must not have them: %+v", second)
	}
	if second.Body != "Summary of the second" {
		t.Errorf("body fallback description: %q", second.Body)
	}
	if first.Fingerprint == second.Fingerprint || first.Fingerprint == "" {
		t.Errorf("fingerprint must differ and be non empty")
	}
}

func TestParseAtom(t *testing.T) {
	f, items, err := Parse(loadFixture(t, "atom.xml"))
	if err != nil {
		t.Fatalf("parse atom: %v", err)
	}
	if f.Title != "Atom Example" || f.Link != "https://atom.example/" {
		t.Errorf("bad feed meta: %+v", f)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, I have %d", len(items))
	}
	it := items[0]
	if it.GUID != "urn:uuid:1225c695-cfb8-4ebb-aaaa-80da344efa6a" {
		t.Errorf("atom id as guid: %q", it.GUID)
	}
	if it.Author != "Luis Atom" {
		t.Errorf("author: %q", it.Author)
	}
	if it.Body != "<p><em>Atom</em> content</p>" {
		t.Errorf("body: %q", it.Body)
	}
	if it.PubDate != time.Date(2003, 12, 13, 18, 30, 2, 0, time.UTC).Unix() {
		t.Errorf("pubDate: %d", it.PubDate)
	}
}

func TestParseInvalid(t *testing.T) {
	if _, _, err := Parse([]byte("this is not xml")); err == nil {
		t.Fatal("expected an error with garbage")
	}
}

// dedupe: same guid -> same hash (uniqueness by feed_id+guid_hash in the DB).
func TestGUIDHashStable(t *testing.T) {
	_, items, err := Parse(loadFixture(t, "rss.xml"))
	if err != nil {
		t.Fatal(err)
	}
	_, again, _ := Parse(loadFixture(t, "rss.xml"))
	if items[0].GUIDHash != again[0].GUIDHash {
		t.Error("guid_hash must be deterministic")
	}
}

func TestHasFullContent(t *testing.T) {
	summary := "<p>" + strings.Repeat("a", 300) + "</p>"
	full := "<p>" + strings.Repeat("a", 1200) + "</p>"
	if HasFullContent([]store.NewItem{{Body: summary}}) {
		t.Error("a short summary must not flag full")
	}
	if !HasFullContent([]store.NewItem{{Body: summary}, {Body: full}}) {
		t.Error("one long item is enough to flag full")
	}
	if HasFullContent(nil) {
		t.Error("without items it does not flag full")
	}
}

// TestDiscover: autodiscovery of RSS/Atom feeds on an HTML page.
func TestDiscover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rss.xml" {
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = w.Write(loadFixture(t, "rss.xml"))
			return
		}
		html := `<!doctype html><html><head>
			<link rel="alternate" type="application/rss+xml" title="Blog RSS" href="/rss.xml">
			<link rel="alternate" type="application/atom+xml" href="/atom.xml">
			<link rel="icon" href="/favicon.ico">
			</head><body>hello</body></html>`
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	}))
	defer srv.Close()

	h := NewHTTPFetcherAllowLocal(5 * time.Second)
	feeds, err := h.Discover(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(feeds) != 2 {
		t.Fatalf("expected 2 feeds, I have %d: %+v", len(feeds), feeds)
	}
	var kinds []string
	for _, f := range feeds {
		kinds = append(kinds, f.Type)
	}
	if !containsStr(kinds, "rss") || !containsStr(kinds, "atom") {
		t.Fatalf("types: %v", kinds)
	}
	// absolute url resolved against the base
	found := false
	for _, f := range feeds {
		if strings.HasSuffix(f.URL, "/rss.xml") {
			found = true
		}
	}
	if !found {
		t.Fatalf("it did not resolve the relative rss.xml: %+v", feeds)
	}
}

func containsStr(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// TestFetchBasicAuth: the fetcher sends Basic Authorization when the feed
// requires it, and without credentials (or wrong ones) it returns ErrAuthRequired.
func TestFetchBasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "reader" || p != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write(loadFixture(t, "rss.xml"))
	}))
	defer srv.Close()

	h := NewHTTPFetcherAllowLocal(5 * time.Second)

	// no credentials -> 401 -> ErrAuthRequired
	if _, _, err := h.Fetch(context.Background(), srv.URL, "", ""); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("without creds expected ErrAuthRequired: %v", err)
	}
	// wrong credentials -> ErrAuthRequired
	if _, _, err := h.Fetch(context.Background(), srv.URL, "reader", "bad"); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("wrong creds expected ErrAuthRequired: %v", err)
	}
	// right credentials -> feed parsed
	f, items, err := h.Fetch(context.Background(), srv.URL, "reader", "s3cret")
	if err != nil {
		t.Fatalf("with creds: %v", err)
	}
	if f.Title != "Example Blog" || len(items) != 2 {
		t.Fatalf("feed with auth badly parsed: %+v (%d items)", f, len(items))
	}
}

// TestClusterKey: normalization groups equivalent titles and separates the
// different ones (#42).
func TestClusterKey(t *testing.T) {
	a := clusterKey(store.NewItem{Title: "  OpenAI releases  GPT-5  ", Body: "<p>AI story</p>"})
	b := clusterKey(store.NewItem{Title: "openai releases gpt-5", Body: "<p>AI story</p>"})
	if a != b {
		t.Errorf("equivalent normalized titles should share a cluster: %s != %s", a, b)
	}
	c := clusterKey(store.NewItem{Title: "OpenAI fires its CEO", Body: "<p>something else</p>"})
	if a == c {
		t.Error("different stories should not share a cluster")
	}
}
