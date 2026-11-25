package privacy

import (
	"html"
	"net/url"
	"regexp"
	"strings"
)

// Level of link hygiene applied to the href of anchors in an article body.
type Level string

const (
	// LevelOff leaves links the way the site wrote them.
	LevelOff Level = "off"
	// LevelTracking strips tracking parameters. It is the default: the same
	// list already applied to the item URL, with no heuristics.
	LevelTracking Level = "tracking"
	// LevelAggressive also unwraps intermediaries (shorteners and publisher
	// redirectors). Opt-in because it can be wrong on a legitimate URL that
	// happens to carry a destination parameter.
	LevelAggressive Level = "aggressive"
)

// ValidLevel normalizes and validates a level. Empty means tracking (default).
func ValidLevel(l string) (Level, bool) {
	switch Level(strings.ToLower(strings.TrimSpace(l))) {
	case "":
		return LevelTracking, true
	case LevelOff:
		return LevelOff, true
	case LevelTracking:
		return LevelTracking, true
	case LevelAggressive:
		return LevelAggressive, true
	}
	return "", false
}

var (
	aTagRe   = regexp.MustCompile(`(?i)<a\b[^>]*>`)
	hrefRe   = regexp.MustCompile(`(?i)\bhref\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	domainRe = regexp.MustCompile(`(?i)^(?:[a-z0-9_-]+\.)+[a-z]{2,}$`)

	// shorteners: domains that only shorten and redirect. Compared by suffix
	// so subdomains count too (bit.ly, www.bit.ly).
	shorteners = []string{
		"bit.ly", "bitly.com", "j.mp", "t.co", "lnkd.in", "goo.gl", "ow.ly",
		"buff.ly", "tinyurl.com", "is.gd", "amzn.to", "amzn.eu", "rb.gy",
		"cutt.ly", "shorturl.at", "trib.al", "dlvr.it", "flip.it", "sco.lt",
		"wp.me", "kutt.it", "t.ly", "s.id", "cstu.io",
	}

	// redirectorLabels: host labels typical of a publisher exit panel
	// (go.example.com, out.example.com, links.example.com).
	redirectorLabels = []string{
		"go", "out", "link", "links", "redirect", "redirects", "click",
		"clicktrack", "url", "away", "exit", "goto", "r", "wire", "refer",
	}

	// destParams: parameter names that carry the real destination.
	destParams = []string{"url", "u", "target", "dest", "destination", "redirect", "redirect_uri", "to", "link", "href", "out"}
)

// IsShortener reports whether the host is a known shortener.
func IsShortener(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, s := range shorteners {
		if h == s || strings.HasSuffix(h, "."+s) {
			return true
		}
	}
	return false
}

// isRedirectorLabel reports whether the first host label is an exit panel.
func isRedirectorLabel(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if i := strings.Index(h, "."); i > 0 {
		h = h[:i]
	}
	for _, l := range redirectorLabels {
		if h == l {
			return true
		}
	}
	return false
}

// IsRedirector reports whether the host looks like a site exit panel
// (go.example.com, links.news.es).
func IsRedirector(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return isRedirectorLabel(u.Host)
}

// Unwrap returns the destination hidden behind an intermediary URL, and
// whether it found one. It never leaves the process: it only unwraps when a
// single destination parameter points at a full host.
func Unwrap(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	q := u.Query()

	// 1) A parameter holding a full destination URL.
	for _, p := range destParams {
		for k, vs := range q {
			if !strings.EqualFold(k, p) || len(vs) == 0 {
				continue
			}
			// The value may come without a scheme (go.example.com/x?url=example.com/a).
			cand := strings.TrimSpace(vs[0])
			if !strings.Contains(cand, "://") {
				cand = "https://" + strings.TrimPrefix(cand, "//")
			}
			du, err := url.Parse(cand)
			if err != nil || (du.Scheme != "http" && du.Scheme != "https") || du.Host == "" {
				continue
			}
			if !domainRe.MatchString(du.Hostname()) {
				continue
			}
			// destination on the same host: not an intermediary
			if strings.EqualFold(du.Host, u.Host) {
				continue
			}
			return du.String(), true
		}
	}

	// 2) Without a destination parameter a shortener cannot be resolved
	// without going out to the network. The original link is kept: better not
	// to guess.
	return "", false
}

// CleanHref applies the chosen level to one href: strips tracking and, at the
// aggressive level, unwraps the intermediary and cleans its destination too.
func CleanHref(raw string, level Level) string {
	if level == LevelOff {
		return raw
	}
	clean := StripParams(raw)
	if level == LevelAggressive {
		if dest, ok := Unwrap(clean); ok {
			clean = StripParams(dest)
		}
	}
	return clean
}

// CleanLinks rewrites the href of every anchor in the HTML. Best effort with
// a regex over HTML already sanitized by bluemonday (quoted attributes, no
// odd entities).
func CleanLinks(doc string, level Level) string {
	if level == LevelOff || doc == "" {
		return doc
	}
	if !strings.Contains(strings.ToLower(doc), "<a") {
		return doc
	}
	return aTagRe.ReplaceAllStringFunc(doc, func(tag string) string {
		return hrefRe.ReplaceAllStringFunc(tag, func(m string) string {
			sub := hrefRe.FindStringSubmatch(m)
			if len(sub) < 3 {
				return m
			}
			raw := sub[1]
			quote := `"`
			if raw == "" && sub[2] != "" {
				raw, quote = sub[2], "'"
			}
			if raw == "" {
				return m
			}
			// Sanitized HTML keeps entities (&amp;) in attributes: work with the
			// real URL and escape on the way out.
			real := html.UnescapeString(raw)
			clean := CleanHref(real, level)
			if clean == real {
				return m
			}
			return "href=" + quote + html.EscapeString(clean) + quote
		})
	})
}
