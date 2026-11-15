package extract

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestArticleWithSelector(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><div id="articleBody"><h1>Title</h1><p>Lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua. Ut enim ad minim veniam quis nostrud exercitation ullamco laboris.</p><p>Second paragraph with more filler content to go over the minimum character threshold of the extraction and make sure the validation passes without problems.</p><p>Third additional paragraph to give room to the total length of the plain text extracted from the selector.</p></div></body></html>`))
	}))
	defer ts.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_ = log
	e := NewAllowLocal(5 * time.Second)
	body, err := e.Article(context.Background(), ts.URL, "div#articleBody")
	if err != nil {
		t.Fatalf("Article with selector: %v", err)
	}
	if !strings.Contains(body, "Title") {
		t.Errorf("body without the expected content: %s", body)
	}
}

func TestArticleSelectorFallbackToReadability(t *testing.T) {
	// page without the selector: it must fall back to readability (which with little text
	// returns an error or content, here we only check that it does not panic)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><main><p>Short content.</p></main></body></html>`))
	}))
	defer ts.Close()

	e := NewAllowLocal(5 * time.Second)
	_, _ = e.Article(context.Background(), ts.URL, "div#does-not-exist")
}

func TestArticleSelectorInvalid(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><p>x</p></body></html>`))
	}))
	defer ts.Close()
	e := NewAllowLocal(5 * time.Second)
	// selector without match -> fallback to readability -> content too short
	_, err := e.Article(context.Background(), ts.URL, "div.that.does.not.exist")
	if err == nil {
		t.Error("expected an error (extraction too short)")
	}
}
