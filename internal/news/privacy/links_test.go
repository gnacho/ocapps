package privacy

import (
	"strings"
	"testing"
)

func TestCleanLinksTracking(t *testing.T) {
	in := `<p><a href="https://pub.example/a?utm_source=rss&amp;id=7">uno</a> y <a href="https://pub.example/b">dos</a></p>`
	out := CleanLinks(in, LevelTracking)
	if strings.Contains(out, "utm_source") {
		t.Errorf("utm_source not removed: %s", out)
	}
	if !strings.Contains(out, `href="https://pub.example/a?id=7"`) {
		t.Errorf("legitimate query lost: %s", out)
	}
	if !strings.Contains(out, `href="https://pub.example/b"`) {
		t.Errorf("clean link altered: %s", out)
	}
}

func TestCleanLinksKeepsQuote(t *testing.T) {
	out := CleanLinks(`<a href='https://x.example/a?fbclid=1'>x</a>`, LevelTracking)
	if !strings.Contains(out, `href='https://x.example/a'`) {
		t.Errorf("single quotes not preserved: %s", out)
	}
}

func TestCleanLinksOff(t *testing.T) {
	in := `<a href="https://x.example/a?utm_source=rss">x</a>`
	if got := CleanLinks(in, LevelOff); got != in {
		t.Errorf("off must leave the HTML untouched: %s", got)
	}
}

func TestCleanLinksUntouchedWithoutAnchors(t *testing.T) {
	in := `<p>sin enlaces</p>`
	if got := CleanLinks(in, LevelAggressive); got != in {
		t.Errorf("HTML without anchors altered: %s", got)
	}
}

func TestUnwrapDestinationParam(t *testing.T) {
	cases := []struct{ in, want string }{
		// exit panel with the destination in the query
		{"https://go.medio.example/out?url=https%3A%2F%2Fdestino.example%2Fnota&utm_source=feed", "https://destino.example/nota"},
		// destination without a scheme
		{"https://links.medio.example/click?u=destino.example%2Fa", "https://destino.example/a"},
		// shortener with the destination in the query
		{"https://bit.ly/abc?url=https://destino.example/b", "https://destino.example/b"},
	}
	for _, c := range cases {
		got := CleanHref(c.in, LevelAggressive)
		if got != c.want {
			t.Errorf("CleanHref(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUnwrapRefusesHostlessDestination(t *testing.T) {
	// url= with garbage: not unwrapped
	in := "https://go.medio.example/out?url=no-es-un-dominio/../x"
	if got := CleanHref(in, LevelAggressive); got != in {
		t.Errorf("invalid destination unwrapped: %q", got)
	}
}

func TestUnwrapSameHostIgnored(t *testing.T) {
	in := "https://medio.example/r?url=https%3A%2F%2Fmedio.example%2Fnota"
	if got := CleanHref(in, LevelAggressive); got != in {
		t.Errorf("destination on the same host treated as an intermediary: %q", got)
	}
}

func TestUnwrapNotAppliedAtTrackingLevel(t *testing.T) {
	in := "https://go.medio.example/out?url=https%3A%2F%2Fdestino.example%2Fa"
	if got := CleanHref(in, LevelTracking); got != in {
		t.Errorf("tracking must not unwrap: %q", got)
	}
}

func TestValidLevel(t *testing.T) {
	cases := []struct {
		in    string
		want  Level
		valid bool
	}{
		{"", LevelTracking, true},
		{"off", LevelOff, true},
		{"tracking", LevelTracking, true},
		{"aggressive", LevelAggressive, true},
		{"AGGRESSIVE", LevelAggressive, true},
		{" medium ", "", false},
	}
	for _, c := range cases {
		got, ok := ValidLevel(c.in)
		if ok != c.valid || got != c.want {
			t.Errorf("ValidLevel(%q) = %q,%v; want %q,%v", c.in, got, ok, c.want, c.valid)
		}
	}
}

func TestIsShortener(t *testing.T) {
	for _, h := range []string{"bit.ly", "www.bit.ly", "t.co", "lnkd.in"} {
		if !IsShortener(h) {
			t.Errorf("%q should be a shortener", h)
		}
	}
	for _, h := range []string{"ejemplo.example", "bit.lyfalso.example"} {
		if IsShortener(h) {
			t.Errorf("%q NO should be a shortener", h)
		}
	}
}
