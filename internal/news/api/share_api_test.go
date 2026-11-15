package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestShareAPI(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	_, body := e.do(t, "POST", "/feeds", e.user, e.pass, map[string]string{"url": "https://sh.example/f"})
	var feedResp struct {
		Feeds []struct{ ID int64 }
	}
	decode(t, body, &feedResp)
	if len(feedResp.Feeds) != 1 {
		t.Fatalf("feed not created: %s", body)
	}

	// get an item from the feed
	_, body = e.do(t, "GET", "/items?type=0&id="+fmt.Sprint(feedResp.Feeds[0].ID), e.user, e.pass, nil)
	var items struct {
		Items []struct {
			ID    int64
			Title string
		}
	}
	decode(t, body, &items)
	if len(items.Items) == 0 {
		t.Fatalf("no items: %s", body)
	}
	itemID := items.Items[0].ID

	code, body := e.do(t, "POST", fmt.Sprintf("/items/%d/share", itemID), e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("create share: %d %s", code, body)
	}
	var share struct {
		Share struct {
			Token string `json:"token"`
			URL   string `json:"url"`
		} `json:"share"`
	}
	decode(t, body, &share)
	if share.Share.Token == "" || share.Share.URL == "" {
		t.Fatalf("share incompleto: %s", body)
	}

	// the public URL must serve HTML without auth
	resp, err := e.client.Get(e.ts.URL + Base + "/share/" + share.Share.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public view: %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("public view content-type: %q", ct)
	}

	// delete share -> 204 and the public view turns 404
	if code, _ := e.do(t, "DELETE", fmt.Sprintf("/items/%d/share", itemID), e.user, e.pass, nil); code != 204 {
		t.Fatalf("delete share: %d", code)
	}
	resp2, err := e.client.Get(e.ts.URL + Base + "/share/" + share.Share.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("public view after delete expected 404, I have %d", resp2.StatusCode)
	}
}
