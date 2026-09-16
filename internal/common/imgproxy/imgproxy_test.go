package imgproxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestProxy(t *testing.T) *Proxy {
	t.Helper()
	p, err := NewAllowLocal(t.TempDir(), slog.Default())
	if err != nil {
		t.Fatalf("NewAllowLocal: %v", err)
	}
	return p
}

// TestSignVectorFijo: firma HMAC calculada con el algoritmo EXACTO de
// ocnews/ocnotes (hex(HMAC-SHA256(secret, url))[:32]) sobre un secret fijo.
// Garantía de compatibilidad: las URLs firmadas por los servicios antiguos
// siguen validando con el proxy unificado si se conserva el imgsecret.
func TestSignVectorFijo(t *testing.T) {
	dir := t.TempDir()
	secret := []byte("0123456789abcdef0123456789abcdef") // 32 bytes fijos
	if err := os.WriteFile(filepath.Join(dir, "imgsecret"), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(dir, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const (
		rawURL  = "https://example.com/img/photo%20x.jpg?w=1"
		wantSig = "a0e5a07e9778e36f5282ebe8eff1782d" // vector fijo (algoritmo ocnews)
	)
	if got := p.Sign(rawURL); got != wantSig {
		t.Fatalf("Sign: got %q, want %q (¡algoritmo HMAC cambiado, firmas viejas rotas!)", got, wantSig)
	}
	if !p.valid(rawURL, wantSig) {
		t.Fatal("firma del vector rechazada")
	}
}

func TestSignRoundtrip(t *testing.T) {
	p := newTestProxy(t)
	u := "http://img.youtube.com/vi/abc/0.jpg"
	sig := p.Sign(u)
	if !p.valid(u, sig) {
		t.Fatal("firma válida rechazada")
	}
	if p.valid(u+"x", sig) {
		t.Fatal("firma de otra URL aceptada")
	}
	if p.valid(u, sig+"0") {
		t.Fatal("firma manipulada aceptada")
	}
}

// TestSecretPersistente: el mismo imgsecret produce las mismas firmas entre
// instancias (reinicio del servicio).
func TestSecretPersistente(t *testing.T) {
	dir := t.TempDir()
	p1, err := New(dir, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	p2, err := New(dir, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	u := "https://example.com/a.jpg"
	if p1.Sign(u) != p2.Sign(u) {
		t.Fatal("firmas distintas con el mismo imgsecret persistido")
	}
}

// TestSecretCorruptoFailLoud: un imgsecret demasiado corto es un error, no
// una regeneración silenciosa (SPEC Q3).
func TestSecretCorruptoFailLoud(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "imgsecret"), []byte("debil"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir, slog.Default()); err == nil {
		t.Fatal("esperaba error por imgsecret corrupto")
	}
}

func TestProxiedURLRoundtrip(t *testing.T) {
	base := "/index.php/apps/notes/api/v1/"
	u := "https://example.com/a%20b.jpg?x=1&y=2,3"
	pu := ProxiedURL(base, u, "sig")
	q, err := url.Parse(pu)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if q.Path != base+"img" {
		t.Fatalf("path inesperado: %s", q.Path)
	}
	if got := q.Query().Get("u"); got != u {
		t.Fatalf("u no recupera la original: %q != %q", got, u)
	}
}

func TestServeRejectsBadSignature(t *testing.T) {
	p := newTestProxy(t)
	srv := httptest.NewServer(http.HandlerFunc(p.Serve))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "?u=http://example.com/x.jpg&t=badbadbad")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("firma mala: got %d, want 403", resp.StatusCode)
	}
}

// TestServeFirmaAntiguaValida: una petición firmada con el algoritmo/secret
// viejo (vector fijo) sirve la imagen → compat de firmas extremo a extremo.
func TestServeFirmaAntiguaValida(t *testing.T) {
	dir := t.TempDir()
	secret := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(filepath.Join(dir, "imgsecret"), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := NewAllowLocal(dir, slog.Default())
	if err != nil {
		t.Fatal(err)
	}

	// origen "externo" simulado
	img := []byte("\xff\xd8\xff fake-jpeg")
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(img)
	}))
	defer origin.Close()

	sig := p.Sign(origin.URL + "/foto.jpg")
	front := httptest.NewServer(http.HandlerFunc(p.Serve))
	defer front.Close()

	resp, err := http.Get(front.URL + "?u=" + url.QueryEscape(origin.URL+"/foto.jpg") + "&t=" + sig)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("firma vieja rechazada en Serve: got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != string(img) {
		t.Fatalf("cuerpo: %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type: %q", ct)
	}

	// segunda petición: sale de la caché en disco
	resp2, err := http.Get(front.URL + "?u=" + url.QueryEscape(origin.URL+"/foto.jpg") + "&t=" + sig)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("cache: got %d", resp2.StatusCode)
	}
}

func TestServeRechazaEsquema(t *testing.T) {
	p := newTestProxy(t)
	srv := httptest.NewServer(http.HandlerFunc(p.Serve))
	defer srv.Close()
	u := "file:///etc/passwd"
	resp, err := http.Get(srv.URL + "?u=" + url.QueryEscape(u) + "&t=" + p.Sign(u))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("esquema file://: got %d, want 403", resp.StatusCode)
	}
}

func TestServeTipoNoPermitido(t *testing.T) {
	p := newTestProxy(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>"))
	}))
	defer origin.Close()
	srv := httptest.NewServer(http.HandlerFunc(p.Serve))
	defer srv.Close()
	u := origin.URL + "/x"
	resp, err := http.Get(srv.URL + "?u=" + url.QueryEscape(u) + "&t=" + p.Sign(u))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("html: got %d, want 415", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("error no es texto plano: %q", resp.Header.Get("Content-Type"))
	}
}
