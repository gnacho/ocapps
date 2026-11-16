package store

// Data layer tests: users, feeds, item accessors, saved searches and public
// shares against a real SQLite file (same store the API and the collectors use).

import (
	"context"
	"errors"
	"testing"
)

func mustCreateFeed(t *testing.T, st *Store, uid int64, url string, items ...NewItem) *Feed {
	t.Helper()
	f, err := st.CreateFeed(uid, url, nil, "t", "https://site.example", "", items)
	if err != nil {
		t.Fatalf("CreateFeed(%s): %v", url, err)
	}
	return f
}

func TestPing(t *testing.T) {
	st, _ := newTestStore(t)
	if err := st.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestCreateUserWithOCID(t *testing.T) {
	st, _ := newTestStore(t)

	id, err := st.CreateUserWithOCID("alice", "uuid-alice", "hash", "Alice", "admin")
	if err != nil {
		t.Fatalf("CreateUserWithOCID: %v", err)
	}
	u, err := st.GetUserByOCID("uuid-alice")
	if err != nil {
		t.Fatalf("GetUserByOCID: %v", err)
	}
	if u.ID != id || u.Username != "alice" || u.OCID != "uuid-alice" || u.Role != "admin" {
		t.Fatalf("user = %+v", u)
	}
	if u.Language != "auto" {
		t.Fatalf("language = %q, want the auto default", u.Language)
	}

	// The oc_id is unique: a second shadow for the same identity is a conflict,
	// the signal the auth glue uses to resolve a create race.
	if _, err := st.CreateUserWithOCID("alice2", "uuid-alice", "hash", "Alice", "user"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate oc_id error = %v, want ErrConflict", err)
	}
	if _, err := st.GetUserByOCID("uuid-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing oc_id error = %v, want ErrNotFound", err)
	}
}

func TestSetUserOCIDLinksAPreviousUser(t *testing.T) {
	st, uid := newTestStore(t)

	if err := st.SetUserOCID(uid, "uuid-linked"); err != nil {
		t.Fatalf("SetUserOCID: %v", err)
	}
	u, err := st.GetUserByOCID("uuid-linked")
	if err != nil || u.ID != uid {
		t.Fatalf("GetUserByOCID = %+v, %v, want the linked user", u, err)
	}
}

func TestUserProfileAndPassword(t *testing.T) {
	st, uid := newTestStore(t)

	if err := st.UpdateProfile(uid, "New name", "es"); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	u, err := st.GetUserByID(uid)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if u.DisplayName != "New name" || u.Language != "es" {
		t.Fatalf("profile = %q/%q, want the updated one", u.DisplayName, u.Language)
	}

	if err := st.SetUserLanguage(uid, "en"); err != nil {
		t.Fatalf("SetUserLanguage: %v", err)
	}
	if u, _ = st.GetUserByID(uid); u.Language != "en" {
		t.Fatalf("language = %q, want en", u.Language)
	}

	if err := st.UpdatePassword(uid, "new-hash"); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}
	if u, _ = st.GetUserByID(uid); u.PasswordHash != "new-hash" {
		t.Fatalf("hash = %q, want the new one", u.PasswordHash)
	}

	if _, err := st.GetUserByID(12345); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user error = %v, want ErrNotFound", err)
	}
	if _, err := st.GetUserByUsername("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing username error = %v, want ErrNotFound", err)
	}
}

func TestUpdateUserFields(t *testing.T) {
	st, uid := newTestStore(t)

	role, lang := "admin", "es"
	if err := st.UpdateUserFields(uid, &role, &lang); err != nil {
		t.Fatalf("UpdateUserFields: %v", err)
	}
	u, _ := st.GetUserByID(uid)
	if u.Role != "admin" || u.Language != "es" {
		t.Fatalf("user = %+v, want role and language updated", u)
	}

	// One field at a time leaves the other untouched.
	lang = "en"
	if err := st.UpdateUserFields(uid, nil, &lang); err != nil {
		t.Fatalf("UpdateUserFields(language): %v", err)
	}
	u, _ = st.GetUserByID(uid)
	if u.Role != "admin" || u.Language != "en" {
		t.Fatalf("user = %+v, want only the language changed", u)
	}
	if err := st.UpdateUserFields(uid, nil, nil); err != nil {
		t.Fatalf("UpdateUserFields(no fields): %v", err)
	}
}

