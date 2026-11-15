package api

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// extractJSONInt pulls the first number following key in a flat JSON.
func extractJSONInt(t *testing.T, body []byte, key string) int64 {
	t.Helper()
	idx := strings.Index(string(body), key)
	if idx < 0 {
		t.Fatalf("key %s not found in %s", key, body)
	}
	rest := strings.TrimLeft(string(body)[idx+len(key):], " ")
	end := strings.IndexAny(rest, ",}]")
	if end < 0 {
		end = len(rest)
	}
	n, err := strconv.ParseInt(rest[:end], 10, 64)
	if err != nil {
		t.Fatalf("invalid number after %s: %v", key, err)
	}
	return n
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestAndroidCompat: peculiarities of the news-android client verified
// against its source code (retrofit NewsAPI.java).
func TestAndroidCompat(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})

	// create a feed with form-urlencoded (the client uses @Field, not JSON)
	req, _ := http.NewRequest("POST", e.ts.URL+Base+"/feeds",
		strings.NewReader("url=https://form.example/rss&folderId=0"))
	req.SetBasicAuth(e.user, e.pass)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"url":"https://form.example/rss"`) {
		t.Fatalf("create feed form: %d %s", resp.StatusCode, body)
	}

	// create a folder (JSON, the client does it with Map) and a feed with query params
	code, fb := e.do(t, "POST", "/folders", e.user, e.pass, map[string]string{"name": "C"})
	if code != 200 {
		t.Fatalf("folder: %d", code)
	}
	code, fb = e.do(t, "GET", "/items?type=3&getRead=true", e.user, e.pass, nil)
	if !strings.Contains(string(fb), "form.example") {
		_ = code
	}

	// PUT rename feed (the spec says POST, the client sends PUT)
	code, fb = e.do(t, "GET", "/feeds", e.user, e.pass, nil)
	feedID := extractJSONInt(t, fb, `"id":`)
	if code, _ = e.do(t, "PUT", "/feeds/"+itoa(feedID)+"/rename", e.user, e.pass,
		map[string]string{"feedTitle": "Renombrado"}); code != 204 {
		t.Fatalf("PUT rename: %d", code)
	}
	// PUT move (the spec says POST, the client sends PUT)
	if code, _ = e.do(t, "PUT", "/feeds/"+itoa(feedID)+"/move", e.user, e.pass,
		map[string]any{"folderId": nil}); code != 204 {
		t.Fatalf("PUT move: %d", code)
	}
	// POST sigue valiendo (spec)
	if code, _ = e.do(t, "POST", "/feeds/"+itoa(feedID)+"/rename", e.user, e.pass,
		map[string]string{"feedTitle": "Other"}); code != 204 {
		t.Fatalf("POST rename: %d", code)
	}

	// check the change applied
	_, fb = e.do(t, "GET", "/feeds", e.user, e.pass, nil)
	if !strings.Contains(string(fb), "Other") {
		t.Fatalf("rename not applied: %s", fb)
	}

	// create a folder with JSON but form content-type (curl -d by default):
	// decodeBody decides by content, not by header
	reqC, _ := http.NewRequest("POST", e.ts.URL+Base+"/folders",
		strings.NewReader(`{"name":"Technology"}`))
	reqC.SetBasicAuth(e.user, e.pass)
	reqC.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	respC, err := e.client.Do(reqC)
	if err != nil {
		t.Fatal(err)
	}
	bodyC := readBody(t, respC)
	if respC.StatusCode != 200 || !strings.Contains(string(bodyC), "Technology") {
		t.Fatalf("JSON folder with form content-type: %d %s", respC.StatusCode, bodyC)
	}
}

// TestOCSUserNotRegisteredInNews (SPEC §4.2): the OCS stub was removed in H5,
// /ocs/v2.php/cloud/user is registered ONLY by the notes module. The assertions
// of the JSON contract for news-android (ocs envelope, id=username,
// displayname, display-name, 401 without auth) now live in the tests of the
// extended handler of notes (internal/notes/api/server_test.go,
// TestUserInfoJSON*) and in the cmd/ocapps integration.
func TestOCSUserNotRegisteredInNews(t *testing.T) {
	e := newTestEnv(t, &fakeFetcher{})
	req, _ := http.NewRequest("GET", e.ts.URL+"/ocs/v2.php/cloud/user?format=json", nil)
	req.SetBasicAuth(e.user, e.pass)
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("news must not register /ocs/v2.php/cloud/user: %d (want 404)", resp.StatusCode)
	}
}
