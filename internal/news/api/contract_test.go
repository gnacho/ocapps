package api

// Contract tests of the handlers that only differ from the happy path in their
// validation: the error code returned for an unusable id, body or field, and
// the handlers that had no coverage at all (auto-mark rules, folder read,
// scraper selector, the admin cleanup routes).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gnacho/ocapps/internal/news/store"
)

// errorCode decodes the error envelope of a response.
func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var b struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decode(t, body, &b)
	if b.Error.Message == "" {
		t.Fatalf("error without message: %s", body)
	}
	return b.Error.Code
}

func TestAutoReadRules(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})

	// Empty list.
	code, body := e.do(t, "GET", "/auto-read", e.user, e.pass, nil)
	if code != 200 || !strings.Contains(string(body), `"rules":[]`) {
		t.Fatalf("list = %d %s", code, body)
	}

	// Create a global rule.
	code, body = e.do(t, "POST", "/auto-read", e.user, e.pass, map[string]any{"titlePattern": " ^sponsor "})
	if code != 201 {
		t.Fatalf("create = %d %s", code, body)
	}
	var created struct {
		Rule struct {
			ID           int64  `json:"id"`
			FeedID       int64  `json:"feedId"`
			TitlePattern string `json:"titlePattern"`
		} `json:"rule"`
	}
	decode(t, body, &created)
	if created.Rule.ID == 0 || created.Rule.TitlePattern != "^sponsor" {
		t.Fatalf("rule = %+v, want the trimmed pattern stored", created.Rule)
	}

	// Validation: no pattern, unusable regex and unknown feed.
	for _, body := range []map[string]any{
		{"titlePattern": "   "},
		{"titlePattern": "(["},
		{"feedId": 999999, "titlePattern": "x"},
	} {
		code, resp := e.do(t, "POST", "/auto-read", e.user, e.pass, body)
		want := 422
		if body["feedId"] != nil {
			want = 404
		}
		if code != want {
			t.Fatalf("create %v = %d %s, want %d", body, code, resp, want)
		}
	}
	// Without a body the pattern is empty, which is what gets rejected.
	code, body = e.doRaw(t, "POST", Base+"/auto-read", e.user, e.pass, nil)
	if code != 422 || errorCode(t, body) != "invalid_rule" {
		t.Fatalf("create without body = %d %s", code, body)
	}

	// A rule bound to an existing feed.
	code, feedBody := e.do(t, "POST", "/feeds", e.user, e.pass, map[string]any{"url": "https://ar.example/rss"})
	if code != 200 {
		t.Fatalf("create feed = %d %s", code, feedBody)
	}
	feedID := extractJSONInt(t, feedBody, `"id":`)
	code, body = e.do(t, "POST", "/auto-read", e.user, e.pass, map[string]any{"feedId": feedID, "titlePattern": "promo"})
	if code != 201 {
		t.Fatalf("create feed rule = %d %s", code, body)
	}

	// Delete: invalid id, unknown id and the real one.
	code, body = e.do(t, "DELETE", "/auto-read/not-a-number", e.user, e.pass, nil)
	if code != 404 || errorCode(t, body) != "rule_not_found" {
		t.Fatalf("delete invalid id = %d %s", code, body)
	}
	code, body = e.do(t, "DELETE", "/auto-read/999999", e.user, e.pass, nil)
	if code != 404 || errorCode(t, body) != "rule_not_found" {
		t.Fatalf("delete unknown rule = %d %s", code, body)
	}
	code, body = e.do(t, "DELETE", "/auto-read/"+itoa(created.Rule.ID), e.user, e.pass, nil)
	if code != 204 {
		t.Fatalf("delete = %d %s, want 204", code, body)
	}
	code, body = e.do(t, "GET", "/auto-read", e.user, e.pass, nil)
	if code != 200 || strings.Contains(string(body), "^sponsor") {
		t.Fatalf("list after delete = %d %s, want the rule gone", code, body)
	}
}