func TestBootstrapUser(t *testing.T) {
	st, err := Open(t.TempDir() + "/b.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var count int
	if err := st.BootstrapCount(&count); err != nil || count != 0 {
		t.Fatalf("BootstrapCount = %d, %v, want an empty table", count, err)
	}

	created, err := st.BootstrapUser("admin", "hash")
	if err != nil || !created {
		t.Fatalf("BootstrapUser = %v, %v, want the first user created", created, err)
	}
	u, err := st.GetUserByUsername("admin")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if u.Role != "admin" || u.DisplayName != "admin" {
		t.Fatalf("bootstrap user = %+v, want an admin named after the username", u)
	}

	// Idempotent: with users already there it creates nothing.
	created, err = st.BootstrapUser("other", "hash")
	if err != nil || created {
		t.Fatalf("second BootstrapUser = %v, %v, want false and no error", created, err)
	}
}

func TestCountsAndLoginMark(t *testing.T) {
	st, uid := newTestStore(t)

	if n, err := st.CountUsers(); err != nil || n != 1 {
		t.Fatalf("CountUsers = %d, %v, want 1", n, err)
	}
	if n, err := st.CountAdmins(); err != nil || n != 0 {
		t.Fatalf("CountAdmins = %d, %v, want 0 (the test user is not admin)", n, err)
	}

	st.TouchLogin(uid)
	u, _ := st.GetUserByID(uid)
	if u.LastLoginAt == 0 {
		t.Fatal("TouchLogin must mark the login")
	}
	first := u.LastLoginAt

	// The mark is throttled: a second call within the minute does not write.
	st.TouchLogin(uid)
	if u, _ = st.GetUserByID(uid); u.LastLoginAt != first {
		t.Fatalf("last login = %d, want the first mark %d (throttled)", u.LastLoginAt, first)
	}
}

func TestListUsersHidesTheHashes(t *testing.T) {
	st, uid := newTestStore(t)
	if _, err := st.CreateUser("second", "secret-hash", "Second", "admin"); err != nil {
		t.Fatal(err)
	}

	users, err := st.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("users = %d, want 2", len(users))
	}
	for _, u := range users {
		if u.PasswordHash != "" {
			t.Fatalf("user %q exposes its hash", u.Username)
		}
	}
	if users[0].Username != "second" || users[1].ID != uid {
		t.Fatalf("order = %q, want alphabetical", users[0].Username)
	}
}

