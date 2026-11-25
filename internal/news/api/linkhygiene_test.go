package api

import (
	"io"
	"strings"
	"testing"

	"github.com/gnacho/ocapps/internal/news/store"
)

// TestLinkHygieneSetting: the level persists in user_settings through
// /api/me/settings, it validates, and the default (tracking) is stored empty.
func TestLinkHygieneSetting(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})

	// invalid -> 422
	if code, _ := e.do(t, "PUT", "/api/me/settings", e.user, e.pass,
		map[string]string{"linkHygiene": "maximo"}); code != 422 {
		t.Fatalf("invalid level expected 422, tengo %d", code)
	}

	var got struct {
		LinkHygiene string `json:"linkHygiene"`
	}

	// untouched default: tracking
	if code, body := e.do(t, "GET", "/api/me/settings", e.user, e.pass, nil); code != 200 {
		t.Fatalf("read settings: %d", code)
	} else {
		decode(t, body, &got)
		if got.LinkHygiene != "tracking" {
			t.Fatalf("default should be tracking: %s", body)
		}
	}

	// aggressive persists
	if code, body := e.do(t, "PUT", "/api/me/settings", e.user, e.pass,
		map[string]string{"linkHygiene": "aggressive"}); code != 200 {
		t.Fatalf("save aggressive: %d %s", code, body)
	}
	code, body := e.do(t, "GET", "/api/me/settings", e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("read settings: %d", code)
	}
	decode(t, body, &got)
	if got.LinkHygiene != "aggressive" {
		t.Fatalf("level not persisted: %s", body)
	}

	// going back to tracking stores the default and still reads back
	if code, _ := e.do(t, "PUT", "/api/me/settings", e.user, e.pass,
		map[string]string{"linkHygiene": "tracking"}); code != 200 {
		t.Fatalf("reset to tracking: %d", code)
	}
	body = func() []byte {
		code, b := e.do(t, "GET", "/api/me/settings", e.user, e.pass, nil)
		if code != 200 {
			t.Fatalf("read settings: %d", code)
		}
		return b
	}()
	decode(t, body, &got)
	if got.LinkHygiene != "tracking" {
		t.Fatalf("tracking not read as default: %s", body)
	}
}

// TestItemsServeCleanedLinks: body href leave clean according to the user
// level, and the aggressive level also unwraps the intermediary.
func TestItemsServeCleanedLinks(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	st := e.srv.store
	uid, _ := st.GetUserByUsername(e.user)
	body := `<p><a href="https://pub.example/a?utm_source=rss&amp;id=7">uno</a></p>` +
		`<p><a href="https://go.medio.example/out?url=https%3A%2F%2Fdestino.example%2Fnota%3Futm_campaign%3Dx">dos</a></p>`
	if _, err := st.CreateFeedFull(uid.ID, "https://feed.example/rss", nil, "Feed", "https://feed.example",
		"", []store.NewItem{{GUID: "g1", GUIDHash: "h1", URL: "https://feed.example/1", Title: "Item", PubDate: 1, Body: body}}, false); err != nil {
		t.Fatal(err)
	}

	fetch := func() string {
		t.Helper()
		code, raw := e.do(t, "GET", "/items?type=3&getRead=true", e.user, e.pass, nil)
		if code != 200 {
			t.Fatalf("items: %d %s", code, raw)
		}
		var resp struct {
			Items []struct {
				Body string `json:"body"`
			} `json:"items"`
		}
		decode(t, raw, &resp)
		if len(resp.Items) != 1 {
			t.Fatalf("expected 1 item, got %d", len(resp.Items))
		}
		return resp.Items[0].Body
	}

	// default (tracking): the utm goes, the intermediary stays
	got := fetch()
	if strings.Contains(got, "utm_source") {
		t.Errorf("tracking not removed: %s", got)
	}
	if !strings.Contains(got, "go.medio.example") {
		t.Errorf("tracking must not unwrap the intermediary: %s", got)
	}
	if !strings.Contains(got, "id=7") {
		t.Errorf("legitimate query lost: %s", got)
	}

	// aggressive: it also unwraps, and the destination loses tracking
	if code, _ := e.do(t, "PUT", "/api/me/settings", e.user, e.pass,
		map[string]string{"linkHygiene": "aggressive"}); code != 200 {
		t.Fatalf("save aggressive: %d", code)
	}
	got = fetch()
	if strings.Contains(got, "go.medio.example") {
		t.Errorf("intermediary not unwrapped: %s", got)
	}
	if !strings.Contains(got, "destino.example/nota") {
		t.Errorf("destination not applied: %s", got)
	}
	if strings.Contains(got, "utm_campaign") {
		t.Errorf("destination tracking not removed: %s", got)
	}

	// off: HTML as is except for the image proxy
	if code, _ := e.do(t, "PUT", "/api/me/settings", e.user, e.pass,
		map[string]string{"linkHygiene": "off"}); code != 200 {
		t.Fatalf("guardar off: %d", code)
	}
	got = fetch()
	if !strings.Contains(got, "utm_source") || !strings.Contains(got, "go.medio.example") {
		t.Errorf("off should leave links untouched: %s", got)
	}
}

// TestSharePageUsesOwnerLinkHygiene: the public page of a shared article is
// cleaned with the item OWNER setting, not the visitor's.
func TestSharePageUsesOwnerLinkHygiene(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	st := e.srv.store
	uid, _ := st.GetUserByUsername(e.user)
	body := `<p><a href="https://pub.example/a?utm_source=rss">uno</a></p>`
	f, err := st.CreateFeedFull(uid.ID, "https://feed.example/rss", nil, "Feed", "https://feed.example",
		"", []store.NewItem{{GUID: "g1", GUIDHash: "h1", URL: "https://feed.example/1", Title: "Item", PubDate: 1, Body: body}}, false)
	if err != nil {
		t.Fatal(err)
	}
	items, err := st.ListItems(store.ItemFilter{UserID: uid.ID, Type: 3, GetRead: true, BatchSize: -1})
	if err != nil || len(items) == 0 {
		t.Fatalf("no items: %v", err)
	}
	token, err := st.CreateShare(uid.ID, items[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.ID == 0 {
		t.Fatal("feed without id")
	}

	resp, err := e.client.Get(e.ts.URL + Base + "/share/" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	if strings.Contains(page, "utm_source") {
		t.Errorf("the shared page did not clean the owner tracking: %s", page)
	}
	if !strings.Contains(page, "pub.example/a") {
		t.Errorf("link lost in the shared page: %s", page)
	}
}
