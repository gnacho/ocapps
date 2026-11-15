// Package netguard: mitigates SSRF in outgoing connections (merge of
// ocnews/internal/netguard and ocnotes/internal/netguard - identical). The
// http.Transport resolves the host and rejects destinations to private IPs,
// loopback, link-local or cloud metadata before connecting. It is used in ALL
// the clients that fetch user-controlled URLs (imgproxy, feed fetcher,
// extractor, favicons).
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// DefaultTimeout is the timeout of the SafeClient with no arguments.
const DefaultTimeout = 30 * time.Second

// isBlockedIP decides whether an IP must not be reached (SSRF).
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// IPv4-mapped IPv6 (::ffff:a.b.c.d) -> analyze the underlying IPv4
	if v4 := ip.To4(); v4 != nil {
		return v4.IsLoopback() || v4.IsPrivate() || v4.IsLinkLocalUnicast() ||
			v4.IsLinkLocalMulticast() || v4.IsUnspecified() || v4.IsMulticast()
	}
	// Cloud metadata (169.254.169.254) and the usual reserved ranges
	// (already covered by IsLinkLocalUnicast for 169.254/16).
	return false
}

// CheckURL validates a URL before fetching it: only http/https and whose host
// does not resolve to blocked IPs. Useful to reject early in handlers that
// receive user URLs (in addition to the protection in Dial).
func CheckURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("nil url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme not allowed: %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url without host")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return fmt.Errorf("blocked destination (SSRF): %s -> %s", host, ip)
		}
	}
	return nil
}

// SafeTransport returns an http.Transport with SSRF protection and a
// reasonable dial timeout.
func SafeTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: 10 * time.Second}
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("invalid address %q: %w", addr, err)
			}
			if ips, err := net.LookupIP(host); err != nil {
				return nil, fmt.Errorf("resolve %q: %w", host, err)
			} else {
				for _, ip := range ips {
					if isBlockedIP(ip) {
						return nil, fmt.Errorf("blocked destination (SSRF): %s -> %s", host, ip)
					}
				}
			}
			return d.DialContext(ctx, network, addr)
		},
		MaxIdleConns:        20,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

// SafeClient returns an http.Client with the safe transport and the default
// timeout (30 s).
func SafeClient() *http.Client {
	return Client(DefaultTimeout)
}

// Client returns an http.Client with the safe transport and its own timeout.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: SafeTransport()}
}

// ClientAllowLocal returns a client whose transport allows loopback/local.
// Tests ONLY (httptest listens on 127.0.0.1), never in production.
func ClientAllowLocal(timeout time.Duration) *http.Client {
	t := SafeTransport()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	return &http.Client{Timeout: timeout, Transport: t}
}