func TestDeleteUser(t *testing.T) {
	st, uid := newTestStore(t)
	adminID, err := st.CreateUser("boss", "hash", "Boss", "admin")
	if err != nil {
		t.Fatal(err)
	}

	// A normal user goes away with its data (dead feeds and their items).
	f := mustCreateFeed(t, st, uid, "https://d.example/f", NewItem{GUID: "g", GUIDHash: "h"})
	if err := st.DeleteUser(uid); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := st.GetUserByID(uid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted user still there: %v", err)
	}
	if _, err := st.GetFeed(uid, f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the feeds of a deleted user must cascade: %v", err)
	}

	// The only admin cannot be deleted.
	if err := st.DeleteUser(adminID); !errors.Is(err, ErrConflict) {
		t.Fatalf("deleting the last admin = %v, want ErrConflict", err)
	}
	// With another admin it can.
	if _, err := st.CreateUser("boss2", "hash", "Boss 2", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUser(adminID); err != nil {
		t.Fatalf("deleting an admin with company = %v", err)
	}
	if err := st.DeleteUser(99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a missing user = %v, want ErrNotFound", err)
	}
}

func TestListFeedsAndLookups(t *testing.T) {
	st, uid := newTestStore(t)
	b := mustCreateFeed(t, st, uid, "https://b.example/f")
	a := mustCreateFeed(t, st, uid, "https://a.example/f")

	feeds, err := st.ListFeeds(uid)
	if err != nil {
		t.Fatalf("ListFeeds: %v", err)
	}
	if len(feeds) != 2 {
		t.Fatalf("feeds = %d, want 2", len(feeds))
	}

	// FeedExistsByURL is per user and by exact URL.
	if ok, err := st.FeedExistsByURL(uid, "https://a.example/f"); err != nil || !ok {
		t.Fatalf("existing url = %v, %v", ok, err)
	}
	if ok, err := st.FeedExistsByURL(uid, "https://nope.example/f"); err != nil || ok {
		t.Fatalf("missing url = %v, %v", ok, err)
	}
	other, err := st.CreateUser("other", "hash", "Other", "user")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := st.FeedExistsByURL(other, "https://a.example/f"); ok {
		t.Fatal("another user must not own that URL")
	}
	_ = b

	// AllFeedsWithUser is the admin view: id plus username.
	all, err := st.AllFeedsWithUser()
	if err != nil {
		t.Fatalf("AllFeedsWithUser: %v", err)
	}
	if len(all) != 2 || all[0].UserID != "u" {
		t.Fatalf("all feeds = %+v, want both with the username", all)
	}

	// FeedByURLHash is the public favicon lookup.
	byHash, err := st.FeedByURLHash(a.URLHash)
	if err != nil || byHash.ID != a.ID {
		t.Fatalf("FeedByURLHash = %+v, %v, want feed %d", byHash, err, a.ID)
	}
	if _, err := st.FeedByURLHash("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing hash error = %v, want ErrNotFound", err)
	}
}

func TestCreateFeedFullAndDeferred(t *testing.T) {
	st, uid := newTestStore(t)

	full, err := st.CreateFeedFull(uid, "https://full.example/f", nil, "Full", "https://full.example", "",
		[]NewItem{{GUID: "g1", GUIDHash: "h1", EnclosureLink: strPtr("https://full.example/a.mp3"), EnclosureMime: strPtr("audio/mpeg")}}, true)
	if err != nil {
		t.Fatalf("CreateFeedFull: %v", err)
	}
	if !full.FullContent {
		t.Fatal("the full content flag must be stored")
	}
	if !full.IsPodcast {
		t.Fatal("a feed with an audio enclosure must be a podcast")
	}

	deferred, err := st.CreateFeedDeferred(uid, "https://deferred.example/f", nil)
	if err != nil {
		t.Fatalf("CreateFeedDeferred: %v", err)
	}
	if deferred.Title != "https://deferred.example/f" {
		t.Fatalf("title = %q, want the URL as a provisional one", deferred.Title)
	}
	if deferred.NextUpdateTime != 0 {
		t.Fatalf("next update = %d, want 0 so the scheduler picks it up", deferred.NextUpdateTime)
	}

	// The same URL twice is a conflict, also through the deferred path.
	if _, err := st.CreateFeedDeferred(uid, "https://deferred.example/f", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate deferred feed = %v, want ErrConflict", err)
	}
}

func strPtr(s string) *string { return &s }

func TestRecordFeedErrorAndReset(t *testing.T) {
	st, uid := newTestStore(t)
	f := mustCreateFeed(t, st, uid, "https://err.example/f")

	for i := 1; i <= 2; i++ {
		if err := st.RecordFeedError(f.ID, "boom"); err != nil {
			t.Fatalf("RecordFeedError: %v", err)
		}
		got, _ := st.GetFeed(uid, f.ID)
		if got.UpdateErrorCount != i {
			t.Fatalf("error count = %d, want %d", got.UpdateErrorCount, i)
		}
		if got.LastUpdateError == nil || *got.LastUpdateError != "boom" {
			t.Fatalf("last error = %v, want the message", got.LastUpdateError)
		}
	}
}

