// Package xmp construye y lee el paquete XMP que se guarda junto a cada foto
// (sidecar .xmp) para que etiquetas y favorito sobrevivan fuera del índice.
package xmp

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"
	"time"
)

// RatingFavorite es la puntuación que representa "favorita" en xmp:Rating.
const RatingFavorite = 5

// Meta son los campos que el servicio proyecta al sidecar.
type Meta struct {
	Tags     []string
	Favorite bool
}

// SidecarPath devuelve la ruta del sidecar de una foto (misma carpeta, nombre + .xmp).
func SidecarPath(path string) string { return path + ".xmp" }

// Build genera un paquete XMP con dc:subject para las etiquetas y xmp:Rating
// para el favorito. Es un paquete completo: sustituye al anterior.
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

// Extract lee los campos que gestiona el servicio de un paquete XMP. Tolera
// paquetes escritos por otras herramientas: lo que no reconoce se ignora, y un
// documento ilegible devuelve Meta vacío en vez de error.
func Extract(data []byte) Meta {
	var doc struct {
		RDF struct {
			Descriptions []struct {
				Subject struct {
					Bag struct {
						Li []string `xml:"li"`
					} `xml:"Bag"`
				} `xml:"subject"`
				Rating int `xml:"Rating"`
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
		if d.Rating >= RatingFavorite {
			m.Favorite = true
		}
	}
	return m
}

func escape(b *bytes.Buffer, s string) {
	if err := xml.EscapeText(b, []byte(s)); err != nil {
		b.WriteString(s)
	}
}
