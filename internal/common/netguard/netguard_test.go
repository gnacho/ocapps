package netguard

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// TestSafeTransportBlocksPrivate: el transporte seguro rechaza loopback/local.
func TestSafeTransportBlocksPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := SafeClient().Get(srv.URL); err == nil {
		t.Fatalf("el transporte seguro conectó a loopback %s (SSRF no bloqueado)", srv.URL)
	}
	// el client allow-local SÍ debe conectar (solo tests)
	lc := ClientAllowLocal(3 * time.Second)
	resp, err := lc.Get(srv.URL)
	if err != nil {
		t.Fatalf("allow-local no conectó a loopback: %v", err)
	}
	resp.Body.Close()
}

// TestIsBlockedIP cubre los rangos críticos.
func TestIsBlockedIP(t *testing.T) {
	cases := []struct {
		ip    string
		block bool
	}{
		{"127.0.0.1", true},        // loopback
		{"10.0.0.5", true},         // RFC1918
		{"192.168.1.1", true},      // RFC1918
		{"172.16.0.1", true},       // RFC1918
		{"169.254.169.254", true},  // metadata
		{"::1", true},              // loopback v6
		{"::ffff:127.0.0.1", true}, // v4-mapped loopback
		{"8.8.8.8", false},         // público
		{"1.1.1.1", false},         // público
	}
	for _, c := range cases {
		if got := isBlockedIP(net.ParseIP(c.ip)); got != c.block {
			t.Errorf("%s: esperaba block=%v, tengo %v", c.ip, c.block, got)
		}
	}
}

func TestCheckURL(t *testing.T) {
	cases := []struct {
		raw     string
		wantErr bool
	}{
		{"http://127.0.0.1:8080/x", true}, // loopback
		{"http://192.168.0.1/x", true},    // privada
		{"http://169.254.169.254/", true}, // metadata
		{"ftp://example.com/x", true},     // esquema no permitido
		{"http:///x", true},               // sin host
		{"http://8.8.8.8/x", false},       // IP pública literal (sin DNS)
		{"https://1.1.1.1:443/y", false},  // IP pública literal
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", c.raw, err)
		}
		if err := CheckURL(u); (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", c.raw, err, c.wantErr)
		}
	}
	if err := CheckURL(nil); err == nil {
		t.Error("url nula: esperaba error")
	}
}
