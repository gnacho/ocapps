// Package xmp builds and reads the XMP packet stored next to each photo
// (.xmp sidecar) so that tags and favorite survive outside the index.
package xmp

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"
	"time"
)

// RatingFavorite is the rating that means "favorite" in xmp:Rating.
const RatingFavorite = 5

// Meta holds the fields the service projects into the sidecar. HasRating tells
// "the sidecar has no xmp:Rating" apart from "it carries an explicit 0": when
// reading a foreign packet, without that difference you cannot know whether the
// favorite should be cleared or left as it is.
type Meta struct {
	Tags      []string
	Favorite  bool
	HasRating bool
}

// SidecarPath returns the sidecar path of a photo (same folder, name + .xmp).
func SidecarPath(path string) string { return path + ".xmp" }

// Build produces an XMP packet with dc:subject for the tags and xmp:Rating
// for the favorite flag. It is a complete packet: it replaces the previous one.
func Build(m Meta) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xpacket begin="` + "\ufeff" + `" id="W5M0MpCehiHzreSzNTczkc9d"?>` + "\n")
	b.WriteString(`<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="ocapps">` + "\n")
	b.WriteString(` <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` + "\n")
	b.WriteString(`  <rdf:Description rdf:about=""` + "\n")
	b.WriteString(`    xmlns:dc="http://purl.org/dc/elements/1.1/"` + "\n")
	b.WriteString(`    xmlns:xmp="http://ns.adobe.com/xap/1.0/">` + "\n")
	b.WriteString(`   <dc:subject><rdf:Bag>`)
	for _, t := range m.Tags {
		b.WriteString("<rdf:li>")
		escape(&b, t)
		b.WriteString("</rdf:li>")
	}
	b.WriteString(`</rdf:Bag></dc:subject>` + "\n")
	rating := 0
	if m.Favorite {
		rating = RatingFavorite
	}
	b.WriteString(`   <xmp:Rating>` + strconv.Itoa(rating) + `</xmp:Rating>` + "\n")
	b.WriteString(`   <xmp:MetadataDate>` + time.Now().UTC().Format(time.RFC3339) + `</xmp:MetadataDate>` + "\n")
	b.WriteString(`  </rdf:Description>` + "\n")
	b.WriteString(` </rdf:RDF>` + "\n")
	b.WriteString(`</x:xmpmeta>` + "\n")
	b.WriteString(`<?xpacket end="w"?>` + "\n")
	return b.Bytes()
}

// Extract reads the fields the service manages from an XMP packet. It tolerates
// packets written by other tools: whatever it does not recognize is ignored, and
// an unreadable document returns an empty Meta instead of an error.
func Extract(data []byte) Meta {
	var doc struct {
		RDF struct {
			Descriptions []struct {
				Subject struct {
					Bag struct {
						Li []string `xml:"li"`
					} `xml:"Bag"`
				} `xml:"subject"`
				Rating *int `xml:"Rating"`
			} `xml:"Description"`
		} `xml:"RDF"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return Meta{}
	}
	var m Meta
	for _, d := range doc.RDF.Descriptions {
		for _, li := range d.Subject.Bag.Li {
			if t := strings.TrimSpace(li); t != "" {
				m.Tags = append(m.Tags, t)
			}
		}
		if d.Rating != nil {
			m.HasRating = true
			if *d.Rating >= RatingFavorite {
				m.Favorite = true
			}
		}
	}
	return m
}

func escape(b *bytes.Buffer, s string) {
	if err := xml.EscapeText(b, []byte(s)); err != nil {
		b.WriteString(s)
	}
}
