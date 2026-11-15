package api

import (
	"fmt"
	"testing"
)

// TestFeedRulesAPI: store/read/delete feed rules + 422 validation.
func TestFeedRulesAPI(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	_, body := e.do(t, "POST", "/feeds", e.user, e.pass, map[string]string{"url": "https://a.example/f"})
	var feedResp struct {
		Feeds []struct{ ID int64 }
	}
	decode(t, body, &feedResp)
	if len(feedResp.Feeds) != 1 {
		t.Fatalf("the feed was not created: %s", body)
	}
	feedID := feedResp.Feeds[0].ID

	// invalid rule -> 422
	code, _ := e.do(t, "POST", fmt.Sprintf("/feeds/%d/rules", feedID), e.user, e.pass,
		map[string]string{"block": "EntryTitle=("})
	if code != 422 {
		t.Fatalf("invalid regex expected 422, I have %d", code)
	}

	// store a valid rule
	code, body = e.do(t, "POST", fmt.Sprintf("/feeds/%d/rules", feedID), e.user, e.pass,
		map[string]string{"block": "EntryTitle=(?i)oferta\nEntryDate=future", "keep": "EntryAuthor=(?i)luis"})
	if code != 200 {
		t.Fatalf("store rules: %d %s", code, body)
	}
	var got struct {
		Rules struct {
			Block string `json:"block"`
			Keep  string `json:"keep"`
		} `json:"rules"`
	}
	decode(t, body, &got)
	if got.Rules.Block == "" || got.Rules.Keep == "" {
		t.Errorf("stored rules empty: %s", body)
	}

	// read
	code, _ = e.do(t, "GET", fmt.Sprintf("/feeds/%d/rules", feedID), e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("read rules: %d", code)
	}

	// delete
	code, _ = e.do(t, "DELETE", fmt.Sprintf("/feeds/%d/rules", feedID), e.user, e.pass, nil)
	if code != 204 {
		t.Fatalf("delete rules: %d", code)
	}
	_, body = e.do(t, "GET", fmt.Sprintf("/feeds/%d/rules", feedID), e.user, e.pass, nil)
	decode(t, body, &got)
	if got.Rules.Block != "" || got.Rules.Keep != "" {
		t.Errorf("rules not deleted: %s", body)
	}
}

// TestGlobalRulesAPI: PUT/GET /api/me/rules validates and persists.
func TestGlobalRulesAPI(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	code, _ := e.do(t, "PUT", "/api/me/rules", e.user, e.pass,
		map[string]string{"block": "EntryTitle=("})
	if code != 422 {
		t.Fatalf("invalid global rule expected 422, I have %d", code)
	}
	code, body := e.do(t, "PUT", "/api/me/rules", e.user, e.pass,
		map[string]string{"keep": "EntryContent=(?i)terraform"})
	if code != 200 {
		t.Fatalf("store global rules: %d %s", code, body)
	}
	var got struct {
		Rules struct{ Keep string }
	}
	decode(t, body, &got)
	if got.Rules.Keep == "" {
		t.Errorf("global keep not stored: %s", body)
	}
	code, body = e.do(t, "GET", "/api/me/rules", e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("read global rules: %d", code)
	}
	decode(t, body, &got)
	if got.Rules.Keep == "" {
		t.Errorf("global keep not read: %s", body)
	}
}
