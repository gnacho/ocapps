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
		t.Fatal("se esperaba favorita")
	}
}

func TestBuildWithoutTagsOrFavorite(t *testing.T) {
	got := Extract(Build(Meta{}))
	if len(got.Tags) != 0 {
		t.Fatalf("tags: %#v", got.Tags)
	}
	if got.Favorite {
		t.Fatal("no debería ser favorita")
	}
}

func TestBuildEscapesTags(t *testing.T) {
	packet := string(Build(Meta{Tags: []string{"a & b <c>"}}))
	if strings.Contains(packet, "a & b <c>") {
		t.Fatal("la etiqueta debería ir escapada")
	}
	got := Extract([]byte(packet))
	if len(got.Tags) != 1 || got.Tags[0] != "a & b <c>" {
		t.Fatalf("tags: %#v", got.Tags)
	}
}

func TestExtractForeignPacket(t *testing.T) {
	// Paquete al estilo Lightroom: dos Description, varios espacios de nombres
	// y propiedades que no gestionamos.
	packet := `<?xpacket begin="" id="W5M0MpCehiHzreSzNTczkc9d"?>
<x:xmpmeta xmlns:x="adobe:ns:meta/">
 <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
  <rdf:Description rdf:about=""
    xmlns:dc="http://purl.org/dc/elements/1.1/"
    xmlns:xmp="http://ns.adobe.com/xap/1.0/">
   <dc:subject><rdf:Bag><rdf:li>viaje</rdf:li><rdf:li>mar</rdf:li></rdf:Bag></dc:subject>
  </rdf:Description>
  <rdf:Description rdf:about="" xmlns:xmp="http://ns.adobe.com/xap/1.0/">
   <xmp:Rating>5</xmp:Rating>
   <xmp:CreatorTool>Adobe Lightroom</xmp:CreatorTool>
  </rdf:Description>
 </rdf:RDF>
</x:xmpmeta>
<?xpacket end="w"?>`
	got := Extract([]byte(packet))
	if len(got.Tags) != 2 || got.Tags[0] != "viaje" || got.Tags[1] != "mar" {
		t.Fatalf("tags: %#v", got.Tags)
	}
	if !got.Favorite {
		t.Fatal("rating 5 debería leerse como favorita")
	}
}

func TestExtractRejectsGarbage(t *testing.T) {
	got := Extract([]byte("no es xmp"))
	if len(got.Tags) != 0 || got.Favorite {
		t.Fatalf("se esperaba vacío, llegó %#v", got)
	}
}

func TestSidecarPath(t *testing.T) {
	if p := SidecarPath("/Fotos/IMG_1.jpg"); p != "/Fotos/IMG_1.jpg.xmp" {
		t.Fatalf("path: %s", p)
	}
}