func TestMarkFolderRead(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	code, body := e.do(t, "POST", "/folders", e.user, e.pass, map[string]any{"name": "Later"})
	if code != 200 {
		t.Fatalf("create folder = %d %s", code, body)
	}
	folderID := extractJSONInt(t, body, `"id":`)

	// A feed inside the folder with items to mark.
	code, body = e.do(t, "POST", "/feeds", e.user, e.pass, map[string]any{"url": "https://fr.example/rss", "folderId": folderID})
	if code != 200 {
		t.Fatalf("create feed = %d %s", code, body)
	}

	code, body = e.do(t, "POST", "/folders/"+itoa(folderID)+"/read", e.user, e.pass, map[string]any{"newestItemId": 0})
	if code != 204 {
		t.Fatalf("mark folder read = %d %s, want 204", code, body)
	}

	// Errors: unknown folder, unusable id and unusable body.
	code, body = e.do(t, "POST", "/folders/999999/read", e.user, e.pass, map[string]any{"newestItemId": 0})
	if code != 404 || errorCode(t, body) != "folder_not_found" {
		t.Fatalf("unknown folder = %d %s", code, body)
	}
	code, body = e.do(t, "POST", "/folders/not-a-number/read", e.user, e.pass, map[string]any{"newestItemId": 0})
	if code != 404 || errorCode(t, body) != "folder_not_found" {
		t.Fatalf("unusable id = %d %s", code, body)
	}
	// Without a body the cap is 0 (everything), which is accepted.
	code, body = e.doRaw(t, "POST", Base+"/folders/"+itoa(folderID)+"/read", e.user, e.pass, nil)
	if code != 204 {
		t.Fatalf("empty body = %d %s, want 204", code, body)
	}
}

func TestFeedScraperSelector(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	code, body := e.do(t, "POST", "/feeds", e.user, e.pass, map[string]any{"url": "https://sc.example/rss"})
	if code != 200 {
		t.Fatalf("create feed = %d %s", code, body)
	}
	feedID := extractJSONInt(t, body, `"id":`)
	path := "/feeds/" + itoa(feedID) + "/scraper"

	// Empty by default.
	code, body = e.do(t, "GET", path, e.user, e.pass, nil)
	if code != 200 || !strings.Contains(string(body), `"scraperSelector":""`) {
		t.Fatalf("get = %d %s", code, body)
	}

	code, body = e.do(t, "POST", path, e.user, e.pass, map[string]any{"scraperSelector": "article .body"})
	if code != 200 || !strings.Contains(string(body), "article .body") {
		t.Fatalf("set = %d %s", code, body)
	}
	code, body = e.do(t, "GET", path, e.user, e.pass, nil)
	if code != 200 || !strings.Contains(string(body), "article .body") {
		t.Fatalf("get after set = %d %s", code, body)
	}

	// Clearing the selector is allowed.
	code, body = e.do(t, "POST", path, e.user, e.pass, map[string]any{"scraperSelector": ""})
	if code != 200 || !strings.Contains(string(body), `"scraperSelector":""`) {
		t.Fatalf("clear = %d %s", code, body)
	}

	// An unusable selector is refused instead of stored.
	code, body = e.do(t, "POST", path, e.user, e.pass, map[string]any{"scraperSelector": "article[[["})
	if code != 422 || errorCode(t, body) != "invalid_selector" {
		t.Fatalf("invalid selector = %d %s", code, body)
	}
	// Without a body the selector is empty, which clears it instead of failing.
	code, body = e.doRaw(t, "POST", Base+path, e.user, e.pass, nil)
	if code != 200 || !strings.Contains(string(body), `"scraperSelector":""`) {
		t.Fatalf("empty body = %d %s, want the selector cleared", code, body)
	}

	// Unknown feed and unusable id.
	for _, p := range []string{"/feeds/999999/scraper", "/feeds/zero/scraper"} {
		code, body = e.do(t, "GET", p, e.user, e.pass, nil)
		if code != 404 || errorCode(t, body) != "feed_not_found" {
			t.Fatalf("get %s = %d %s", p, code, body)
		}
		code, body = e.do(t, "POST", p, e.user, e.pass, map[string]any{"scraperSelector": "a"})
		if code != 404 || errorCode(t, body) != "feed_not_found" {
			t.Fatalf("set %s = %d %s", p, code, body)
		}
	}
}

