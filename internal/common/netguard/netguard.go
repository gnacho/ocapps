// Package netguard: mitiga SSRF en las conexiones salientes (merge de
// ocnews/internal/netguard y ocnotes/internal/netguard — idénticos). El
// http.Transport resuelve el host y rechaza destinos a IPs privadas,
// loopback, link-local o metadata cloud antes de conectar. Se usa en TODOS
// los clientes que fetchean URLs controladas por el usuario (imgproxy, feed
// fetcher, extractor, favicons).
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// DefaultTimeout es el timeout del SafeClient sin argumentos.
const DefaultTimeout = 30 * time.Second

// isBlockedIP decide si una IP no debe alcanzarse (SSRF).
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// IPv4-mapped IPv6 (::ffff:a.b.c.d) → analizar la IPv4 subyacente
	if v4 := ip.To4(); v4 != nil {
		return v4.IsLoopback() || v4.IsPrivate() || v4.IsLinkLocalUnicast() ||
			v4.IsLinkLocalMulticast() || v4.IsUnspecified() || v4.IsMulticast()
	}
	// Metadata de cloud (169.254.169.254) y rangos reservados habituales
	// (ya cubiertos por IsLinkLocalUnicast para 169.254/16).
	return false
}

// CheckURL valida una URL antes de fetchearla: solo http/https y cuyo host
// no resuelva a IPs bloqueadas. Útil para rechazar temprano en handlers que
// reciben URLs del usuario (además de la protección en Dial).
func CheckURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("url nula")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("esquema no permitido: %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url sin host")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("resolver %q: %w", host, err)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return fmt.Errorf("destino bloqueado (SSRF): %s -> %s", host, ip)
		}
	}
	return nil
}

// SafeTransport devuelve un http.Transport con protección SSRF y un timeout
// de dial razonable.
func SafeTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: 10 * time.Second}
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("dirección inválida %q: %w", addr, err)
			}
			if ips, err := net.LookupIP(host); err != nil {
				return nil, fmt.Errorf("resolver %q: %w", host, err)
			} else {
				for _, ip := range ips {
					if isBlockedIP(ip) {
						return nil, fmt.Errorf("destino bloqueado (SSRF): %s -> %s", host, ip)
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

// SafeClient devuelve un http.Client con el transporte seguro y el timeout
// por defecto (30 s).
func SafeClient() *http.Client {
	return Client(DefaultTimeout)
}

// Client devuelve un http.Client con el transporte seguro y timeout propio.
func Client(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: SafeTransport()}
}

// ClientAllowLocal devuelve un client cuyo transporte permite loopback/local.
// SOLO para tests (httptest escucha en 127.0.0.1); nunca en producción.
func ClientAllowLocal(timeout time.Duration) *http.Client {
	t := SafeTransport()
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	return &http.Client{Timeout: timeout, Transport: t}
}
