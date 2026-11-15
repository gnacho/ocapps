package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/cred"
	"github.com/gnacho/ocapps/internal/common/imgproxy"
	"github.com/gnacho/ocapps/internal/news/auth"
	"github.com/gnacho/ocapps/internal/news/extract"
	"github.com/gnacho/ocapps/internal/news/favicon"
	"github.com/gnacho/ocapps/internal/news/feed"
	"github.com/gnacho/ocapps/internal/news/refresher"
	"github.com/gnacho/ocapps/internal/news/store"
)

// testCipher: cipher with an ephemeral feedsecret in a temp dir (common/cred,
// it replaces the old cred.Load of ocnews).
func testCipher(t *testing.T) *cred.Cipher {
	t.Helper()
	key, err := cred.LoadOrCreateSecret(filepath.Join(t.TempDir(), "feedsecret"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := cred.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fakeFetcher serves canned feeds by URL, refreshes counts the calls
// to simulate the feed publishing new items.
type fakeFetcher struct {
	fetches  int
	failURLs map[string]bool
	// credentials received in the last call (feed auth tests)
	lastAuthUser string
	lastAuthPass string
}

func (f *fakeFetcher) Fetch(_ context.Context, url, authUser, authPass string) (*store.Feed, []store.NewItem, error) {
	f.fetches++
	f.lastAuthUser, f.lastAuthPass = authUser, authPass
	if f.failURLs != nil && f.failURLs[url] {
		return nil, nil, fmt.Errorf("feed roto (fake)")
	}
	fd := &store.Feed{Title: "Feed " + url, Link: "https://site.example"}
	base := len(url)
	if f.fetches > 1 {
		base++ // after a refresh an extra item shows up
	}
	items := make([]store.NewItem, 0, base)
	for i := 0; i < base; i++ {
		guid := fmt.Sprintf("%s#%d", url, i)
		items = append(items, store.NewItem{
			GUID: guid, GUIDHash: fmt.Sprintf("h%d", i),
			URL:     "https://site.example/" + fmt.Sprint(i),
			Title:   fmt.Sprintf("Item %d of %s", i, url),
			PubDate: int64(1000 + i),
		})
	}
	return fd, items, nil
}

func (f *fakeFetcher) Discover(_ context.Context, url string) ([]feed.DiscoveredFeed, error) {
	if f.failURLs != nil && f.failURLs[url] {
		return nil, fmt.Errorf("discover roto (fake)")
	}
	return []feed.DiscoveredFeed{{URL: url + "/rss", Title: "Feed " + url, Type: "rss"}}, nil
}

type testEnv struct {
	ts         *httptest.Server
	client     *http.Client
	user       string
	pass       string
	faviconDir string
	srv        *Server
}

func newTestEnv(t *testing.T, fetcher feed.Fetcher) *testEnv {
	t.Helper()
	st, err := store.Open(t.TempDir() + string(os.PathSeparator) + "test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	hash, err := auth.HashPassword("pass1234")
	if err != nil {
		t.Fatal(err)
	}
	nachoID, err := st.CreateUser("nacho", hash, "Nacho", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserLanguage(nachoID, "es"); err != nil {
		t.Fatal(err)
	}
	hash2, _ := auth.HashPassword("other1234")
	if _, err := st.CreateUser("other", hash2, "Other", "user"); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	creds := testCipher(t)
	refresh := refresher.New(st, fetcher, creds, log, time.Minute, time.Hour, nil)
	favDir := t.TempDir()
	fc, err := favicon.NewCache(favDir, log)
	if err != nil {
		t.Fatal(err)
	}
	imgs, err := imgproxy.New(t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	validator := &auth.LocalValidator{Store: st}
	ex := extract.NewAllowLocal(5 * time.Second)
	srv := NewServer(st, validator, fetcher, refresh, fc, imgs, ex, creds, 90*24*time.Hour, log)
	h := srv.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &testEnv{ts: ts, client: ts.Client(), user: "nacho", pass: "pass1234", faviconDir: favDir, srv: srv}
}

// do runs a request with Basic auth of the given user.
func (e *testEnv) do(t *testing.T, method, path, user, pass string, body any) (int, []byte) {
	t.Helper()
	if strings.HasPrefix(path, "/api/") {
		return e.doRaw(t, method, path, user, pass, body)
	}
	return e.doRaw(t, method, Base+path, user, pass, body)
}

func (e *testEnv) doRaw(t *testing.T, method, path, user, pass string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(user, pass)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func decode(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decodificar %s: %v", data, err)
	}
}

func TestHealthzWithoutAuth(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	resp, err := e.client.Get(e.ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
}

func TestAuthRequired(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	code, _ := e.do(t, "GET", "/version", "", "", nil)
	if code != 401 {
		t.Fatalf("without auth expected 401, I have %d", code)
	}
	code, _ = e.do(t, "GET", "/version", "nacho", "badpass", nil)
	if code != 401 {
		t.Fatalf("bad password expected 401, I have %d", code)
	}
}

func TestMiscEndpoints(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	code, body := e.do(t, "GET", "/version", e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("version: %d", code)
	}
	var v struct{ Version string }
	decode(t, body, &v)
	if v.Version != reportedVersion {
		t.Errorf("version: %q", v.Version)
	}

	code, body = e.do(t, "GET", "/status", e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("status: %d", code)
	}
	if !strings.Contains(string(body), "improperlyConfiguredCron") {
		t.Errorf("status without warnings: %s", body)
	}

	code, body = e.do(t, "GET", "/user", e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("user: %d", code)
	}
	var u struct {
		UserID string `json:"userId"`
	}
	decode(t, body, &u)
	if u.UserID != "nacho" {
		t.Errorf("user: %s", body)
	}
}

func TestFolderCRUD(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	code, body := e.do(t, "POST", "/folders", e.user, e.pass, map[string]string{"name": "Tech"})
	if code != 200 {
		t.Fatalf("create folder: %d %s", code, body)
	}
	var fr struct {
		Folders []store.Folder
	}
	decode(t, body, &fr)
	if len(fr.Folders) != 1 || fr.Folders[0].Name != "Tech" {
		t.Fatalf("folder response: %s", body)
	}
	folderID := fr.Folders[0].ID

	if code, _ := e.do(t, "POST", "/folders", e.user, e.pass, map[string]string{"name": "Tech"}); code != 409 {
		t.Fatalf("duplicate expected 409, I have %d", code)
	}
	if code, _ := e.do(t, "POST", "/folders", e.user, e.pass, map[string]string{"name": ""}); code != 422 {
		t.Fatalf("empty expected 422, I have %d", code)
	}
	// isolation: another user does not see the folder
	code, body = e.do(t, "GET", "/folders", "other", "other1234", nil)
	if code != 200 || strings.Contains(string(body), "Tech") {
		t.Fatalf("broken folder isolation: %d %s", code, body)
	}

	if code, _ = e.do(t, "PUT", fmt.Sprintf("/folders/%d", folderID), e.user, e.pass, map[string]string{"name": "Tech2"}); code != 204 {
		t.Fatalf("rename: %d", code)
	}
	_, body = e.do(t, "GET", "/folders", e.user, e.pass, nil)
	if !strings.Contains(string(body), "Tech2") {
		t.Fatalf("rename not applied: %s", body)
	}
	if code, _ = e.do(t, "DELETE", "/folders/999", e.user, e.pass, nil); code != 404 {
		t.Fatalf("delete of a missing one expected 404, I have %d", code)
	}
	if code, _ = e.do(t, "DELETE", fmt.Sprintf("/folders/%d", folderID), e.user, e.pass, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
}

func TestFeedAndSyncFlow(t *testing.T) {
	ff := &fakeFetcher{}
	e := newTestEnv(t, ff)

	// folder + feed
	code, body := e.do(t, "POST", "/folders", e.user, e.pass, map[string]string{"name": "Lecturas"})
	if code != 200 {
		t.Fatalf("create folder: %d", code)
	}
	var fr struct {
		Folders []store.Folder
	}
	decode(t, body, &fr)
	folderID := fr.Folders[0].ID

	code, body = e.do(t, "POST", "/feeds", e.user, e.pass,
		map[string]any{"url": "https://feed.example/rss", "folderId": folderID})
	if code != 200 {
		t.Fatalf("create feed: %d %s", code, body)
	}
	var feedResp struct {
		Feeds []struct {
			ID          int64  `json:"id"`
			URL         string `json:"url"`
			Title       string `json:"title"`
			FolderID    *int64 `json:"folderId"`
			UnreadCount int64  `json:"unreadCount"`
		}
		NewestItemID int64 `json:"newestItemId"`
	}
	decode(t, body, &feedResp)
	if len(feedResp.Feeds) != 1 || feedResp.Feeds[0].URL != "https://feed.example/rss" {
		t.Fatalf("feed response: %s", body)
	}
	// the subscription URL wins over the XML one
	if feedResp.Feeds[0].Title != "Feed https://feed.example/rss" {
		t.Errorf("title feed: %s", body)
	}
	if feedResp.Feeds[0].FolderID == nil || *feedResp.Feeds[0].FolderID != folderID {
		t.Errorf("folderId: %s", body)
	}
	if feedResp.Feeds[0].UnreadCount != int64(len("https://feed.example/rss")) {
		t.Errorf("unreadCount: %d (url len %d)", feedResp.Feeds[0].UnreadCount, len("https://feed.example/rss"))
	}
	newest := feedResp.NewestItemID
	if newest == 0 {
		t.Fatal("newestItemId should exist with items")
	}
	feedID := feedResp.Feeds[0].ID

	// duplicate -> 409, broken -> 422
	if code, _ = e.do(t, "POST", "/feeds", e.user, e.pass, map[string]string{"url": "https://feed.example/rss"}); code != 409 {
		t.Fatalf("dup feed expected 409: %d", code)
	}
	ff.failURLs = map[string]bool{"https://roto.example/rss": true}
	if code, _ = e.do(t, "POST", "/feeds", e.user, e.pass, map[string]string{"url": "https://roto.example/rss"}); code != 422 {
		t.Fatalf("broken feed expected 422: %d", code)
	}

	// initial sync: unread all + starred
	code, body = e.do(t, "GET", "/items?type=3&getRead=false&batchSize=-1", e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("items: %d", code)
	}
	var itemsResp struct {
		Items []store.Item
	}
	decode(t, body, &itemsResp)
	n := len(itemsResp.Items)
	if n != len("https://feed.example/rss") {
		t.Fatalf("items iniciales: %d", n)
	}
	// default order: newest first
	if itemsResp.Items[0].ID != newest {
		t.Errorf("order: first %d, expected %d", itemsResp.Items[0].ID, newest)
	}
	firstID := itemsResp.Items[0].ID

	// pagination: batchSize 2 + offset by id (cursor: items with id <= cursor)
	// NOTE: capture ids BEFORE the next decode (json.Unmarshal reuses
	// the slice backing array and would overwrite page1).
	_, body = e.do(t, "GET", "/items?type=3&getRead=true&batchSize=2", e.user, e.pass, nil)
	decode(t, body, &itemsResp)
	page1 := itemsResp.Items
	if len(page1) != 2 {
		t.Fatalf("batchSize: %d", len(page1))
	}
	cursor := page1[len(page1)-1].ID
	secondID := page1[1].ID // page1[0] == firstID (same newest item)
	_, body = e.do(t, "GET", fmt.Sprintf("/items?type=3&getRead=true&batchSize=2&offset=%d", cursor), e.user, e.pass, nil)
	decode(t, body, &itemsResp)
	if len(itemsResp.Items) != 2 {
		t.Fatalf("page 2 with batchSize=2 must have 2 items, I have %d", len(itemsResp.Items))
	}
	for _, it := range itemsResp.Items {
		if it.ID > cursor {
			t.Fatalf("offset returns items newer than the cursor: %d > %d", it.ID, cursor)
		}
	}

	// bulk mark read: spec variant (POST itemIds)
	if code, _ = e.do(t, "POST", "/items/read/multiple", e.user, e.pass,
		map[string]any{"itemIds": []int64{firstID}}); code != 204 {
		t.Fatalf("read multiple POST: %d", code)
	}
	// How To Sync variant (PUT items)
	if code, _ = e.do(t, "PUT", "/items/read/multiple", e.user, e.pass,
		map[string]any{"items": []int64{secondID}}); code != 204 {
		t.Fatalf("read multiple PUT: %d", code)
	}

	_, body = e.do(t, "GET", "/items?type=3&getRead=false&batchSize=-1", e.user, e.pass, nil)
	decode(t, body, &itemsResp)
	if len(itemsResp.Items) != n-2 {
		t.Fatalf("after marking 2 as read %d remain, expected %d", len(itemsResp.Items), n-2)
	}

	// GET /feeds reflects unreadCount
	_, body = e.do(t, "GET", "/feeds", e.user, e.pass, nil)
	decode(t, body, &feedResp)
	if feedResp.Feeds[0].UnreadCount != int64(n-2) {
		t.Errorf("unreadCount en feeds: %d", feedResp.Feeds[0].UnreadCount)
	}

	// star single + starred sync
	if code, _ = e.do(t, "POST", fmt.Sprintf("/items/%d/star", firstID), e.user, e.pass, nil); code != 204 {
		t.Fatalf("star: %d", code)
	}
	_, body = e.do(t, "GET", "/items?type=2&getRead=true&batchSize=-1", e.user, e.pass, nil)
	decode(t, body, &itemsResp)
	if len(itemsResp.Items) != 1 || !itemsResp.Items[0].Starred {
		t.Fatalf("starred: %s", body)
	}

	// /items/updated reflects state changes (lastModified bump)
	_, body = e.do(t, "GET", "/items/updated?lastModified=1&type=3", e.user, e.pass, nil)
	decode(t, body, &itemsResp)
	if len(itemsResp.Items) != n {
		t.Fatalf("updated after the marks: %d (expected %d)", len(itemsResp.Items), n)
	}

	// feed read with a newestItemId cap
	if code, _ = e.do(t, "POST", fmt.Sprintf("/feeds/%d/read", feedID), e.user, e.pass,
		map[string]any{"newestItemId": newest}); code != 204 {
		t.Fatalf("feed read: %d", code)
	}
	_, body = e.do(t, "GET", "/items?type=3&getRead=false", e.user, e.pass, nil)
	decode(t, body, &itemsResp)
	if len(itemsResp.Items) != 0 {
		t.Fatalf("%d unread remain after feed read", len(itemsResp.Items))
	}

	// updater API: admin refreshes and 1 new item shows up
	fetchesBefore := ff.fetches
	code, _ = e.do(t, "GET", "/feeds/update?userId=nacho&feedId="+fmt.Sprint(feedID), e.user, e.pass, nil)
	if code != 204 {
		t.Fatalf("feeds/update: %d", code)
	}
	if ff.fetches != fetchesBefore+1 {
		t.Fatal("the updater has not fetched")
	}
	_, body = e.do(t, "GET", "/items?type=3&getRead=true&batchSize=1&oldestFirst=false", e.user, e.pass, nil)
	decode(t, body, &itemsResp)
	_, body = e.do(t, "GET", "/items?type=3&getRead=false", e.user, e.pass, nil)
	decode(t, body, &itemsResp)
	if len(itemsResp.Items) != 1 {
		t.Fatalf("after refresh expected 1 new unread, I have %d", len(itemsResp.Items))
	}

	// updater with a non-admin user -> 401
	if code, _ = e.do(t, "GET", "/feeds/update?userId=nacho&feedId=1", "other", "other1234", nil); code != 401 {
		t.Fatalf("updater por no-admin: %d", code)
	}

	// move + rename + delete
	if code, _ = e.do(t, "POST", fmt.Sprintf("/feeds/%d/move", feedID), e.user, e.pass,
		map[string]any{"folderId": nil}); code != 204 {
		t.Fatalf("move: %d", code)
	}
	if code, _ = e.do(t, "POST", fmt.Sprintf("/feeds/%d/rename", feedID), e.user, e.pass,
		map[string]string{"feedTitle": "Mi feed"}); code != 204 {
		t.Fatalf("rename feed: %d", code)
	}
	if code, _ = e.do(t, "DELETE", fmt.Sprintf("/feeds/%d", feedID), e.user, e.pass, nil); code != 204 {
		t.Fatalf("delete feed: %d", code)
	}
	_, body = e.do(t, "GET", "/items?type=3&getRead=true", e.user, e.pass, nil)
	decode(t, body, &itemsResp)
	if len(itemsResp.Items) != 0 {
		t.Fatalf("deleting a feed must take its items away: %d", len(itemsResp.Items))
	}
}

func TestMarkAllRead(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	_, body := e.do(t, "POST", "/feeds", e.user, e.pass, map[string]string{"url": "https://a.example/f"})
	var feedResp struct {
		NewestItemID int64 `json:"newestItemId"`
	}
	decode(t, body, &feedResp)

	if code, _ := e.do(t, "POST", "/items/read", e.user, e.pass,
		map[string]any{"newestItemId": feedResp.NewestItemID}); code != 204 {
		t.Fatal("mark all read failed")
	}
	_, body = e.do(t, "GET", "/items?type=3&getRead=false", e.user, e.pass, nil)
	var itemsResp struct {
		Items []store.Item
	}
	decode(t, body, &itemsResp)
	if len(itemsResp.Items) != 0 {
		t.Fatalf("quedan %d unread", len(itemsResp.Items))
	}
}

// TestFeedCredentials: subscription with Basic auth (the credentials reach
// the fetcher), later edit (an empty password keeps it) and delete. The
// password NEVER shows up in the responses.
func TestFeedCredentials(t *testing.T) {
	ff := &fakeFetcher{}
	e := newTestEnv(t, ff)

	code, body := e.do(t, "POST", "/feeds", e.user, e.pass,
		map[string]string{"url": "https://auth.example/f", "username": "reader", "password": "s3cret"})
	if code != 200 {
		t.Fatalf("create feed with auth: %d %s", code, body)
	}
	if ff.lastAuthUser != "reader" || ff.lastAuthPass != "s3cret" {
		t.Fatalf("the fetcher did not receive the credentials: %q/%q", ff.lastAuthUser, ff.lastAuthPass)
	}
	var fr struct {
		Feeds []store.Feed `json:"feeds"`
	}
	decode(t, body, &fr)
	if len(fr.Feeds) != 1 || fr.Feeds[0].AuthUser != "reader" {
		t.Fatalf("authUser in response: %s", body)
	}
	if strings.Contains(string(body), "s3cret") {
		t.Fatal("the password must NOT show up in the response")
	}
	feedID := fr.Feeds[0].ID

	// GET /feeds exposes authUser but not the password
	code, body = e.do(t, "GET", "/feeds", e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("list feeds: %d", code)
	}
	if !strings.Contains(string(body), `"authUser":"reader"`) {
		t.Fatalf("authUser not exposed: %s", body)
	}
	if strings.Contains(string(body), "s3cret") {
		t.Fatal("the password must NOT show up in /feeds")
	}

	// change only the user (empty password = keep it)
	if code, _ := e.do(t, "POST", fmt.Sprintf("/feeds/%d/credentials", feedID), e.user, e.pass,
		map[string]string{"username": "reader2"}); code != 200 {
		t.Fatalf("update user: %d", code)
	}
	// the kept password still works: force a refresh through the admin updater
	if code, _ := e.do(t, "GET", fmt.Sprintf("/feeds/update?userId=%s&feedId=%d", e.user, feedID), e.user, e.pass, nil); code != 204 {
		t.Fatalf("update after changing the user: %d", code)
	}
	if ff.lastAuthUser != "reader2" || ff.lastAuthPass != "s3cret" {
		t.Fatalf("credentials after the edit: %q/%q", ff.lastAuthUser, ff.lastAuthPass)
	}

	// change the password
	if code, _ := e.do(t, "POST", fmt.Sprintf("/feeds/%d/credentials", feedID), e.user, e.pass,
		map[string]string{"username": "reader2", "password": "new"}); code != 200 {
		t.Fatalf("update pass: %d", code)
	}
	if code, _ := e.do(t, "GET", fmt.Sprintf("/feeds/update?userId=%s&feedId=%d", e.user, feedID), e.user, e.pass, nil); code != 204 {
		t.Fatalf("update after changing the password: %d", code)
	}
	if ff.lastAuthPass != "new" {
		t.Fatalf("password not updated: %q", ff.lastAuthPass)
	}

	// remove the auth
	if code, _ := e.do(t, "POST", fmt.Sprintf("/feeds/%d/credentials", feedID), e.user, e.pass,
		map[string]string{}); code != 200 {
		t.Fatalf("remove auth: %d", code)
	}
	if code, _ := e.do(t, "GET", fmt.Sprintf("/feeds/update?userId=%s&feedId=%d", e.user, feedID), e.user, e.pass, nil); code != 204 {
		t.Fatalf("update after removing auth: %d", code)
	}
	if ff.lastAuthUser != "" || ff.lastAuthPass != "" {
		t.Fatalf("auth not removed: %q/%q", ff.lastAuthUser, ff.lastAuthPass)
	}

	// password without a user -> invalid
	if code, _ := e.do(t, "POST", fmt.Sprintf("/feeds/%d/credentials", feedID), e.user, e.pass,
		map[string]string{"password": "alone"}); code != 422 {
		t.Fatalf("password without user expected 422: %d", code)
	}
}

func TestUserIsolation(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	e.do(t, "POST", "/feeds", e.user, e.pass, map[string]string{"url": "https://mine.example/f"})
	_, body := e.do(t, "GET", "/items?type=3&getRead=true", "other", "other1234", nil)
	var itemsResp struct {
		Items []store.Item
	}
	decode(t, body, &itemsResp)
	if len(itemsResp.Items) != 0 {
		t.Fatalf("broken isolation: another user sees %d items", len(itemsResp.Items))
	}
	// marking by another user id must not work (404)
	if code, _ := e.do(t, "POST", "/items/1/read", "other", "other1234", nil); code != 404 {
		t.Fatalf("mark another user item: %d (expected 404)", code)
	}
}

func TestCORSPresent(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	req, _ := http.NewRequest("OPTIONS", e.ts.URL+Base+"/feeds", nil)
	req.Header.Set("Origin", "https://pwa.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("CORS ausente en preflight")
	}
}

type errEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// i18n: user es -> Spanish messages, auto -> Accept-Language, nothing -> en.
func TestErrorI18n(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})

	// user with language=es (nacho is created with es in newTestEnv)
	code, body := e.do(t, "DELETE", "/folders/999", e.user, e.pass, nil)
	if code != 404 {
		t.Fatalf("404 esperado: %d", code)
	}
	var env errEnvelope
	decode(t, body, &env)
	if env.Error.Code != "folder_not_found" {
		t.Errorf("code: %q", env.Error.Code)
	}
	if env.Error.Message != "carpeta no encontrada" {
		t.Errorf("es message: %q", env.Error.Message)
	}

	// auto + Accept-Language en -> English (DELETE: there is no GET for a single folder)
	req, _ := http.NewRequest("DELETE", e.ts.URL+Base+"/folders/999", nil)
	req.SetBasicAuth("other", "other1234")
	req.Header.Set("Accept-Language", "en-GB,en;q=0.9")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	decode(t, raw, &env)
	if env.Error.Message != "folder not found" {
		t.Errorf("message in en via Accept-Language: %q", env.Error.Message)
	}

	// 401 without auth negotiates by Accept-Language (es)
	req2, _ := http.NewRequest("GET", e.ts.URL+Base+"/version", nil)
	req2.Header.Set("Accept-Language", "es-ES,es;q=0.9")
	resp2, err := e.client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	raw2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	decode(t, raw2, &env)
	if env.Error.Code != "unauthorized" || env.Error.Message != "no autorizado" {
		t.Errorf("401 i18n: %s", raw2)
	}

	// /user exposes language (own extension)
	_, body = e.do(t, "GET", "/user", e.user, e.pass, nil)
	if !strings.Contains(string(body), `"language":"es"`) {
		t.Errorf("/user language: %s", body)
	}
}

// TestFeedFilterFlow: /feeds/{id}/filter endpoints (News 28.4.0).
func TestFeedFilterFlow(t *testing.T) {
	ff := &fakeFetcher{}
	e := newTestEnv(t, ff)

	// subscribe a feed
	if code, body := e.do(t, "POST", "/feeds", e.user, e.pass,
		map[string]string{"url": "https://feed.example/rss"}); code != 200 {
		t.Fatalf("create feed: %d %s", code, body)
	}
	var feedResp struct {
		Feeds []struct{ ID int64 }
	}
	_, body := e.do(t, "GET", "/feeds", e.user, e.pass, nil)
	decode(t, body, &feedResp)
	fid := feedResp.Feeds[0].ID

	// GET without a filter -> empty
	code, body := e.do(t, "GET", fmt.Sprintf("/feeds/%d/filter", fid), e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("get filter: %d", code)
	}
	var fr struct {
		Filter store.FeedFilter
	}
	decode(t, body, &fr)
	if fr.Filter.HasFilter() {
		t.Fatalf("the initial filter should be empty: %s", body)
	}

	// POST with keywords -> it re-applies and returns the filter
	code, body = e.do(t, "POST", fmt.Sprintf("/feeds/%d/filter", fid), e.user, e.pass,
		map[string]any{"titleKeywords": "sponsored", "bodyKeywords": "tracking"})
	if code != 200 {
		t.Fatalf("set filter: %d %s", code, body)
	}
	decode(t, body, &fr)
	if fr.Filter.TitleKeywords != "sponsored" || fr.Filter.BodyKeywords != "tracking" {
		t.Fatalf("filtro guardado: %s", body)
	}

	// DELETE -> it is deleted and the items are unfrozen
	code, _ = e.do(t, "DELETE", fmt.Sprintf("/feeds/%d/filter", fid), e.user, e.pass, nil)
	if code != 204 {
		t.Fatalf("delete filter: %d", code)
	}
	code, body = e.do(t, "GET", fmt.Sprintf("/feeds/%d/filter", fid), e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("get filter after delete: %d %s", code, body)
	}
	decode(t, body, &fr)
	if fr.Filter.HasFilter() {
		t.Fatalf("the filter should have been deleted: %s", body)
	}
}

// TestSearchFlow: GET /items/search?query= searches in title/body/URL.
func TestSearchFlow(t *testing.T) {
	ff := &fakeFetcher{}
	e := newTestEnv(t, ff)
	if code, body := e.do(t, "POST", "/feeds", e.user, e.pass,
		map[string]string{"url": "https://feed.example/rss"}); code != 200 {
		t.Fatalf("create feed: %d %s", code, body)
	}

	// the fake generates titles "Item %d of %s"
	code, body := e.do(t, "GET", "/items/search?query=Item", e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("search: %d", code)
	}
	var sr struct {
		Items []store.Item
	}
	decode(t, body, &sr)
	if len(sr.Items) == 0 {
		t.Fatalf("search 'Item' should return results: %s", body)
	}

	// empty query -> 422
	if code, _ := e.do(t, "GET", "/items/search?query=", e.user, e.pass, nil); code != 422 {
		t.Fatalf("empty query expected 422: %d", code)
	}
	// no results -> empty list
	code, body = e.do(t, "GET", "/items/search?query=zzzznothing", e.user, e.pass, nil)
	decode(t, body, &sr)
	if code != 200 || len(sr.Items) != 0 {
		t.Fatalf("search without results: %d %s", code, body)
	}
}

// TestSettingsAndRetentionFlow: user settings and per-feed retention.
func TestSettingsAndRetentionFlow(t *testing.T) {
	ff := &fakeFetcher{}
	e := newTestEnv(t, ff)

	// default settings
	code, body := e.do(t, "GET", "/api/me/settings", e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("get settings: %d %s", code, body)
	}
	var st struct {
		Theme          string `json:"theme"`
		ReaderMaxWidth string `json:"readerMaxWidth"`
		ReaderFont     string `json:"readerFont"`
		ReaderFontSize string `json:"readerFontSize"`
	}
	decode(t, body, &st)
	if st.Theme != "system" || st.ReaderMaxWidth != "normal" || st.ReaderFont != "default" || st.ReaderFontSize != "15" {
		t.Fatalf("settings default: %s", body)
	}

	// update theme + interval
	code, body = e.do(t, "PUT", "/api/me/settings", e.user, e.pass,
		map[string]any{"theme": "dark", "feedIntervalMin": "30"})
	if code != 200 {
		t.Fatalf("update settings: %d %s", code, body)
	}
	decode(t, body, &st)
	if st.Theme != "dark" {
		t.Fatalf("theme after update: %s", body)
	}
	// invalid interval -> 422
	if code, _ := e.do(t, "PUT", "/api/me/settings", e.user, e.pass,
		map[string]any{"feedIntervalMin": "1"}); code != 422 {
		t.Fatalf("invalid interval expected 422: %d", code)
	}

	// reader font and size
	code, body = e.do(t, "PUT", "/api/me/settings", e.user, e.pass,
		map[string]any{"readerFont": "serif", "readerFontSize": "17"})
	if code != 200 {
		t.Fatalf("update font settings: %d %s", code, body)
	}
	decode(t, body, &st)
	if st.ReaderFont != "serif" || st.ReaderFontSize != "17" {
		t.Fatalf("font after update: %s", body)
	}
	if code, _ := e.do(t, "PUT", "/api/me/settings", e.user, e.pass,
		map[string]any{"readerFont": "comic-sans"}); code != 422 {
		t.Fatalf("invalid font expected 422: %d", code)
	}
	if code, _ := e.do(t, "PUT", "/api/me/settings", e.user, e.pass,
		map[string]any{"readerFontSize": "42"}); code != 422 {
		t.Fatalf("invalid size expected 422: %d", code)
	}

	// create a feed and set retention
	if code, body := e.do(t, "POST", "/feeds", e.user, e.pass,
		map[string]string{"url": "https://feed.example/rss"}); code != 200 {
		t.Fatalf("create feed: %d %s", code, body)
	}
	var feedResp struct {
		Feeds []struct{ ID int64 }
	}
	_, body = e.do(t, "GET", "/feeds", e.user, e.pass, nil)
	decode(t, body, &feedResp)
	fid := feedResp.Feeds[0].ID

	code, body = e.do(t, "GET", fmt.Sprintf("/feeds/%d/retention", fid), e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("get retention: %d %s", code, body)
	}
	var rr struct {
		RetentionDays int64 `json:"retentionDays"`
	}
	decode(t, body, &rr)
	if rr.RetentionDays != 0 {
		t.Fatalf("default retention: %d", rr.RetentionDays)
	}
	code, body = e.do(t, "POST", fmt.Sprintf("/feeds/%d/retention", fid), e.user, e.pass,
		map[string]any{"retentionDays": 30})
	if code != 200 {
		t.Fatalf("set retention: %d %s", code, body)
	}
	decode(t, body, &rr)
	if rr.RetentionDays != 30 {
		t.Fatalf("retention after set: %d", rr.RetentionDays)
	}
	// invalid -> 422
	if code, _ := e.do(t, "POST", fmt.Sprintf("/feeds/%d/retention", fid), e.user, e.pass,
		map[string]any{"retentionDays": 99999}); code != 422 {
		t.Fatalf("invalid retention expected 422: %d", code)
	}
}

// TestDiscoverFlow: GET /feeds/discover?url= detects the feeds of a site.
func TestDiscoverFlow(t *testing.T) {
	ff := &fakeFetcher{}
	e := newTestEnv(t, ff)

	code, body := e.do(t, "GET", "/feeds/discover?url=https://site.example", e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("discover: %d %s", code, body)
	}
	var dr struct {
		Feeds []feed.DiscoveredFeed
	}
	decode(t, body, &dr)
	if len(dr.Feeds) == 0 {
		t.Fatalf("discover should return feeds: %s", body)
	}

	// empty url -> 422
	if code, _ := e.do(t, "GET", "/feeds/discover?url=", e.user, e.pass, nil); code != 422 {
		t.Fatalf("discover without url expected 422: %d", code)
	}
	// url not http(s) -> 422
	if code, _ := e.do(t, "GET", "/feeds/discover?url=ftp://x", e.user, e.pass, nil); code != 422 {
		t.Fatalf("discover invalid scheme expected 422: %d", code)
	}
	// site without feeds -> 422
	ff.failURLs = map[string]bool{"https://nofeeds.example": true}
	if code, _ := e.do(t, "GET", "/feeds/discover?url=https://nofeeds.example", e.user, e.pass, nil); code != 422 {
		t.Fatalf("discover without feeds expected 422: %d", code)
	}
}