// bodyOf runs a GET and returns its body, failing on a non-200.
func bodyOf(t *testing.T, e *testEnv, path string, args ...any) []byte {
	t.Helper()
	code, body := e.do(t, "GET", path, e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("GET %s = %d %s", path, code, body)
	}
	return body
}

// TestAdminCleanupRoutes: the cleanup and updater routes are admin only and
// answer the contract the external updater expects.
func TestAdminCleanupRoutes(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	code, body := e.do(t, "POST", "/feeds", e.user, e.pass, map[string]any{"url": "https://cu.example/rss"})
	if code != 200 {
		t.Fatalf("create feed = %d %s", code, body)
	}

	admin := []struct {
		path string
		code int
	}{
		{"/cleanup/before-update", http.StatusNoContent},
		{"/cleanup/after-update", http.StatusNoContent},
		{"/feeds/all", http.StatusOK},
	}
	for _, route := range admin {
		path := route.path
		if code, body = e.do(t, "GET", path, e.user, e.pass, nil); code != route.code {
			t.Fatalf("%s = %d %s, want %d for an admin", path, code, body, route.code)
		}
		// A normal user gets the admin error, never the payload.
		code, body = e.do(t, "GET", path, "other", "other1234", nil)
		if code != http.StatusUnauthorized || errorCode(t, body) != "admin_required" {
			t.Fatalf("%s as a normal user = %d %s", path, code, body)
		}
	}

	// /feeds/update is the spec updater route: admin only, it refreshes the
	// feed of a user chosen by username and feed id.
	code, body = e.do(t, "GET", "/feeds/update", "other", "other1234", nil)
	if code != http.StatusUnauthorized || errorCode(t, body) != "admin_required" {
		t.Fatalf("feeds/update as a normal user = %d %s", code, body)
	}
	code, body = e.do(t, "GET", "/feeds/update", e.user, e.pass, nil)
	if code != 404 || errorCode(t, body) != "feed_not_found" {
		t.Fatalf("feeds/update without parameters = %d %s", code, body)
	}
	feedID := extractJSONInt(t, bodyOf(t, e, "/feeds"), `"id":`)
	code, body = e.do(t, "GET", "/feeds/update?userId=ghost&feedId="+itoa(feedID), e.user, e.pass, nil)
	if code != 404 || errorCode(t, body) != "user_not_found" {
		t.Fatalf("feeds/update for an unknown user = %d %s", code, body)
	}
	code, body = e.do(t, "GET", "/feeds/update?userId=nacho&feedId="+itoa(feedID), e.user, e.pass, nil)
	if code != 204 {
		t.Fatalf("feeds/update = %d %s, want 204 after the refresh", code, body)
	}

	code, body = e.do(t, "GET", "/feeds/all", e.user, e.pass, nil)
	if !strings.Contains(string(body), `"userId":"nacho"`) {
		t.Fatalf("feeds/all = %d %s, want the owner username", code, body)
	}
}

func TestMediaMime(t *testing.T) {
	s := &Server{}
	cases := map[string]bool{
		"audio/mpeg":       true,
		"video/mp4":        true,
		"image/png":        true,
		"text/html":        false,
		"application/pdf":  false,
		"":                 false,
		"application/mp4x": false,
	}
	for mime, want := range cases {
		if got := s.mediaMime(mime); got != want {
			t.Fatalf("mediaMime(%q) = %v, want %v", mime, got, want)
		}
	}
}

