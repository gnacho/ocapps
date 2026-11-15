package i18n

import "testing"

func TestNegotiate(t *testing.T) {
	cases := []struct {
		user, accept string
		want         Lang
	}{
		{"es", "", ES},
		{"en", "es-ES,es;q=0.9", EN}, // explicit preference wins
		{"auto", "es-ES,es;q=0.9,en;q=0.8", ES},
		{"auto", "en-GB,en;q=0.9", EN},
		{"auto", "fr-FR,de;q=0.9", EN}, // no match -> default
		{"auto", "", EN},
		{"", "es", ES},
		{"auto", "es-ES", ES},
	}
	for _, c := range cases {
		if got := Negotiate(c.user, c.accept); got != c.want {
			t.Errorf("Negotiate(%q,%q)=%q, expected %q", c.user, c.accept, got, c.want)
		}
	}
}

func TestT(t *testing.T) {
	if T(ES, "folder_not_found") != "carpeta no encontrada" {
		t.Error("wrong ES message")
	}
	if T(EN, "folder_not_found") != "folder not found" {
		t.Error("wrong EN message")
	}
	if T(Lang("fr"), "folder_not_found") != "folder not found" {
		t.Error("unknown language must fall back to EN")
	}
	if T(ES, "missing_key") != "error interno" {
		t.Error("unknown key must fall back to internal_error")
	}
}
