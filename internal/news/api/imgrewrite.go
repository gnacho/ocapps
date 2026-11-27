package api

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/gnacho/ocapps/internal/news/privacy"
	"github.com/gnacho/ocapps/internal/news/store"
)

var imgSrcRe = regexp.MustCompile(`src="(https?://[^"]+)"`)

// linkHygiene reads the link hygiene level of the requesting user. Empty or on
// error means tracking, which is the default and has no false positives.
func (s *Server) linkHygiene(r *http.Request) privacy.Level {
	u := user(r)
	if u == nil {
		return privacy.LevelTracking
	}
	v, err := s.store.GetUserSetting(u.ID, "link_hygiene")
	if err != nil {
		return privacy.LevelTracking
	}
	lvl, ok := privacy.ValidLevel(v)
	if !ok {
		return privacy.LevelTracking
	}
	return lvl
}

// ownerLinkHygiene resolves the level of the owner of a feed: the public page
// of a shared article is cleaned with the level of whoever shared it, not with
// the visitor's.
func (s *Server) ownerLinkHygiene(feedID int64) privacy.Level {
	f, err := s.store.GetFeedByID(feedID)
	if err != nil || f == nil {
		return privacy.LevelTracking
	}
	v, err := s.store.GetUserSetting(f.UserID, "link_hygiene")
	if err != nil {
		return privacy.LevelTracking
	}
	lvl, ok := privacy.ValidLevel(v)
	if !ok {
		return privacy.LevelTracking
	}
	return lvl
}

// rewriteBody prepares an article body for the client: link hygiene first (the
// href no longer carry tracking or intermediaries) and then the image proxy,
// in that order because cleaning href does not touch src.
func (s *Server) rewriteBody(body string, level privacy.Level) string {
	return s.rewriteImages(privacy.CleanLinks(body, level))
}

// rewriteImages transforms the http(s) src of the body towards the signed proxy
// (the host CSP blocks external images, the browser can load
// from its own domain). The HTML is already sanitized: the src attributes are
// clean URLs. The & of the query is escaped as &amp; for valid HTML.
// u travels with url.QueryEscape (it escapes % -> %25): q.Get() of the proxy recovers
// the EXACT URL that was signed (with its %2C etc. intact).
func (s *Server) rewriteImages(body string) string {
	if body == "" || s.imgs == nil {
		return body
	}
	return imgSrcRe.ReplaceAllStringFunc(body, func(m string) string {
		sub := imgSrcRe.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		// the sanitized HTML stores entities in attributes (&amp;): the real
		// URL that is signed and fetched is the unescaped one
		u := html.UnescapeString(sub[1])
		return `src="` + Base + `/img?u=` + url.QueryEscape(u) +
			`&amp;t=` + s.imgs.Sign(u) + `"`
	})
}

// rewriteAll applies the proxy to all the items of a response: bodies
// (images and embedded media) and audio/video/image enclosures.
func (s *Server) rewriteAll(items []store.Item, level privacy.Level) {
	for i := range items {
		items[i].Body = s.rewriteBody(items[i].Body, level)
		if items[i].EnclosureLink != nil && items[i].EnclosureMime != nil {
			m := *items[i].EnclosureMime
			if s.mediaMime(m) && strings.HasPrefix(*items[i].EnclosureLink, "http") {
				u := html.UnescapeString(*items[i].EnclosureLink)
				proxied := Base + "/img?u=" + url.QueryEscape(u) + "&t=" + s.imgs.Sign(u)
				items[i].EnclosureLink = &proxied
			}
		}
	}
}

func (s *Server) mediaMime(m string) bool {
	return strings.HasPrefix(m, "audio/") || strings.HasPrefix(m, "video/") ||
		strings.HasPrefix(m, "image/")
}

// rewriteFavicon points to the public favicon endpoint served from the backend
// cache (/favicon/{urlHash}) instead of signing the origin URL. This way the
// cache is the only source (it is not re-fetched per instance) and the browser
// <img> does not need auth (the route is public, it only reads from disk).
func (s *Server) rewriteFavicon(urlHash string) string {
	if urlHash == "" || s.favicons == nil {
		return ""
	}
	return Base + "/favicon/" + urlHash
}
