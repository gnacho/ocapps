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

// TestSignVectorFijo: HMAC signature computed with the EXACT algorithm of
// ocnews/ocnotes (hex(HMAC-SHA256(secret, url))[:32]) over a fixed secret.
// Compatibility guarantee: URLs signed by the old services keep validating
// with the unified proxy as long as the imgsecret is kept.
func TestSignVectorFijo(t *testing.T) {
	dir := t.TempDir()
	secret := []byte("0123456789abcdef0123456789abcdef") // 32 fixed bytes
	if err := os.WriteFile(filepath.Join(dir, "imgsecret"), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(dir, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const (
		rawURL  = "https://example.com/img/photo%20x.jpg?w=1"
		wantSig = "a0e5a07e9778e36f5282ebe8eff1782d" // fixed vector (ocnews algorithm)
	)
	if got := p.Sign(rawURL); got != wantSig {
		t.Fatalf("Sign: got %q, want %q (HMAC algorithm changed, old signatures broken!)", got, wantSig)
	}
	if !p.valid(rawURL, wantSig) {
		t.Fatal("vector signature rejected")
	}
}

func TestSignRoundtrip(t *testing.T) {
	p := newTestProxy(t)
	u := "http://img.youtube.com/vi/abc/0.jpg"
	sig := p.Sign(u)
	if !p.valid(u, sig) {
		t.Fatal("valid signature rejected")
	}
	if p.valid(u+"x", sig) {
		t.Fatal("signature of another URL accepted")
	}
	if p.valid(u, sig+"0") {
		t.Fatal("tampered signature accepted")
	}
}

// TestSecretPersistente: the same imgsecret produces the same signatures
// across instances (service restart).
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
		t.Fatal("different signatures with the same persisted imgsecret")
	}
}

// TestSecretCorruptoFailLoud: an imgsecret that is too short is an error, not
// a silent regeneration (SPEC Q3).
func TestSecretCorruptoFailLoud(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "imgsecret"), []byte("weak"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir, slog.Default()); err == nil {
		t.Fatal("expected an error for a corrupt imgsecret")
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
		t.Fatalf("unexpected path: %s", q.Path)
	}
	if got := q.Query().Get("u"); got != u {
		t.Fatalf("u does not recover the original: %q != %q", got, u)
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad signature: got %d, want 403", resp.StatusCode)
	}
}

// TestServeFirmaAntiguaValida: a request signed with the old algorithm/secret
// (fixed vector) serves the image -> end to end signature compatibility.
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

	// simulated "external" origin
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("old signature rejected in Serve: got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != string(img) {
		t.Fatalf("body: %q", body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type: %q", ct)
	}

	// second request: it comes from the on disk cache
	resp2, err := http.Get(front.URL + "?u=" + url.QueryEscape(origin.URL+"/foto.jpg") + "&t=" + sig)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("file:// scheme: got %d, want 403", resp.StatusCode)
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("html: got %d, want 415", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("error is not plain text: %q", resp.Header.Get("Content-Type"))
	}
}