func TestRewriteAllSignsMediaEnclosures(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	media := "video/mp4"
	link := "https://media.example/clip.mp4"
	doc := "application/pdf"
	pdfLink := "https://media.example/doc.pdf"

	items := []store.Item{
		{Body: `<img src="https://img.example/a.jpg">`, EnclosureMime: &media, EnclosureLink: &link},
		{Body: "plain", EnclosureMime: &doc, EnclosureLink: &pdfLink},
	}
	e.srv.rewriteAll(items)

	if !strings.Contains(items[0].Body, "/img?u=") {
		t.Fatalf("body = %q, want the image proxied", items[0].Body)
	}
	if items[0].EnclosureLink == nil || !strings.HasPrefix(*items[0].EnclosureLink, Base+"/img?u=") {
		t.Fatalf("media enclosure = %v, want it proxied", items[0].EnclosureLink)
	}
	// A document enclosure is left alone: the proxy only serves media.
	if items[1].EnclosureLink == nil || *items[1].EnclosureLink != pdfLink {
		t.Fatalf("document enclosure = %v, want it untouched", items[1].EnclosureLink)
	}
}

func TestSettingsValidators(t *testing.T) {
	for _, w := range []string{"narrow", "normal", "wide"} {
		if !validWidth(w) {
			t.Fatalf("validWidth(%q) = false", w)
		}
	}
	for _, w := range []string{"", "huge", "Normal"} {
		if validWidth(w) {
			t.Fatalf("validWidth(%q) = true", w)
		}
	}

	if !validNtfyTopic("news_casa-2026") {
		t.Fatal("a plain topic must be valid")
	}
	if !validNtfyTopic("") {
		t.Fatal("an empty topic is valid: it clears the setting")
	}
	for _, bad := range []string{"with space", "acentué", strings.Repeat("a", 65), "a/b"} {
		if validNtfyTopic(bad) {
			t.Fatalf("validNtfyTopic(%q) = true", bad)
		}
	}
}

func TestUpdateMySettingsRejectsUnusableValues(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})

	cases := []struct {
		name string
		body map[string]any
		code int
		want string
	}{
		{"theme", map[string]any{"theme": "neon"}, 422, "invalid_theme"},
		{"width", map[string]any{"readerMaxWidth": "huge"}, 422, "invalid_reader_width"},
		{"interval", map[string]any{"feedIntervalMin": "2"}, 422, "invalid_feed_interval"},
		{"font", map[string]any{"readerFont": "comic"}, 422, "invalid_reader_font"},
		{"font size", map[string]any{"readerFontSize": "40"}, 422, "invalid_reader_font_size"},
		{"ntfy topic", map[string]any{"ntfyTopic": "a b"}, 422, "invalid_ntfy_topic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := e.do(t, "PUT", "/api/me/settings", e.user, e.pass, tc.body)
			if code != tc.code || errorCode(t, body) != tc.want {
				t.Fatalf("status = %d %s, want %d %s", code, body, tc.code, tc.want)
			}
		})
	}

	// The valid values are accepted and come back in the settings.
	code, body := e.do(t, "PUT", "/api/me/settings", e.user, e.pass, map[string]any{
		"theme": "dark", "readerMaxWidth": "wide", "ntfyTopic": "casa-2026",
	})
	if code != 200 {
		t.Fatalf("valid settings = %d %s", code, body)
	}
	if !strings.Contains(string(body), `"theme":"dark"`) || !strings.Contains(string(body), "casa-2026") {
		t.Fatalf("settings = %s, want the stored values", body)
	}
}

