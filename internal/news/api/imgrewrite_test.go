package api

import (
	"strings"
	"testing"
)

func TestRewriteImages(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	in := `<p>hola</p><img src="https://cdn.example/pic.webp?resize=406%2C232&amp;ssl=1" alt="x"><img src="/local.png">`
	out := e.srv.rewriteImages(in)
	if !strings.Contains(out, `/img?u=https%3A%2F%2Fcdn.example%2Fpic.webp`) {
		t.Fatalf("external src not rewritten with QueryEscape: %s", out)
	}
	if !strings.Contains(out, "&amp;t=") {
		t.Fatalf("missing the signed parameter: %s", out)
	}
	if !strings.Contains(out, `src="/local.png"`) {
		t.Fatalf("relative src must not be touched: %s", out)
	}
	// round-trip: the signed u must match what q.Get() will recover
	sig := e.srv.imgs.Sign("https://cdn.example/pic.webp?resize=406%2C232&ssl=1")
	if !strings.Contains(out, "&amp;t="+sig) {
		t.Fatalf("the signature must be computed over the exact original URL: %s", out)
	}
}
