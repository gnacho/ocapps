// Package extract: fetches the full body of an article from its original URL
// (feeds usually only carry the summary). It uses readability and returns
// HTML already sanitized by the SAME policy as feed bodies.
package extract

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/go-shiori/go-readability"

	"github.com/gnacho/ocapps/internal/common/netguard"
	"github.com/gnacho/ocapps/internal/news/sanitize"
)

const (
	maxArticleBytes = 5 << 20 // 5 MB of HTML
	minTextLen      = 250     // less than this = failed extraction
	userAgent       = "Mozilla/5.0 (compatible; ocnews/0.5; +https://github.com/gnacho/ocnews)"
)

type Extractor struct {
	Client *http.Client
}

func New(timeout time.Duration) *Extractor {
	return &Extractor{Client: netguard.Client(timeout)}
}

// NewAllowLocal is like New but the transport allows loopback. TESTS only.
func NewAllowLocal(timeout time.Duration) *Extractor {
	return &Extractor{Client: netguard.ClientAllowLocal(timeout)}
}

// Article downloads the page and extracts the main content.
// If selector is not empty, it tries to extract that CSS element first
// (per-feed rule) and falls back to readability if the selector fails.
// It returns ("", error) on failure, the HTML is sanitized.
func (e *Extractor) Article(ctx context.Context, rawURL, selector string) (string, error) {
	if selector != "" {
		if body, err := e.articleBySelector(ctx, rawURL, selector); err == nil {
			return body, nil
		}
	}
	return e.articleByReadability(ctx, rawURL)
}

// articleBySelector extracts the first element matching the CSS selector.
func (e *Extractor) articleBySelector(ctx context.Context, rawURL, selector string) (string, error) {
	resp, err := e.fetch(ctx, rawURL)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, maxArticleBytes))
	if err != nil {
		return "", fmt.Errorf("parse with selector: %w", err)
	}
	sel := doc.Find(selector).First()
	if sel.Length() == 0 {
		return "", fmt.Errorf("selector %q without matches", selector)
	}
	text := strings.TrimSpace(sel.Text())
	if len(text) < minTextLen {
		return "", fmt.Errorf("selector %q too short (%d chars)", selector, len(text))
	}
	html, err := sel.Html()
	if err != nil {
		return "", err
	}
	return sanitize.Body(html), nil
}

// articleByReadability uses go-readability (default fallback).
func (e *Extractor) articleByReadability(ctx context.Context, rawURL string) (string, error) {
	resp, err := e.fetch(ctx, rawURL)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	article, err := readability.FromReader(io.LimitReader(resp.Body, maxArticleBytes), parsed)
	if err != nil {
		return "", fmt.Errorf("extraer: %w", err)
	}
	text := strings.TrimSpace(article.TextContent)
	if len(text) < minTextLen {
		return "", fmt.Errorf("extraction too short (%d chars)", len(text))
	}
	return sanitize.Body(article.Content), nil
}

// fetch downloads an article URL with the ocnews User-Agent.
func (e *Extractor) fetch(ctx context.Context, rawURL string) (*http.Response, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*")
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download article: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("article: HTTP %d", resp.StatusCode)
	}
	return resp, nil
}
