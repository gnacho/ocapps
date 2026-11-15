package xmp

import (
	"strings"
	"testing"
)

func TestBuildExtractRoundTrip(t *testing.T) {
	in := Meta{Tags: []string{"beach", "sunset"}, Favorite: true}
	got := Extract(Build(in))
	if len(got.Tags) != 2 || got.Tags[0] != "beach" || got.Tags[1] != "sunset" {
		t.Fatalf("tags: %#v", got.Tags)
	}
	if !got.Favorite {
		t.Fatal("expected favorite")
	}
}

func TestBuildWithoutTagsOrFavorite(t *testing.T) {
	got := Extract(Build(Meta{}))
	if len(got.Tags) != 0 {
		t.Fatalf("tags: %#v", got.Tags)
	}
	if got.Favorite {
		t.Fatal("should not be favorite")
	}
}

func TestBuildEscapesTags(t *testing.T) {
	packet := string(Build(Meta{Tags: []string{"a & b <c>"}}))
	if strings.Contains(packet, "a & b <c>") {
		t.Fatal("the tag should be escaped")
	}
	got := Extract([]byte(packet))
	if len(got.Tags) != 1 || got.Tags[0] != "a & b <c>" {
		t.Fatalf("tags: %#v", got.Tags)
	}
}

func TestExtractForeignPacket(t *testing.T) {
	// Lightroom style packet: two Descriptions, several namespaces
	// and properties we do not manage.
	packet := `<?xpacket begin="" id="W5M0MpCehiHzreSzNTczkc9d"?>
<x:xmpmeta xmlns:x="adobe:ns:meta/">
 <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
  <rdf:Description rdf:about=""
    xmlns:dc="http://purl.org/dc/elements/1.1/"
    xmlns:xmp="http://ns.adobe.com/xap/1.0/">
   <dc:subject><rdf:Bag><rdf:li>travel</rdf:li><rdf:li>sea</rdf:li></rdf:Bag></dc:subject>
  </rdf:Description>
  <rdf:Description rdf:about="" xmlns:xmp="http://ns.adobe.com/xap/1.0/">
   <xmp:Rating>5</xmp:Rating>
   <xmp:CreatorTool>Adobe Lightroom</xmp:CreatorTool>
  </rdf:Description>
 </rdf:RDF>
</x:xmpmeta>
<?xpacket end="w"?>`
	got := Extract([]byte(packet))
	if len(got.Tags) != 2 || got.Tags[0] != "travel" || got.Tags[1] != "sea" {
		t.Fatalf("tags: %#v", got.Tags)
	}
	if !got.Favorite {
		t.Fatal("rating 5 should be read as favorite")
	}
}

func TestExtractRejectsGarbage(t *testing.T) {
	got := Extract([]byte("not xmp"))
	if len(got.Tags) != 0 || got.Favorite {
		t.Fatalf("expected empty, got %#v", got)
	}
}

func TestSidecarPath(t *testing.T) {
	if p := SidecarPath("/Fotos/IMG_1.jpg"); p != "/Fotos/IMG_1.jpg.xmp" {
		t.Fatalf("path: %s", p)
	}
}

// TestExtractHasRating: telling "no xmp:Rating" apart from "explicit rating 0" is
// what lets the scanner avoid clearing a local favorite with a foreign sidecar
// that says nothing about ratings.
func TestExtractHasRating(t *testing.T) {
	cases := []struct {
		name    string
		rating  string
		wantHas bool
		wantFav bool
	}{
		{name: "no rating", rating: "", wantHas: false, wantFav: false},
		{name: "explicit zero", rating: `<xmp:Rating>0</xmp:Rating>`, wantHas: true, wantFav: false},
		{name: "three stars", rating: `<xmp:Rating>3</xmp:Rating>`, wantHas: true, wantFav: false},
		{name: "five stars", rating: `<xmp:Rating>5</xmp:Rating>`, wantHas: true, wantFav: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			packet := `<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` +
				`<rdf:Description rdf:about="" xmlns:xmp="http://ns.adobe.com/xap/1.0/">` + tc.rating +
				`</rdf:Description></rdf:RDF></x:xmpmeta>`
			got := Extract([]byte(packet))
			if got.HasRating != tc.wantHas {
				t.Errorf("HasRating=%v, want %v", got.HasRating, tc.wantHas)
			}
			if got.Favorite != tc.wantFav {
				t.Errorf("Favorite=%v, want %v", got.Favorite, tc.wantFav)
			}
		})
	}
}

// TestBuildReadsWithRating: a sidecar of our own always declares the rating, so
// when read back HasRating is true whether it is favorite or not.
func TestBuildReadsWithRating(t *testing.T) {
	for _, fav := range []bool{true, false} {
		got := Extract(Build(Meta{Tags: []string{"x"}, Favorite: fav}))
		if !got.HasRating || got.Favorite != fav {
			t.Fatalf("fav=%v: HasRating=%v Favorite=%v", fav, got.HasRating, got.Favorite)
		}
	}
}
