package sanitize

import (
	"strings"
	"testing"
)

func TestBodyStripsScripts(t *testing.T) {
	cases := []struct{ name, in, mustNotContain string }{
		{"script tag", `<p>hello</p><script>alert(1)</script>`, "<script"},
		{"inline handler", `<p onclick="evil()">hola</p>`, "onclick"},
		{"img onerror", `<img src="https://x.example/a.png" onerror="evil()">`, "onerror"},
		{"javascript href", `<a href="javascript:evil()">click</a>`, "javascript:"},
		{"iframe", `<iframe src="https://evil.example"></iframe>`, "<iframe"},
		{"style block", `<style>body{}</style><p>x</p>`, "<style"},
	}
	for _, c := range cases {
		out := Body(c.in)
		if strings.Contains(out, c.mustNotContain) {
			t.Errorf("%s: output contains %q: %s", c.name, c.mustNotContain, out)
		}
	}
}

func TestBodyKeepsFormatting(t *testing.T) {
	in := `<h2>Title</h2><p>Paragraph with <b>bold</b>, <i>italic</i> and <a href="https://example.example/x" title="t">link</a>.</p><ul><li>one</li></ul><figure><img src="https://example.example/i.png" alt="alt text" width="600"><figcaption>caption</figcaption></figure>`
	out := Body(in)
	for _, want := range []string{"<h2>", "<b>bold</b>", `<a href="https://example.example/x"`, "<ul>", "<li>one</li>", `<img src="https://example.example/i.png"`, "<figcaption>caption</figcaption>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in: %s", want, out)
		}
	}
}

func TestBodyEmpty(t *testing.T) {
	if Body("") != "" {
		t.Error("empty must stay empty")
	}
}

func TestBodySanitizesBrokenHTML(t *testing.T) {
	// broken HTML is normalized, the readable content survives
	out := Body(`<p>unclosed <b>bold`)
	if !strings.Contains(out, "unclosed") || !strings.Contains(out, "bold") {
		t.Errorf("content lost: %s", out)
	}
}