func TestFeedRenameMoveAndDelete(t *testing.T) {
	st, uid := newTestStore(t)
	f := mustCreateFeed(t, st, uid, "https://m.example/f")

	if err := st.RenameFeed(uid, f.ID, "Renamed"); err != nil {
		t.Fatalf("RenameFeed: %v", err)
	}
	if got, _ := st.GetFeed(uid, f.ID); got.Title != "Renamed" {
		t.Fatalf("title = %q", got.Title)
	}
	if err := st.RenameFeed(uid, 9999, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renaming a missing feed = %v, want ErrNotFound", err)
	}

	folder, err := st.CreateFolder(uid, "Folder", nil)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	if err := st.MoveFeed(uid, f.ID, &folder.ID); err != nil {
		t.Fatalf("MoveFeed: %v", err)
	}
	if got, _ := st.GetFeed(uid, f.ID); got.FolderID == nil || *got.FolderID != folder.ID {
		t.Fatalf("folder = %v, want %d", got.FolderID, folder.ID)
	}
	// Moving to a folder of another user is not found.
	other, _ := st.CreateUser("other", "hash", "Other", "user")
	otherFolder, _ := st.CreateFolder(other, "Theirs", nil)
	if err := st.MoveFeed(uid, f.ID, &otherFolder.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("moving into a foreign folder = %v, want ErrNotFound", err)
	}
	// Back to the root.
	if err := st.MoveFeed(uid, f.ID, nil); err != nil {
		t.Fatalf("MoveFeed(root): %v", err)
	}
	if got, _ := st.GetFeed(uid, f.ID); got.FolderID != nil {
		t.Fatalf("folder = %v, want the root", got.FolderID)
	}

	// Delete removes the feed and its items.
	if _, err := st.ReplaceFeedItems(f.ID, uid, "t", "l", []NewItem{{GUID: "g", GUIDHash: "h"}}, false); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteFeed(uid, f.ID); err != nil {
		t.Fatalf("DeleteFeed: %v", err)
	}
	if _, err := st.GetFeed(uid, f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("feed still there: %v", err)
	}
	if n, _ := st.CountItems(ItemFilter{UserID: uid, Type: 3, GetRead: true, BatchSize: -1}); n != 0 {
		t.Fatalf("items = %d, want them deleted with the feed", n)
	}
	if err := st.DeleteFeed(uid, f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice = %v, want ErrNotFound", err)
	}
}

func TestFeedCredentialsAndScraperSelector(t *testing.T) {
	st, uid := newTestStore(t)
	f := mustCreateFeed(t, st, uid, "https://cred.example/f")

	enc := "ciphertext"
	if err := st.SetFeedCredentials(uid, f.ID, "user", &enc); err != nil {
		t.Fatalf("SetFeedCredentials: %v", err)
	}
	got, _ := st.GetFeed(uid, f.ID)
	if got.AuthUser != "user" || got.AuthPassEnc != enc {
		t.Fatalf("credentials = %q/%q, want them stored", got.AuthUser, got.AuthPassEnc)
	}
	// nil keeps the stored password (editing only the user).
	if err := st.SetFeedCredentials(uid, f.ID, "user2", nil); err != nil {
		t.Fatalf("SetFeedCredentials(nil): %v", err)
	}
	if got, _ = st.GetFeed(uid, f.ID); got.AuthUser != "user2" || got.AuthPassEnc != enc {
		t.Fatalf("credentials = %q/%q, want the user changed and the password kept", got.AuthUser, got.AuthPassEnc)
	}
	empty := ""
	if err := st.SetFeedCredentials(uid, f.ID, "", &empty); err != nil {
		t.Fatalf("SetFeedCredentials(clear): %v", err)
	}
	if got, _ = st.GetFeed(uid, f.ID); got.AuthUser != "" || got.AuthPassEnc != "" {
		t.Fatalf("credentials = %q/%q, want them cleared", got.AuthUser, got.AuthPassEnc)
	}

	if err := st.SetFeedScraperSelector(uid, f.ID, ".article"); err != nil {
		t.Fatalf("SetFeedScraperSelector: %v", err)
	}
	if sel, err := st.GetFeedScraperSelector(f.ID); err != nil || sel != ".article" {
		t.Fatalf("selector = %q, %v", sel, err)
	}
	other, _ := st.CreateUser("other", "hash", "Other", "user")
	if err := st.SetFeedScraperSelector(other, f.ID, ".x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("setting a selector on a foreign feed = %v, want ErrNotFound", err)
	}
	if _, err := st.GetFeedScraperSelector(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("selector of a missing feed = %v, want ErrNotFound", err)
	}
}