// TestNotFoundContract checks the 404 of every resource address by id: the
// clients rely on it to fall back after a failed refresh.
func TestNotFoundContract(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	const missing = "/999999"

	paths := []struct {
		method, path, wantCode string
	}{
		{"DELETE", "/feeds" + missing, "feed_not_found"},
		{"POST", "/feeds" + missing + "/read", "feed_not_found"},
		{"GET", "/feeds" + missing + "/filter", "feed_not_found"},
		{"POST", "/feeds" + missing + "/filter", "feed_not_found"},
		{"DELETE", "/feeds" + missing + "/filter", "feed_not_found"},
		{"GET", "/feeds" + missing + "/rules", "feed_not_found"},
		{"POST", "/feeds" + missing + "/rules", "feed_not_found"},
		{"DELETE", "/feeds" + missing + "/rules", "feed_not_found"},
		{"GET", "/feeds" + missing + "/retention", "feed_not_found"},
		{"POST", "/feeds" + missing + "/retention", "feed_not_found"},
		{"POST", "/feeds" + missing + "/credentials", "feed_not_found"},
		{"DELETE", "/folders" + missing, "folder_not_found"},
		{"DELETE", "/searches" + missing, "search_not_found"},
		{"GET", "/searches" + missing + "/items", "search_not_found"},
	}
	for _, p := range paths {
		code, body := e.do(t, p.method, p.path, e.user, e.pass, nil)
		if code != 404 {
			t.Fatalf("%s %s = %d %s, want 404", p.method, p.path, code, body)
		}
		if got := errorCode(t, body); got != p.wantCode {
			t.Fatalf("%s %s code = %q, want %q", p.method, p.path, got, p.wantCode)
		}
	}

	// Renaming needs a name in the body before it gets to the lookup.
	code, body := e.do(t, "PUT", "/folders"+missing, e.user, e.pass, map[string]any{"name": "x"})
	if code != 404 || errorCode(t, body) != "folder_not_found" {
		t.Fatalf("rename unknown folder = %d %s", code, body)
	}

	// Item routes answer not_found for an item of another user.
	itemPaths := []string{"/items/999999/full"}
	for _, p := range itemPaths {
		code, body := e.do(t, "GET", p, e.user, e.pass, nil)
		if code != 404 {
			t.Fatalf("%s = %d %s, want 404", p, code, body)
		}
	}
}

