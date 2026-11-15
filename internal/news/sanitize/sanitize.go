// Package sanitize: HTML cleanup of item bodies before
// persisting them. Conservative policy: the usual formatting of articles is
// kept (paragraphs, headings, lists, images, links, tables)
// and everything executable or dangerous is removed (script, style, iframes,
// on* handlers, javascript:). Bluemonday rewrites the HTML to valid markup.
package sanitize

import (
	"github.com/microcosm-cc/bluemonday"
)

var policy *bluemonday.Policy

func init() {
	p := bluemonday.NewPolicy()

	// Usual text and structure elements in articles.
	p.AllowElements("p", "br", "hr", "blockquote", "pre", "code",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"ul", "ol", "li", "dl", "dt", "dd",
		"strong", "b", "em", "i", "u", "s", "sub", "sup", "mark", "small",
		"table", "thead", "tbody", "tr", "th", "td", "caption", "colgroup", "col",
		"figure", "figcaption", "picture", "source", "details", "summary", "abbr", "kbd")

	// Links: safe schemes only, target/rel set by the client.
	p.AllowAttrs("href", "title").OnElements("a")
	p.AllowURLSchemes("http", "https", "mailto")
	p.AllowRelativeURLs(true)

	// Images: explicit src, informative alt/title.
	p.AllowAttrs("src", "alt", "title", "width", "height", "loading").OnElements("img")
	p.AllowAttrs("src", "srcset", "media").OnElements("source")

	// Basic embedded media (third-party podcasts/video stay as
	// enclosure in the contract, here only direct audio/video).
	p.AllowAttrs("src", "controls", "preload").OnElements("audio", "video")
	p.AllowAttrs("src").OnElements("track")

	// Code: languages as a restricted class value.
	p.AllowAttrs("class").Matching(bluemonday.SpaceSeparatedTokens).OnElements("code", "pre", "span")

	policy = p
}

// Body cleans the HTML of an article body. Empty in, empty out.
func Body(html string) string {
	if html == "" {
		return ""
	}
	return policy.Sanitize(html)
}