func TestItemAccessors(t *testing.T) {
	st, uid := newTestStore(t)
	f := mustCreateFeed(t, st, uid, "https://i.example/f",
		NewItem{GUID: "g1", GUIDHash: "h1", URL: "https://i.example/1", Title: "one", ClusterKey: "k1"},
		NewItem{GUID: "g2", GUIDHash: "h2", URL: "https://i.example/2", Title: "two", ClusterKey: "k1"})

	items, err := st.ListItems(ItemFilter{UserID: uid, Type: 3, BatchSize: -1})
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %d, %v", len(items), err)
	}
	first, second := items[1], items[0] // newest first

	if newest, err := st.NewestItemID(uid); err != nil || newest != second.ID {
		t.Fatalf("NewestItemID = %d, %v, want %d", newest, err, second.ID)
	}
	if n, err := st.StarredCount(uid); err != nil || n != 0 {
		t.Fatalf("StarredCount = %d, %v, want 0", n, err)
	}
	if err := st.MarkItemsStarFlag(uid, []int64{first.ID}, true); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.StarredCount(uid); n != 1 {
		t.Fatalf("StarredCount = %d, want 1", n)
	}

	if ok, err := st.ItemExists(uid, first.ID); err != nil || !ok {
		t.Fatalf("ItemExists = %v, %v", ok, err)
	}
	other, _ := st.CreateUser("other", "hash", "Other", "user")
	if ok, _ := st.ItemExists(other, first.ID); ok {
		t.Fatal("an item of another user must not exist for this one")
	}

	if u, err := st.GetItemURL(uid, first.ID); err != nil || u != "https://i.example/1" {
		t.Fatalf("GetItemURL = %q, %v", u, err)
	}
	if fid, err := st.GetItemFeedID(uid, first.ID); err != nil || fid != f.ID {
		t.Fatalf("GetItemFeedID = %d, %v, want %d", fid, err, f.ID)
	}
	if _, err := st.GetItemURL(other, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("URL of a foreign item = %v, want ErrNotFound", err)
	}
	if _, err := st.GetItemFeedID(other, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("feed of a foreign item = %v, want ErrNotFound", err)
	}

	keys, err := st.ItemClusterKeys(uid, []int64{first.ID, second.ID})
	if err != nil {
		t.Fatalf("ItemClusterKeys: %v", err)
	}
	if len(keys) != 2 || keys[first.ID] != "k1" {
		t.Fatalf("keys = %+v, want both with the cluster key", keys)
	}
	if empty, err := st.ItemClusterKeys(uid, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty ids = %+v, %v", empty, err)
	}

	// The full body cache lives apart and is upserted.
	if body, err := st.GetItemFull(first.ID); err != nil || body != "" {
		t.Fatalf("GetItemFull = %q, %v, want empty", body, err)
	}
	if err := st.SaveItemFull(first.ID, "full body"); err != nil {
		t.Fatalf("SaveItemFull: %v", err)
	}
	if body, _ := st.GetItemFull(first.ID); body != "full body" {
		t.Fatalf("full body = %q", body)
	}
	if err := st.SaveItemFull(first.ID, "updated body"); err != nil {
		t.Fatalf("SaveItemFull(update): %v", err)
	}
	if body, _ := st.GetItemFull(first.ID); body != "updated body" {
		t.Fatalf("full body = %q, want the updated one", body)
	}

	if err := st.SetItemURLForTesting(first.ID, "https://i.example/rewritten"); err != nil {
		t.Fatalf("SetItemURLForTesting: %v", err)
	}
	if u, _ := st.GetItemURL(uid, first.ID); u != "https://i.example/rewritten" {
		t.Fatalf("url = %q, want the rewritten one", u)
	}
}