// TestFeedOrganisationCRUD walks the whole per-feed surface on a real feed:
// the success path of the handlers the other tests only hit with errors.
func TestFeedOrganisationCRUD(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})

	// Folder: create and rename.
	code, body := e.do(t, "POST", "/folders", e.user, e.pass, map[string]any{"name": "Reading"})
	if code != 200 {
		t.Fatalf("create folder = %d %s", code, body)
	}
	folderID := extractJSONInt(t, body, `"id":`)
	code, body = e.do(t, "PUT", "/folders/"+itoa(folderID), e.user, e.pass, map[string]any{"name": "Later"})
	if code != 204 {
		t.Fatalf("rename folder = %d %s, want 204", code, body)
	}
	code, body = e.do(t, "PUT", "/folders/"+itoa(folderID), e.user, e.pass, map[string]any{"name": ""})
	if code != 422 || errorCode(t, body) != "invalid_folder_name" {
		t.Fatalf("rename without a name = %d %s", code, body)
	}

	// Feed: create, move into the folder, rename.
	code, body = e.do(t, "POST", "/feeds", e.user, e.pass, map[string]any{"url": "https://org.example/rss"})
	if code != 200 {
		t.Fatalf("create feed = %d %s", code, body)
	}
	feedID := extractJSONInt(t, body, `"id":`)
	feedPath := "/feeds/" + itoa(feedID)

	code, body = e.do(t, "POST", feedPath+"/move", e.user, e.pass, map[string]any{"folderId": folderID})
	if code != 204 {
		t.Fatalf("move = %d %s, want 204", code, body)
	}
	code, body = e.do(t, "POST", feedPath+"/move", e.user, e.pass, map[string]any{"folderId": 0})
	if code != 204 {
		t.Fatalf("move to the root = %d %s", code, body)
	}
	code, body = e.do(t, "POST", feedPath+"/rename", e.user, e.pass, map[string]any{"feedTitle": "Renamed"})
	if code != 204 {
		t.Fatalf("rename = %d %s, want 204", code, body)
	}
	code, body = e.do(t, "POST", feedPath+"/rename", e.user, e.pass, map[string]any{"feedTitle": ""})
	if code != 422 || errorCode(t, body) != "invalid_title" {
		t.Fatalf("rename without a title = %d %s", code, body)
	}
	if !strings.Contains(string(bodyOf(t, e, "/feeds")), `"title":"Renamed"`) {
		t.Fatalf("the feed title was not updated: %s", bodyOf(t, e, "/feeds"))
	}

	// Filter: empty, set, read, delete.
	code, body = e.do(t, "GET", feedPath+"/filter", e.user, e.pass, nil)
	if code != 200 || !strings.Contains(string(body), `"filter"`) {
		t.Fatalf("get filter = %d %s", code, body)
	}
	code, body = e.do(t, "POST", feedPath+"/filter", e.user, e.pass, map[string]any{"titleKeywords": "spam"})
	if code != 200 || !strings.Contains(string(body), "spam") {
		t.Fatalf("set filter = %d %s", code, body)
	}
	code, body = e.do(t, "DELETE", feedPath+"/filter", e.user, e.pass, nil)
	if code != 204 {
		t.Fatalf("delete filter = %d %s, want 204", code, body)
	}

	// Rules: set, read and delete.
	code, body = e.do(t, "POST", feedPath+"/rules", e.user, e.pass, map[string]any{"block": "EntryTitle=promo"})
	if code != 200 {
		t.Fatalf("set rules = %d %s", code, body)
	}
	code, body = e.do(t, "GET", feedPath+"/rules", e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("get rules = %d %s", code, body)
	}
	code, body = e.do(t, "DELETE", feedPath+"/rules", e.user, e.pass, nil)
	if code != 204 {
		t.Fatalf("delete rules = %d %s, want 204", code, body)
	}

	// Retention: read the override and set one.
	code, body = e.do(t, "GET", feedPath+"/retention", e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("get retention = %d %s", code, body)
	}
	code, body = e.do(t, "POST", feedPath+"/retention", e.user, e.pass, map[string]any{"retentionDays": 7})
	if code != 200 || !strings.Contains(string(body), `"retentionDays":7`) {
		t.Fatalf("set retention = %d %s", code, body)
	}

	// Mark read: the feed and everything.
	code, body = e.do(t, "POST", feedPath+"/read", e.user, e.pass, map[string]any{"newestItemId": 0})
	if code != 204 {
		t.Fatalf("mark feed read = %d %s, want 204", code, body)
	}
	code, body = e.do(t, "POST", "/items/read", e.user, e.pass, map[string]any{"newestItemId": 0})
	if code != 204 {
		t.Fatalf("mark all read = %d %s, want 204", code, body)
	}

	// Sharing: create the public link and remove it.
	itemsBody := bodyOf(t, e, "/items")
	itemID := extractJSONInt(t, itemsBody, `"id":`)
	code, body = e.do(t, "POST", "/items/"+itoa(itemID)+"/share", e.user, e.pass, nil)
	if code != 200 || !strings.Contains(string(body), `"token"`) {
		t.Fatalf("share = %d %s", code, body)
	}
	code, body = e.do(t, "DELETE", "/items/"+itoa(itemID)+"/share", e.user, e.pass, nil)
	if code != 204 {
		t.Fatalf("unshare = %d %s, want 204", code, body)
	}
}

// TestSavedSearchesCRUD covers the saved search handlers end to end.
func TestSavedSearchesCRUD(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	if code, body := e.do(t, "POST", "/feeds", e.user, e.pass, map[string]any{"url": "https://ss.example/rss"}); code != 200 {
		t.Fatalf("create feed = %d %s", code, body)
	}

	code, body := e.do(t, "POST", "/searches", e.user, e.pass, map[string]any{"name": "Items", "query": "Item"})
	if code != 200 {
		t.Fatalf("create search = %d %s", code, body)
	}
	searchID := extractJSONInt(t, body, `"id":`)

	code, body = e.do(t, "GET", "/searches", e.user, e.pass, nil)
	if code != 200 || !strings.Contains(string(body), "Items") {
		t.Fatalf("list searches = %d %s", code, body)
	}
	code, body = e.do(t, "GET", "/searches/"+itoa(searchID)+"/items", e.user, e.pass, nil)
	if code != 200 {
		t.Fatalf("search items = %d %s", code, body)
	}
	code, body = e.do(t, "DELETE", "/searches/"+itoa(searchID), e.user, e.pass, nil)
	if code != 204 {
		t.Fatalf("delete search = %d %s, want 204", code, body)
	}
}

