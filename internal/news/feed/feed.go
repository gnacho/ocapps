// Package feed fetches and normalizes RSS/Atom feeds into the store models.
// Fetcher is an interface to inject fakes in tests and in the API.
package feed

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"net/http"
	urlpkg "net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/gnacho/ocapps/internal/common/netguard"
	"github.com/gnacho/ocapps/internal/news/privacy"
	"github.com/gnacho/ocapps/internal/news/sanitize"
	"github.com/gnacho/ocapps/internal/news/store"
)

const maxBodyBytes = 10 << 20 // 10 MB cap per feed

type Fetcher interface {
	// Fetch downloads and parses the feed, error if it cannot be read.
	// authUser/authPass: feed HTTP Basic credentials (empty = no auth).
	Fetch(ctx context.Context, url, authUser, authPass string) (*store.Feed, []store.NewItem, error)
	// Discover autodetects RSS/Atom feeds on a web page.
	Discover(ctx context.Context, url string) ([]DiscoveredFeed, error)
}

// ErrAuthRequired: the origin responded 401/403 (feed behind authentication).
var ErrAuthRequired = errors.New("the feed requires authentication")

type HTTPFetcher struct {
	Client *http.Client
}

func NewHTTPFetcher(timeout time.Duration) *HTTPFetcher {
	return &HTTPFetcher{Client: netguard.Client(timeout)}
}

// NewHTTPFetcherAllowLocal creates a fetcher whose transport allows loopback.
// TESTS only (httptest listens on 127.0.0.1).
func NewHTTPFetcherAllowLocal(timeout time.Duration) *HTTPFetcher {
	return &HTTPFetcher{Client: netguard.ClientAllowLocal(timeout)}
}