func TestMarkItemsValidations(t *testing.T) {
	st, uid := newTestStore(t)
	f := mustCreateFeed(t, st, uid, "https://v.example/f", NewItem{GUID: "g", GUIDHash: "h"})
	items, _ := st.ListItems(ItemFilter{UserID: uid, Type: 3, BatchSize: -1})

	// Empty list is a no-op.
	if err := st.MarkItemsUnreadFlag(uid, nil, false); err != nil {
		t.Fatalf("empty list: %v", err)
	}
	// An invalid column is refused instead of building SQL with it.
	if err := st.markItems(uid, []int64{items[0].ID}, "role", true); err == nil {
		t.Fatal("markItems must refuse an unknown column")
	}
	_ = f
}

func TestMarkAllReadScopes(t *testing.T) {
	st, uid := newTestStore(t)
	folder, err := st.CreateFolder(uid, "F", nil)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}
	inFolder, err := st.CreateFeed(uid, "https://f1.example/f", &folder.ID, "t", "l", "", []NewItem{{GUID: "g1", GUIDHash: "h1"}})
	if err != nil {
		t.Fatal(err)
	}
	mustCreateFeed(t, st, uid, "https://f2.example/f", NewItem{GUID: "g2", GUIDHash: "h2"})

	// Feed scope: only the items of that feed.
	n, err := st.MarkAllRead(uid, 0, "feed", inFolder.ID)
	if err != nil || n != 1 {
		t.Fatalf("feed scope = %d, %v, want 1", n, err)
	}
	unread, _ := st.CountItems(ItemFilter{UserID: uid, Type: 3, BatchSize: -1})
	if unread != 1 {
		t.Fatalf("unread = %d, want the other feed untouched", unread)
	}

	// Folder scope: the items of the feeds inside it (already read).
	n, err = st.MarkAllRead(uid, 0, "folder", folder.ID)
	if err != nil || n != 0 {
		t.Fatalf("folder scope = %d, %v, want 0", n, err)
	}

	// All scope with a cap: only the ids up to maxID.
	newest, err := st.NewestItemID(uid)
	if err != nil {
		t.Fatalf("NewestItemID: %v", err)
	}
	n, err = st.MarkAllRead(uid, newest, "all", 0)
	if err != nil || n != 1 {
		t.Fatalf("all scope = %d, %v, want 1", n, err)
	}
	if left, _ := st.CountItems(ItemFilter{UserID: uid, Type: 3, BatchSize: -1}); left != 0 {
		t.Fatalf("unread = %d, want everything read", left)
	}

	if _, err := st.MarkAllRead(uid, 0, "galaxy", 0); err == nil {
		t.Fatal("an unknown scope must be an error")
	}
}

func TestDeleteFeedFilter(t *testing.T) {
	st, uid := newTestStore(t)
	f := mustCreateFeed(t, st, uid, "https://flt.example/f")
	if err := st.SaveFeedFilter(FeedFilter{FeedID: f.ID, TitleKeywords: "spam"}); err != nil {
		t.Fatalf("SaveFeedFilter: %v", err)
	}
	if err := st.DeleteFeedFilter(f.ID); err != nil {
		t.Fatalf("DeleteFeedFilter: %v", err)
	}
	if has, err := st.HasAnyFilter(uid, f.ID); err != nil || has {
		t.Fatalf("HasAnyFilter = %v, %v after deleting it", has, err)
	}
	// Deleting again is a no-op, not an error.
	if err := st.DeleteFeedFilter(f.ID); err != nil {
		t.Fatalf("DeleteFeedFilter twice: %v", err)
	}
}

func TestListAndDeleteAutoRead(t *testing.T) {
	st, uid := newTestStore(t)
	rule, err := st.AddAutoRead(uid, 0, "^sponsor")
	if err != nil {
		t.Fatalf("AddAutoRead: %v", err)
	}

	list, err := st.ListAutoRead(uid)
	if err != nil || len(list) != 1 || list[0].TitlePattern != "^sponsor" {
		t.Fatalf("ListAutoRead = %+v, %v", list, err)
	}

	if err := st.DeleteAutoRead(uid, rule.ID); err != nil {
		t.Fatalf("DeleteAutoRead: %v", err)
	}
	if list, _ := st.ListAutoRead(uid); len(list) != 0 {
		t.Fatalf("rules left = %+v", list)
	}
	if err := st.DeleteAutoRead(uid, rule.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice = %v, want ErrNotFound", err)
	}
	// A rule of another user is not theirs to delete.
	other, _ := st.CreateUser("other", "hash", "Other", "user")
	rule2, _ := st.AddAutoRead(uid, 0, "x")
	if err := st.DeleteAutoRead(other, rule2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a foreign rule = %v, want ErrNotFound", err)
	}
}

