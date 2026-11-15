package api

import (
	"fmt"
	"testing"
)

func TestFeedScraperAPI(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	_, body := e.do(t, "POST", "/feeds", e.user, e.pass, map[string]string{"url": "https://sc.example/f"})
	var feedResp struct {
		Feeds []struct{ ID int64 }
	}
	decode(t, body, &feedResp)
	if len(feedResp.Feeds) != 1 {
		t.Fatalf("feed not created: %s", body)
	}
	feedID := feedResp.Feeds[0].ID

	// invalid selector -> 422
	if code, _ := e.do(t, "POST", fmt.Sprintf("/feeds/%d/scraper", feedID), e.user, e.pass,
		map[string]string{"scraperSelector": "div["}); code != 422 {
		t.Fatalf("invalid selector expected 422, I have %d", code)
	}

	// valid selector -> 200 and persisted
	code, body := e.do(t, "POST", fmt.Sprintf("/feeds/%d/scraper", feedID), e.user, e.pass,
		map[string]string{"scraperSelector": "div#articleBody, article"})
	if code != 200 {
		t.Fatalf("store selector: %d %s", code, body)
	}
	var got struct {
		ScraperSelector string `json:"scraperSelector"`
	}
	decode(t, body, &got)
	if got.ScraperSelector != "div#articleBody, article" {
		t.Errorf("selector not persisted: %s", body)
	}

	// empty = remove
	code, body = e.do(t, "POST", fmt.Sprintf("/feeds/%d/scraper", feedID), e.user, e.pass,
		map[string]string{"scraperSelector": ""})
	if code != 200 {
		t.Fatalf("remove selector: %d", code)
	}
	decode(t, body, &got)
	if got.ScraperSelector != "" {
		t.Errorf("selector not removed: %s", body)
	}
}