func (h *HTTPFetcher) Fetch(ctx context.Context, url, authUser, authPass string) (*store.Feed, []store.NewItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid url: %w", err)
	}
	req.Header.Set("User-Agent", "ocnews/0.5 (+https://github.com/gnacho/ocnews)")
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, text/xml, */*")
	if authUser != "" {
		req.SetBasicAuth(authUser, authPass)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("download feed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, nil, fmt.Errorf("download feed: HTTP %d: %w", resp.StatusCode, ErrAuthRequired)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("download feed: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("read feed: %w", err)
	}
	return Parse(body)
}

// minFullText: plain text threshold to consider that an item arrives
// complete from the feed (typical summaries stay at 100-600 chars).
const minFullText = 900

// HasFullContent says whether ANY item of the batch already brings the whole
// article (heuristic: enough plain text). It is evaluated BEFORE sanitizing.
func HasFullContent(items []store.NewItem) bool {
	for _, it := range items {
		if len(plainTextLen(it.Body)) >= minFullText {
			return true
		}
	}
	return false
}

// plainTextLen: approximate text length (tags out).
func plainTextLen(html string) string {
	var b strings.Builder
	inTag := false
	for _, r := range html {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// SanitizeItems cleans the HTML of the bodies of a batch of items.
// ONLY entry path to the DB: the subscription (API) and the
// refresher (scheduler) call it, never persist bodies without going through here.
func SanitizeItems(items []store.NewItem) {
	for i := range items {
		items[i].Body = sanitize.Body(items[i].Body)
	}
}

// Parse normalizes an RSS/Atom document (separable from the network for tests).
func Parse(body []byte) (*store.Feed, []store.NewItem, error) {
	fp := gofeed.NewParser()
	parsed, err := fp.ParseString(string(body))
	if err != nil {
		return nil, nil, fmt.Errorf("parse feed: %w", err)
	}

	f := &store.Feed{
		URL:   parsed.FeedLink,
		Title: strings.TrimSpace(parsed.Title),
		Link:  websiteLink(parsed),
		Added: time.Now().Unix(),
		Hub:   hubFromBody(body),
	}
	if f.Title == "" {
		f.Title = "feed without title"
	}

	items := make([]store.NewItem, 0, len(parsed.Items))
	for _, it := range parsed.Items {
		guid := it.GUID
		if guid == "" {
			guid = it.Link
		}
		if guid == "" {
			continue // without an identifier it is not deduplicable
		}
		pubDate := time.Now().Unix()
		if it.PublishedParsed != nil {
			pubDate = it.PublishedParsed.Unix()
		} else if it.UpdatedParsed != nil {
			pubDate = it.UpdatedParsed.Unix()
		}
		body := it.Content
		if body == "" {
			body = it.Description
		}
		ni := store.NewItem{
			GUID:     guid,
			GUIDHash: hashGUID(guid),
			URL:      it.Link,
			Title:    strings.TrimSpace(it.Title),
			Author:   authorOf(it),
			PubDate:  pubDate,
			Body:     body,
		}
		if len(it.Enclosures) > 0 {
			mime, link := it.Enclosures[0].Type, it.Enclosures[0].URL
			ni.EnclosureMime, ni.EnclosureLink = &mime, &link
		}
		if thumb, ok := mediaExt(it, "thumbnail", "url"); ok {
			ni.MediaThumbnail = &thumb
		}
		if desc, ok := mediaExt(it, "description", ""); ok {
			ni.MediaDescription = &desc
		}
		applyPrivacy(&ni)
		ni.Fingerprint = fingerprint(ni)
		ni.ClusterKey = clusterKey(ni)
		items = append(items, ni)
	}
	return f, items, nil
}

// clusterKey groups the same story across several feeds: hash over the title
// and the beginning of the normalized body (without URL, which differs per site).
func clusterKey(ni store.NewItem) string {
	title := normalizeText(ni.Title)
	body := normalizeText(plainTextLen(ni.Body))
	if len(body) > 300 {
		body = body[:300]
	}
	sum := md5.Sum([]byte(title + "\x00" + body))
	return fmt.Sprintf("%x", sum)
}

// normalizeText: lowercase and collapsed whitespace to compare titles.
func normalizeText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// applyPrivacy cleans tracking parameters and pixel trackers from the item BEFORE
// the fingerprint (the URL and the body enter the hash already clean).
func applyPrivacy(ni *store.NewItem) {
	ni.URL = privacy.StripParams(ni.URL)
	if ni.EnclosureLink != nil {
		v := privacy.StripParams(*ni.EnclosureLink)
		ni.EnclosureLink = &v
	}
	if ni.MediaThumbnail != nil {
		v := privacy.StripParams(*ni.MediaThumbnail)
		ni.MediaThumbnail = &v
	}
	ni.Body = privacy.RemovePixels(ni.Body)
}

func websiteLink(f *gofeed.Feed) string {
	switch {
	case f.Link != "":
		return f.Link
	case len(f.Links) > 0:
		return f.Links[0]
	case f.FeedLink != "":
		return f.FeedLink
	}
	return ""
}

func authorOf(it *gofeed.Item) string {
	if it.Author != nil {
		return strings.TrimSpace(it.Author.Name)
	}
	for _, a := range it.Authors {
		if a != nil && a.Name != "" {
			return strings.TrimSpace(a.Name)
		}
	}
	return ""
}

// mediaExt extracts media:* extensions (RSS with media namespaces).
func mediaExt(it *gofeed.Item, name, attr string) (string, bool) {
	exts, ok := it.Extensions["media"]
	if !ok {
		return "", false
	}
	list, ok := exts[name]
	if !ok || len(list) == 0 {
		return "", false
	}
	if attr == "" {
		if list[0].Value == "" {
			return "", false
		}
		return list[0].Value, true
	}
	v, ok := list[0].Attrs[attr]
	return v, ok && v != ""
}

func hashGUID(guid string) string {
	sum := md5.Sum([]byte(guid))
	return fmt.Sprintf("%x", sum)
}

// fingerprint: hash over title+body+url+enclosure (dedupe across feeds).
func fingerprint(ni store.NewItem) string {
	enc := ""
	if ni.EnclosureLink != nil {
		enc = *ni.EnclosureLink
	}
	sum := md5.Sum([]byte(ni.Title + "\x00" + ni.Body + "\x00" + ni.URL + "\x00" + enc))
	return fmt.Sprintf("%x", sum)
}

// DiscoveredFeed: a candidate feed detected on the page of a site.
type DiscoveredFeed struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Type  string `json:"type"` // rss | atom
}

var feedLinkRe = regexp.MustCompile(`(?i)<link[^>]*rel=["']\s*alternate\s*["'][^>]*>`)
var hubLinkRe = regexp.MustCompile(`(?i)<link[^>]*rel=["']\s*hub\s*["'][^>]*>`)

// hubFromBody extracts the first rel="hub" href of the document (WebSub, #44).
func hubFromBody(body []byte) string {
	for _, m := range hubLinkRe.FindAll(body, -1) {
		if href := linkAttr(string(m), "href"); href != "" {
			return href
		}
	}
	return ""
}

// Discover downloads a page and detects its RSS/Atom feeds (autodiscovery).
// It downloads ONCE: it first tries to parse the body as a feed, if it
// is not a feed, it extracts the <link rel="alternate" type="rss|atom"> from the HTML.
func (h *HTTPFetcher) Discover(ctx context.Context, url string) ([]DiscoveredFeed, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	req.Header.Set("User-Agent", "ocnews/0.5 (+https://github.com/gnacho/ocnews)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/rss+xml,application/atom+xml,application/xml,*/*")
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}

	// Is it a feed? We try it first to avoid scraping HTML.
	if f, items, err := Parse(body); err == nil {
		_ = items
		return []DiscoveredFeed{{URL: url, Title: f.Title, Type: feedKind(f.URL)}}, nil
	}

	// It is not a feed: extract the feeds from the <link rel="alternate">.
	base, _ := urlpkg.Parse(url)
	out := []DiscoveredFeed{}
	seen := map[string]bool{}
	for _, m := range feedLinkRe.FindAll(body, -1) {
		link := linkAttr(string(m), "href")
		typ := strings.ToLower(linkAttr(string(m), "type"))
		if link == "" {
			continue
		}
		if !strings.Contains(typ, "rss") && !strings.Contains(typ, "atom") && !strings.HasSuffix(link, ".xml") {
			continue
		}
		abs, err := base.Parse(link)
		if err != nil {
			continue
		}
		kind := "rss"
		if strings.Contains(typ, "atom") {
			kind = "atom"
		}
		if seen[abs.String()] {
			continue
		}
		seen[abs.String()] = true
		out = append(out, DiscoveredFeed{URL: privacy.StripParams(abs.String()), Title: titleFromFeedLink(string(m)), Type: kind})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no feeds found on the page")
	}
	return out, nil
}

func feedKind(u string) string {
	if strings.Contains(strings.ToLower(u), "atom") {
		return "atom"
	}
	return "rss"
}

func linkAttr(tag, attr string) string {
	re := regexp.MustCompile(`(?i)` + attr + `\s*=\s*["']([^"']*)["']`)
	m := re.FindStringSubmatch(tag)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

func titleFromFeedLink(tag string) string {
	if t := linkAttr(tag, "title"); t != "" {
		return t
	}
	return ""
}
