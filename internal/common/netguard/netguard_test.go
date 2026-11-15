package netguard

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// TestSafeTransportBlocksPrivate: the safe transport rejects loopback/local.
func TestSafeTransportBlocksPrivate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := SafeClient().Get(srv.URL); err == nil {
		t.Fatalf("the safe transport connected to loopback %s (SSRF not blocked)", srv.URL)
	}
	// the allow-local client MUST connect (tests only)
	lc := ClientAllowLocal(3 * time.Second)
	resp, err := lc.Get(srv.URL)
	if err != nil {
		t.Fatalf("allow-local did not connect to loopback: %v", err)
	}
	_ = resp.Body.Close()
}

// TestIsBlockedIP covers the critical ranges.
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
		{"8.8.8.8", false},         // public
		{"1.1.1.1", false},         // public
	}
	for _, c := range cases {
		if got := isBlockedIP(net.ParseIP(c.ip)); got != c.block {
			t.Errorf("%s: want block=%v, got %v", c.ip, c.block, got)
		}
	}
}

func TestCheckURL(t *testing.T) {
	cases := []struct {
		raw     string
		wantErr bool
	}{
		{"http://127.0.0.1:8080/x", true}, // loopback
		{"http://192.168.0.1/x", true},    // private
		{"http://169.254.169.254/", true}, // metadata
		{"ftp://example.com/x", true},     // scheme not allowed
		{"http:///x", true},               // no host
		{"http://8.8.8.8/x", false},       // literal public IP (no DNS)
		{"https://1.1.1.1:443/y", false},  // literal public IP
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
		t.Error("nil url: expected an error")
	}
}
