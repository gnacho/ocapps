package store

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func newTestStore(t *testing.T) (*Store, int64) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hash, err := bcrypt.GenerateFromPassword([]byte("x"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	uid, err := st.CreateUser("u", string(hash), "u", "user")
	if err != nil {
		t.Fatal(err)
	}
	return st, uid
}

// TestPurgeOldItems: deletes read non-starred items older than the cutoff,
// it keeps unread, starred and recent ones.
func TestPurgeOldItems(t *testing.T) {
	st, uid := newTestStore(t)
	items := []NewItem{
		{GUID: "old-read", GUIDHash: "h1"},   // delete
		{GUID: "old-unread", GUIDHash: "h2"}, // conservar (unread)
		{GUID: "old-star", GUIDHash: "h3"},   // conservar (starred)
		{GUID: "new-read", GUIDHash: "h4"},   // conservar (reciente)
	}
	if _, err := st.CreateFeed(uid, "https://r.example/f", nil, "t", "https://r.example", "", items); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListItems(ItemFilter{UserID: uid, Type: 3, GetRead: true, BatchSize: -1})
	if err != nil || len(got) != 4 {
		t.Fatalf("setup: %v %d", err, len(got))
	}
	byGUID := map[string]int64{}
	for _, x := range got {
		byGUID[x.GUID] = x.ID
	}
	if err := st.MarkItemsUnreadFlag(uid, []int64{byGUID["old-read"], byGUID["new-read"]}, false); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkItemsStarFlag(uid, []int64{byGUID["old-star"]}, true); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).Unix()
	// age last_modified of the three "old" ones
	for _, guid := range []string{"old-read", "old-unread", "old-star"} {
		if _, err := st.db.Exec(`UPDATE items SET last_modified = ? WHERE id = ?`, old, byGUID[guid]); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.PurgeOldItems(time.Now().Add(-24 * time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("purge deleted %d, expected 1 (only old-read)", n)
	}
	rest, _ := st.ListItems(ItemFilter{UserID: uid, Type: 3, GetRead: true, BatchSize: -1})
	if len(rest) != 3 {
		t.Fatalf("%d remain, expected 3", len(rest))
	}
}

// TestListDueFeeds: only the overdue ones, ordered by next_update.
func TestListDueFeeds(t *testing.T) {
	st, uid := newTestStore(t)
	now := time.Now().Unix()
	a, _ := st.CreateFeed(uid, "https://a.example/f", nil, "a", "", "", nil)
	b, _ := st.CreateFeed(uid, "https://b.example/f", nil, "b", "", "", nil)
	_ = st.SetNextUpdate(a.ID, now-100)
	_ = st.SetNextUpdate(b.ID, now+1000)

	due, err := st.ListDueFeeds(now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != a.ID {
		t.Fatalf("due: %+v", due)
	}
}

// TestFeedByURLHash: lookup for the /favicon/{hash} endpoint.
func TestFeedByURLHash(t *testing.T) {
	st, uid := newTestStore(t)
	f, _ := st.CreateFeed(uid, "https://h.example/f", nil, "h", "", "", nil)
	if f.URLHash == "" {
		t.Fatal("empty url_hash")
	}
	row, err := st.FeedByURLHash(f.URLHash)
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != f.ID || row.UserID != uid {
		t.Fatalf("row: %+v", row)
	}
	if _, err := st.FeedByURLHash("missing"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, I have %v", err)
	}
}

// TestFeedFilter: stores/reads/deletes the filter, filters new items and reapplies it.
func TestFeedFilter(t *testing.T) {
	st, uid := newTestStore(t)
	f, _ := st.CreateFeed(uid, "https://fl.example/f", nil, "fl", "", "", nil)
	fid := f.ID

	// no filter: GetFeedFilter returns empty
	ff, err := st.GetFeedFilter(fid)
	if err != nil || ff.HasFilter() {
		t.Fatalf("filtro inicial: %+v err=%v", ff, err)
	}

	// store the filter
	filter := FeedFilter{FeedID: fid, TitleKeywords: "sponsored,ADs", BodyKeywords: "tracking", URLKeywords: "utm_"}
	if err := st.SaveFeedFilter(filter); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetFeedFilter(fid)
	if got.TitleKeywords != "sponsored,ADs" || !got.HasFilter() {
		t.Fatalf("filtro guardado: %+v", got)
	}

	// new items: the refresh flags the matching ones as filtered
	newItems := []NewItem{
		{GUID: "a", GUIDHash: "ha", Title: "normal", URL: "https://x.example/a", Body: "text"},
		{GUID: "b", GUIDHash: "hb", Title: "SPONSORED post", URL: "https://x.example/b", Body: "other"},
		{GUID: "c", GUIDHash: "hc", Title: "normal", URL: "https://x.example/c?utm_source=rss", Body: "text"},
		{GUID: "d", GUIDHash: "hd", Title: "normal", URL: "https://x.example/d", Body: "with tracking"},
	}
	if _, err := st.ReplaceFeedItems(fid, uid, "fl", "", newItems, false); err != nil {
		t.Fatal(err)
	}
	listed, _ := st.ListItems(ItemFilter{UserID: uid, Type: 0, ID: fid, GetRead: true, BatchSize: -1})
	if len(listed) != 1 {
		t.Fatalf("without IncludeFiltered 1 should remain (only 'a'), %d remain", len(listed))
	}
	if listed[0].GUID != "a" {
		t.Fatalf("unfiltered item expected: %s", listed[0].GUID)
	}
	all, _ := st.ListItems(ItemFilter{UserID: uid, Type: 0, ID: fid, GetRead: true, BatchSize: -1, IncludeFiltered: true})
	if len(all) != 4 {
		t.Fatalf("with IncludeFiltered the 4 should be there, there are %d", len(all))
	}
	filteredCount := 0
	for _, it := range all {
		if it.Filtered {
			filteredCount++
		}
	}
	if filteredCount != 3 {
		t.Fatalf("expected 3 filtered (b,c,d), there are %d", filteredCount)
	}

	// remove keywords -> unfreeze (ReapplyFeedFilter with an empty filter)
	if err := st.SaveFeedFilter(FeedFilter{FeedID: fid}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReapplyFeedFilter(fid, FeedFilter{FeedID: fid}); err != nil {
		t.Fatal(err)
	}
	after, _ := st.ListItems(ItemFilter{UserID: uid, Type: 0, ID: fid, GetRead: true, BatchSize: -1})
	if len(after) != 4 {
		t.Fatalf("after unfreezing 4 should be visible, there are %d", len(after))
	}
}

// TestSearchItems: query search in title/body/URL, with scoping.
func TestSearchItems(t *testing.T) {
	st, uid := newTestStore(t)
	_, _ = st.CreateFeed(uid, "https://s.example/f", nil, "s", "", "", []NewItem{
		{GUID: "a", GUIDHash: "ha", Title: "How to bake bread", URL: "https://s.example/bread", Body: "flour and water"},
		{GUID: "b", GUIDHash: "hb", Title: "Go vs Rust", URL: "https://s.example/go", Body: "performance"},
		{GUID: "c", GUIDHash: "hc", Title: "Tennis news", URL: "https://s.example/tennis", Body: "rafa nadal"},
	})

	res, err := st.SearchItems(ItemFilter{UserID: uid, Type: 3, GetRead: true}, "bake", 10)
	if err != nil || len(res) != 1 || res[0].GUID != "a" {
		t.Fatalf("buscar 'bake': %v %+v", err, res)
	}
	res, _ = st.SearchItems(ItemFilter{UserID: uid, Type: 3, GetRead: true}, "performance", 10)
	if len(res) != 1 || res[0].GUID != "b" {
		t.Fatalf("buscar en body: %+v", res)
	}
	res, _ = st.SearchItems(ItemFilter{UserID: uid, Type: 3, GetRead: true}, "tennis", 10)
	if len(res) != 1 || res[0].GUID != "c" {
		t.Fatalf("search by URL: %+v", res)
	}
	// case-insensitive and multiple
	res, _ = st.SearchItems(ItemFilter{UserID: uid, Type: 3, GetRead: true}, "RUST", 10)
	if len(res) != 1 {
		t.Fatalf("case-insensitive: %+v", res)
	}
	// no results
	res, _ = st.SearchItems(ItemFilter{UserID: uid, Type: 3, GetRead: true}, "zzz", 10)
	if len(res) != 0 {
		t.Fatalf("no results expected: %+v", res)
	}
}

// TestFeedRetention: a per-feed override is excluded from the global purge and purges on its own.
func TestFeedRetention(t *testing.T) {
	st, uid := newTestStore(t)
	f1, _ := st.CreateFeed(uid, "https://r1.example/f", nil, "r1", "", "", []NewItem{
		{GUID: "a", GUIDHash: "ha", Title: "a", URL: "https://r1.example/a"},
	})
	f2, _ := st.CreateFeed(uid, "https://r2.example/f", nil, "r2", "", "", []NewItem{
		{GUID: "b", GUIDHash: "hb", Title: "b", URL: "https://r2.example/b"},
	})
	// mark as read and age
	for _, g := range []string{"a", "b"} {
		id := 0
		_ = st.db.QueryRow(`SELECT id FROM items WHERE guid = ?`, g).Scan(&id)
		_ = st.MarkItemsUnreadFlag(uid, []int64{int64(id)}, false)
		_, _ = st.db.Exec(`UPDATE items SET last_modified = ? WHERE id = ?`, time.Now().Add(-48*time.Hour).Unix(), id)
	}
	// f1 with a 1 day override -> it purges on its own, f2 without override -> uses global
	if err := st.SetFeedRetentionDays(f1.ID, uid, 1); err != nil {
		t.Fatal(err)
	}
	over, err := st.FeedsWithRetentionOverride()
	if err != nil || len(over) != 1 || over[0].ID != f1.ID || over[0].Days != 1 {
		t.Fatalf("overrides: %+v err=%v", over, err)
	}
	// per-feed purge (1 day override, 24h cutoff) -> deletes the f1 item
	n, err := st.PurgeOldItemsByFeed(f1.ID, time.Now().Add(-24*time.Hour).Unix())
	if err != nil || n != 1 {
		t.Fatalf("per-feed purge: n=%d err=%v", n, err)
	}
	// the global purge does NOT touch f2 (its item is still old but f2 has no override
	// and the 7 day global cutoff does not reach it with 48h)
	n, err = st.PurgeOldItems(time.Now().Add(-7 * 24 * time.Hour).Unix())
	if err != nil || n != 0 {
		t.Fatalf("global purge should not delete: n=%d err=%v", n, err)
	}
	// check that f1 is empty and f2 keeps its item
	if c, _ := st.CountItems(ItemFilter{UserID: uid, Type: 0, ID: f1.ID, GetRead: true}); c != 0 {
		t.Fatalf("f1 should be empty, there are %d", c)
	}
	if c, _ := st.CountItems(ItemFilter{UserID: uid, Type: 0, ID: f2.ID, GetRead: true}); c != 1 {
		t.Fatalf("f2 should keep 1 item, there are %d", c)
	}
}

// TestUserSettings: key-value setting UPSERT per user.
func TestUserSettings(t *testing.T) {
	st, uid := newTestStore(t)
	if v, _ := st.GetUserSetting(uid, "theme"); v != "" {
		t.Fatalf("initial setting should be empty: %q", v)
	}
	if err := st.SetUserSetting(uid, "theme", "dark"); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.GetUserSetting(uid, "theme"); v != "dark" {
		t.Fatalf("theme: %q", v)
	}
	// upsert
	_ = st.SetUserSetting(uid, "theme", "light")
	if v, _ := st.GetUserSetting(uid, "theme"); v != "light" {
		t.Fatalf("theme after upsert: %q", v)
	}
	// delete with an empty value
	_ = st.SetUserSetting(uid, "theme", "")
	if v, _ := st.GetUserSetting(uid, "theme"); v != "" {
		t.Fatalf("theme after delete: %q", v)
	}
}

// TestPodcastDetection: a feed with an audio enclosure is flagged as is_podcast.
func TestPodcastDetection(t *testing.T) {
	st, uid := newTestStore(t)
	mime := "audio/mpeg"
	link := "https://pod.example/ep1.mp3"
	f, _ := st.CreateFeed(uid, "https://pod.example/feed", nil, "pod", "", "", []NewItem{
		{GUID: "a", GUIDHash: "ha", Title: "Ep 1", URL: "https://pod.example/1", EnclosureMime: &mime, EnclosureLink: &link},
	})
	if !f.IsPodcast {
		t.Fatalf("feed with audio should be a podcast: %+v", f)
	}
	// a feed without enclosures is not a podcast
	f2, _ := st.CreateFeed(uid, "https://txt.example/feed", nil, "txt", "", "", []NewItem{
		{GUID: "b", GUIDHash: "hb", Title: "post", URL: "https://txt.example/1"},
	})
	if f2.IsPodcast {
		t.Fatalf("feed without audio should not be a podcast: %+v", f2)
	}
}