func TestMarkMultipleAcceptsBothShapes(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	if code, body := e.do(t, "POST", "/feeds", e.user, e.pass, map[string]any{"url": "https://mm.example/rss"}); code != 200 {
		t.Fatalf("create feed = %d %s", code, body)
	}
	itemsBody := bodyOf(t, e, "/items")
	var listing struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	decode(t, itemsBody, &listing)
	if len(listing.Items) < 2 {
		t.Fatalf("items = %+v, the test needs at least two", listing)
	}
	a, b := listing.Items[0].ID, listing.Items[1].ID

	// The documented shape (itemIds).
	code, body := e.do(t, "POST", "/items/star/multiple", e.user, e.pass, map[string]any{"itemIds": []int64{a}})
	if code != 204 {
		t.Fatalf("star itemIds = %d %s, want 204", code, body)
	}
	// The legacy shape some clients send (items).
	code, body = e.do(t, "POST", "/items/unstar/multiple", e.user, e.pass, map[string]any{"items": []int64{a}})
	if code != 204 {
		t.Fatalf("unstar items = %d %s, want 204", code, body)
	}
	// Both shapes at once, marking read and unread.
	code, body = e.do(t, "POST", "/items/read/multiple", e.user, e.pass, map[string]any{"itemIds": []int64{a}, "items": []int64{b}})
	if code != 204 {
		t.Fatalf("read multiple = %d %s", code, body)
	}
	code, body = e.do(t, "POST", "/items/unread/multiple", e.user, e.pass, map[string]any{"itemIds": []int64{a, b}})
	if code != 204 {
		t.Fatalf("unread multiple = %d %s", code, body)
	}

	// An empty list is accepted and changes nothing.
	code, body = e.do(t, "POST", "/items/star/multiple", e.user, e.pass, map[string]any{"itemIds": []int64{}})
	if code != 204 {
		t.Fatalf("star with no ids = %d %s, want 204", code, body)
	}
	// An unusable body is rejected with the envelope.
	req, err := http.NewRequest("POST", e.ts.URL+Base+"/items/star/multiple", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(e.user, e.pass)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	broken := readBody(t, resp)
	if resp.StatusCode != 422 || errorCode(t, broken) != "invalid_body" {
		t.Fatalf("invalid body = %d %s", resp.StatusCode, broken)
	}
}

func TestMyRulesValidation(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})

	code, body := e.do(t, "PUT", "/api/me/rules", e.user, e.pass, map[string]any{"block": "EntryTitle=promo"})
	if code != 200 {
		t.Fatalf("set rules = %d %s", code, body)
	}
	code, body = e.do(t, "GET", "/api/me/rules", e.user, e.pass, nil)
	if code != 200 || !strings.Contains(string(body), "EntryTitle=promo") {
		t.Fatalf("get rules = %d %s", code, body)
	}
	// A field that does not exist is refused instead of stored.
	code, body = e.do(t, "PUT", "/api/me/rules", e.user, e.pass, map[string]any{"block": "Nope=x"})
	if code != 422 || errorCode(t, body) != "invalid_rule" {
		t.Fatalf("invalid rule = %d %s", code, body)
	}
}

func TestLogErrorEnvelope(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/x", nil)
	e.srv.logError(rec, req, "boom", errTest{})

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := rec.Body.Bytes()
	if errorCode(t, body) != "internal_error" {
		t.Fatalf("body = %s, want the internal_error code", body)
	}
}

// errTest is a plain error for the logError test.
type errTest struct{}

func (errTest) Error() string { return "boom" }