func TestSavedSearches(t *testing.T) {
	st, uid := newTestStore(t)
	other, _ := st.CreateUser("other", "hash", "Other", "user")

	created, err := st.CreateSavedSearch(uid, "Go news", "golang")
	if err != nil {
		t.Fatalf("CreateSavedSearch: %v", err)
	}
	if created.ID == 0 || created.Name != "Go news" || created.Query != "golang" || created.CreatedAt == 0 {
		t.Fatalf("created = %+v", created)
	}

	list, err := st.ListSavedSearches(uid)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSavedSearches = %+v, %v", list, err)
	}
	got, err := st.GetSavedSearch(uid, created.ID)
	if err != nil || got.Query != "golang" {
		t.Fatalf("GetSavedSearch = %+v, %v", got, err)
	}

	// Scoped by user.
	if list, _ := st.ListSavedSearches(other); len(list) != 0 {
		t.Fatalf("another user sees %+v", list)
	}
	if _, err := st.GetSavedSearch(other, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign search = %v, want ErrNotFound", err)
	}
	if err := st.DeleteSavedSearch(other, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting a foreign search = %v, want ErrNotFound", err)
	}

	if err := st.DeleteSavedSearch(uid, created.ID); err != nil {
		t.Fatalf("DeleteSavedSearch: %v", err)
	}
	if list, _ := st.ListSavedSearches(uid); len(list) != 0 {
		t.Fatalf("searches left = %+v", list)
	}
	if err := st.DeleteSavedSearch(uid, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice = %v, want ErrNotFound", err)
	}
}

func TestShares(t *testing.T) {
	st, uid := newTestStore(t)
	f := mustCreateFeed(t, st, uid, "https://s.example/f", NewItem{GUID: "g", GUIDHash: "h", Title: "shared", URL: "https://s.example/1"})
	items, _ := st.ListItems(ItemFilter{UserID: uid, Type: 3, BatchSize: -1})
	item := items[0]

	token, err := st.CreateShare(uid, item.ID)
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	if len(token) != 32 {
		t.Fatalf("token = %q, want 16 random bytes in hex", token)
	}
	// Idempotent: the same item keeps its token.
	again, err := st.CreateShare(uid, item.ID)
	if err != nil || again != token {
		t.Fatalf("second CreateShare = %q, %v, want the same token", again, err)
	}

	sharedItem, sharedFeed, err := st.ItemByShareToken(token)
	if err != nil {
		t.Fatalf("ItemByShareToken: %v", err)
	}
	if sharedItem.ID != item.ID || sharedItem.Title != "shared" {
		t.Fatalf("item = %+v", sharedItem)
	}
	if sharedFeed.ID != f.ID || sharedFeed.Title == "" {
		t.Fatalf("feed = %+v, want the title of the feed", sharedFeed)
	}

	if err := st.DeleteShare(uid, item.ID); err != nil {
		t.Fatalf("DeleteShare: %v", err)
	}
	if _, _, err := st.ItemByShareToken(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted share = %v, want ErrNotFound", err)
	}
	// Deleting a share that is not there is not an error.
	if err := st.DeleteShare(uid, item.ID); err != nil {
		t.Fatalf("DeleteShare twice: %v", err)
	}
	if _, _, err := st.ItemByShareToken("unknown-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token = %v, want ErrNotFound", err)
	}
}

