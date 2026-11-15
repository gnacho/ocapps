package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// articleServer serves a page with a long body and nav/footer noise.
func articleServer(t *testing.T) *httptest.Server {
	t.Helper()
	long := "<p>" + strings.Repeat("Substantial article text. ", 40) + "</p>"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Article</title></head><body>
<nav>menu menu</nav>
<article><h1>Real title</h1>` + long + `</article>
<footer>footer</footer></body></html>`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestItemFullExtractsAndCaches(t *testing.T) {
	ts := articleServer(t)
	e := newTestEnv(t, &fakeFetcher{})

	// feed + items through the API, then point the URL of the newest item to the article
	e.do(t, "POST", "/feeds", e.user, e.pass, map[string]string{"url": "https://feed.example/rss"})
	var itemsResp struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	_, body := e.do(t, "GET", "/items?type=3&getRead=true", e.user, e.pass, nil)
	decode(t, body, &itemsResp)
	itemID := itemsResp.Items[0].ID

	// rewrite the item URL to the local article (the real extractor fetches it)
	if err := e.srv.store.SetItemURLForTesting(itemID, ts.URL+"/post"); err != nil {
		t.Fatal(err)
	}

	code, body := e.do(t, "GET", "/items/"+itoa(itemID)+"/full", e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("full: %d %s", code, body)
	}
	if !strings.Contains(string(body), "Substantial") {
		t.Fatalf("the full body did not arrive: %s", body[:min(200, len(body))])
	}
	if strings.Contains(string(body), "<nav>") || strings.Contains(string(body), "<script") {
		t.Fatal("the extracted body must be sanitized and without page chrome")
	}

	// second call: served from cache (an invalid URL would prove it now:
	// we change the URL to something unreachable and it still works from cache)
	e.srv.store.SetItemURLForTesting(itemID, "http://127.0.0.1:1/dead")
	code, body = e.do(t, "GET", "/items/"+itoa(itemID)+"/full", e.user, e.pass, nil)
	if code != 200 || !strings.Contains(string(body), "Substantial") {
		t.Fatalf("full cache: %d", code)
	}
}

func TestItemFullUnextractable(t *testing.T) {
	// minimal page: extraction < minTextLen -> 422 with a stable code
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><p>short</p></body></html>`))
	}))
	t.Cleanup(ts.Close)

	e := newTestEnv(t, &fakeFetcher{})
	e.do(t, "POST", "/feeds", e.user, e.pass, map[string]string{"url": "https://feed2.example/rss"})
	var itemsResp struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	_, body := e.do(t, "GET", "/items?type=3&getRead=true", e.user, e.pass, nil)
	decode(t, body, &itemsResp)
	e.srv.store.SetItemURLForTesting(itemsResp.Items[0].ID, ts.URL+"/x")

	code, body := e.do(t, "GET", "/items/"+itoa(itemsResp.Items[0].ID)+"/full", e.user, e.pass, nil)
	if code != 422 || !strings.Contains(string(body), "full_unavailable") {
		t.Fatalf("unextractable: %d %s", code, body)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