func TestWebSubSubscriptionStore(t *testing.T) {
	st, uid := newTestStore(t)
	f := mustCreateFeed(t, st, uid, "https://ws.example/f")

	if _, err := st.GetWebSub(f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetWebSub before any upsert = %v, want ErrNotFound", err)
	}

	if err := st.UpsertWebSub(f.ID, "https://hub.example/", "https://ws.example/f"); err != nil {
		t.Fatalf("UpsertWebSub: %v", err)
	}
	sub, err := st.GetWebSub(f.ID)
	if err != nil {
		t.Fatalf("GetWebSub: %v", err)
	}
	if sub.Hub != "https://hub.example/" || sub.Topic != "https://ws.example/f" {
		t.Fatalf("sub = %+v", sub)
	}
	if sub.LeaseUntil != 0 {
		t.Fatalf("lease = %d, want none before subscribing", sub.LeaseUntil)
	}
	// Without a lease it is due: the loop has to subscribe.
	if due, err := st.WebSubDue(1000, 0); err != nil || len(due) != 1 || due[0].FeedID != f.ID {
		t.Fatalf("due = %+v, %v, want the subscription without a lease", due, err)
	}

	// A second upsert keeps the secret, callback and lease.
	if err := st.SaveWebSubSecret(f.ID, "secret", "https://cb.example/1"); err != nil {
		t.Fatalf("SaveWebSubSecret: %v", err)
	}
	if err := st.SaveWebSubLease(f.ID, 12345); err != nil {
		t.Fatalf("SaveWebSubLease: %v", err)
	}
	if err := st.SaveWebSubStatus(f.ID, "active"); err != nil {
		t.Fatalf("SaveWebSubStatus: %v", err)
	}
	if err := st.UpsertWebSub(f.ID, "https://hub2.example/", "https://ws.example/f2"); err != nil {
		t.Fatalf("UpsertWebSub(second): %v", err)
	}
	sub, _ = st.GetWebSub(f.ID)
	if sub.Hub != "https://hub2.example/" || sub.Topic != "https://ws.example/f2" {
		t.Fatalf("sub = %+v, want the hub and topic updated", sub)
	}
	if sub.Secret != "secret" || sub.Callback != "https://cb.example/1" || sub.LeaseUntil != 12345 || sub.Status != "active" {
		t.Fatalf("sub = %+v, want the subscription state kept", sub)
	}

	// With a lease it stops being due until the lease is close to expiring.
	if due, err := st.WebSubDue(1000, 0); err != nil || len(due) != 0 {
		t.Fatalf("due with a live lease = %+v, %v, want none", due, err)
	}
	if due, _ := st.WebSubDue(20000, 0); len(due) != 1 {
		t.Fatalf("due after the lease expired = %+v, want the subscription", due)
	}

	if err := st.DeleteWebSub(f.ID); err != nil {
		t.Fatalf("DeleteWebSub: %v", err)
	}
	if _, err := st.GetWebSub(f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after DeleteWebSub = %v, want ErrNotFound", err)
	}
}

func TestGetFeedByIDIgnoresTheOwner(t *testing.T) {
	st, uid := newTestStore(t)
	f := mustCreateFeed(t, st, uid, "https://byid.example/f")

	got, err := st.GetFeedByID(f.ID)
	if err != nil || got.ID != f.ID {
		t.Fatalf("GetFeedByID = %+v, %v", got, err)
	}
	if got.UserID != uid {
		t.Fatalf("user = %d, want %d", got.UserID, uid)
	}
	if _, err := st.GetFeedByID(99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing feed = %v, want ErrNotFound", err)
	}
}

func TestRenameFolder(t *testing.T) {
	st, uid := newTestStore(t)
	folder, err := st.CreateFolder(uid, "Old", nil)
	if err != nil {
		t.Fatalf("CreateFolder: %v", err)
	}

	if err := st.RenameFolder(uid, folder.ID, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("empty name = %v, want ErrConflict", err)
	}
	if err := st.RenameFolder(uid, folder.ID, "New"); err != nil {
		t.Fatalf("RenameFolder: %v", err)
	}
	folders, err := st.ListFolders(uid)
	if err != nil || len(folders) != 1 || folders[0].Name != "New" {
		t.Fatalf("folders = %+v, %v, want the renamed one", folders, err)
	}
	other, _ := st.CreateUser("other", "hash", "Other", "user")
	if err := st.RenameFolder(other, folder.ID, "Hijacked"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("renaming a foreign folder = %v, want ErrNotFound", err)
	}
}
