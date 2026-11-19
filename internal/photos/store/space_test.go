package store

import (
	"context"
	"testing"
	"time"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestSpacesListAndCount: the spaces of the index are what feeds the space
// filter of the UI (issue #145): id, name, type and the number of live assets,
// per owner.
func TestSpacesListAndCount(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()
	personal := Space{ID: "p1", Name: "Personal", Type: "personal"}
	project := Space{ID: "k1", Name: "Familia", Type: "project"}

	for i, path := range []string{"/dav/spaces/s$p1/Fotos/a.jpg", "/dav/spaces/s$p1/Fotos/b.jpg"} {
		if _, _, err := st.UpsertInSpace(ctx, "alice", path, string(rune('a'+i)), path, "image", now, 100, personal); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.UpsertInSpace(ctx, "alice", "/dav/spaces/s$k1/Fotos/c.jpg", "c", "c.jpg", "image", now, 100, project); err != nil {
		t.Fatal(err)
	}
	// another owner: must not appear in alice's listing
	if _, _, err := st.UpsertInSpace(ctx, "bob", "/dav/spaces/s$p1/Fotos/z.jpg", "z", "z.jpg", "image", now, 100, personal); err != nil {
		t.Fatal(err)
	}

	spaces, err := st.Spaces(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(spaces) != 2 {
		t.Fatalf("spaces = %+v, want 2", spaces)
	}
	// ordered by name: Familia, Personal
	if spaces[0].ID != "k1" || spaces[0].Name != "Familia" || spaces[0].Type != "project" || spaces[0].Assets != 1 {
		t.Errorf("first space = %+v", spaces[0])
	}
	if spaces[1].ID != "p1" || spaces[1].Assets != 2 {
		t.Errorf("second space = %+v", spaces[1])
	}

	// archived or deleted assets do not count
	id := int64(1) // p1/Fotos/a.jpg
	if err := st.SetArchived(ctx, "alice", id, true); err != nil {
		t.Fatal(err)
	}
	if spaces, _ := st.Spaces(ctx, "alice"); spaces[1].Assets != 1 {
		t.Errorf("an archived asset must not count: %+v", spaces[1])
	}
}

// TestAssetsPageBySpace: the timeline filter by space returns only the assets of
// that space, and keeps the keyset pagination.
func TestAssetsPageBySpace(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()
	personal := Space{ID: "p1", Name: "Personal", Type: "personal"}
	project := Space{ID: "k1", Name: "Familia", Type: "project"}
	for i := 0; i < 3; i++ {
		if _, _, err := st.UpsertInSpace(ctx, "alice", "/dav/spaces/s$p1/Fotos/p"+string(rune('a'+i))+".jpg", string(rune('a'+i)), "p.jpg", "image", now.Add(-time.Duration(i)*time.Hour), 100, personal); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.UpsertInSpace(ctx, "alice", "/dav/spaces/s$k1/Fotos/k.jpg", "k", "k.jpg", "image", now, 100, project); err != nil {
		t.Fatal(err)
	}

	all, err := st.AssetsPage(ctx, "alice", 1<<62-1, 1<<62-1, 100, false, false, "", "")
	if err != nil || len(all) != 4 {
		t.Fatalf("all: %d %v", len(all), err)
	}
	only, err := st.AssetsPage(ctx, "alice", 1<<62-1, 1<<62-1, 100, false, false, "", "k1")
	if err != nil || len(only) != 1 || only[0].SpaceName != "Familia" {
		t.Fatalf("space k1: %+v %v", only, err)
	}
	if only[0].SpaceID != "k1" || only[0].SpaceType != "project" {
		t.Errorf("space metadata of the asset = %+v", only[0])
	}
	if none, _ := st.AssetsPage(ctx, "alice", 1<<62-1, 1<<62-1, 100, false, false, "", "nope"); len(none) != 0 {
		t.Errorf("unknown space must return nothing: %+v", none)
	}
}

// TestUpsertFillsSpaceOfLegacyRows: rows indexed before migration 004 have an
// empty space. The next scan has the same etag, so the space must be filled
// without resetting the EXIF of the photo.
func TestUpsertFillsSpaceOfLegacyRows(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()
	path := "/dav/spaces/s$p1/Fotos/a.jpg"
	// legacy row (no space) already with EXIF processed
	id, _, err := st.UpsertByETag(ctx, "alice", path, "e1", "a.jpg", "image", now, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveExif(ctx, id, ExifResult{TakenAt: &now, Camera: "iPhone"}); err != nil {
		t.Fatal(err)
	}

	_, changed, err := st.UpsertInSpace(ctx, "alice", path, "e1", "a.jpg", "image", now, 100,
		Space{ID: "p1", Name: "Personal", Type: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("filling the space of an unchanged file is not a content change")
	}
	a, err := st.AssetByID(ctx, "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if a.SpaceID != "p1" || a.SpaceName != "Personal" {
		t.Errorf("space not filled: %+v", a)
	}
	if !a.ExifDone {
		t.Error("the EXIF was reset by filling the space")
	}
	if spaces, _ := st.Spaces(ctx, "alice"); len(spaces) != 1 {
		t.Errorf("spaces after the refresh = %+v", spaces)
	}
}

// TestUpsertRefreshesSpaceName: renaming a space does not change the path of
// its assets, so the index must follow the new name on the next scan.
func TestUpsertRefreshesSpaceName(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()
	path := "/dav/spaces/s$k1/Fotos/a.jpg"
	if _, _, err := st.UpsertInSpace(ctx, "alice", path, "e1", "a.jpg", "image", now, 100,
		Space{ID: "k1", Name: "Familia", Type: "project"}); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := st.UpsertInSpace(ctx, "alice", path, "e1", "a.jpg", "image", now, 100,
		Space{ID: "k1", Name: "Casa", Type: "project"}); err != nil || changed {
		t.Fatalf("rename: changed=%v err=%v", changed, err)
	}
	spaces, _ := st.Spaces(ctx, "alice")
	if len(spaces) != 1 || spaces[0].Name != "Casa" {
		t.Errorf("name not refreshed: %+v", spaces)
	}
}

// TestSoftDeleteLostSpaces: the rows of a space the owner can no longer see are
// soft-deleted, including the ones indexed before migration 004 (their space is
// read from the path). A visible space is never touched, and a row whose space
// cannot be determined is kept.
func TestSoftDeleteLostSpaces(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()
	keep := Space{ID: "p1", Name: "Personal", Type: "personal"}

	if _, _, err := st.UpsertInSpace(ctx, "alice", "/dav/spaces/s$p1/Fotos/a.jpg", "e1", "a.jpg", "image", now, 100, keep); err != nil {
		t.Fatal(err)
	}
	// a space that is no longer visible, indexed with its space
	if _, _, err := st.UpsertInSpace(ctx, "alice", "/dav/spaces/s$k1/Fotos/b.jpg", "e2", "b.jpg", "image", now, 100,
		Space{ID: "k1", Name: "Familia", Type: "project"}); err != nil {
		t.Fatal(err)
	}
	// the same case but from before migration 004: no space in the row
	if _, _, err := st.UpsertByETag(ctx, "alice", "/dav/spaces/s$k2/Fotos/c.jpg", "e3", "c.jpg", "image", now, 100); err != nil {
		t.Fatal(err)
	}
	// a path without a space: never pruned on a guess
	if _, _, err := st.UpsertByETag(ctx, "alice", "/dav/x/d.jpg", "e4", "d.jpg", "image", now, 100); err != nil {
		t.Fatal(err)
	}
	// another owner must not be touched
	if _, _, err := st.UpsertInSpace(ctx, "bob", "/dav/spaces/s$k1/Fotos/z.jpg", "e5", "z.jpg", "image", now, 100,
		Space{ID: "k1", Name: "Familia", Type: "project"}); err != nil {
		t.Fatal(err)
	}

	n, err := st.SoftDeleteLostSpaces(ctx, "alice", map[string]bool{"p1": true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 2 {
		t.Errorf("pruned = %d, want 2 (k1 and k2)", n)
	}
	alice, _ := st.AssetsPage(ctx, "alice", 1<<62-1, 1<<62-1, 10, false, false, "", "")
	if len(alice) != 2 { // p1 plus the unparsable path
		t.Fatalf("alice live assets = %d, want 2: %+v", len(alice), alice)
	}
	bob, _ := st.AssetsPage(ctx, "bob", 1<<62-1, 1<<62-1, 10, false, false, "", "")
	if len(bob) != 1 {
		t.Errorf("another owner was touched: %d", len(bob))
	}

	// defensive: an empty visible set is an error, never a full purge
	if _, err := st.SoftDeleteLostSpaces(ctx, "alice", nil); err == nil {
		t.Error("an empty list of visible spaces must not prune")
	}
}

// TestSpaceIDFromPath: the space id is the part after the '$' of the first
// segment of the DAV path.
func TestSpaceIDFromPath(t *testing.T) {
	cases := map[string]string{
		"/dav/spaces/7d443b01-21d3$9bc084a7-9bb4/Fotos/a.jpg": "9bc084a7-9bb4",
		"/dav/spaces/storage$space":                           "space",
		"/dav/x/a.jpg":                                        "",
		"/dav/spaces/plain/Fotos/a.jpg":                       "",
	}
	for p, want := range cases {
		if got := SpaceIDFromPath(p); got != want {
			t.Errorf("SpaceIDFromPath(%q) = %q, want %q", p, got, want)
		}
	}
}

// TestResetExifAndMarkExifDone: the backfill of the EXIF (issue #75) queues the
// owner's live images again, and a failed read marks the asset as processed
// without erasing what the row already had.
func TestResetExifAndMarkExifDone(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()
	id, _, err := st.UpsertInSpace(ctx, "alice", "/dav/spaces/s$p1/Fotos/a.jpg", "e1", "a.jpg", "image", now, 100,
		Space{ID: "p1", Name: "Personal", Type: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveExif(ctx, id, ExifResult{TakenAt: &now, Camera: "Apple iPhone", Width: 4032, Height: 3024}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.UpsertInSpace(ctx, "alice", "/dav/spaces/s$p1/Fotos/v.mp4", "e2", "v.mp4", "video", now, 100,
		Space{ID: "p1", Name: "Personal", Type: "personal"}); err != nil {
		t.Fatal(err)
	}

	n, err := st.ResetExif(ctx, "alice")
	if err != nil {
		t.Fatalf("ResetExif: %v", err)
	}
	if n != 1 {
		t.Errorf("queued = %d, want 1 (videos are not EXIF-processed)", n)
	}
	pending, err := st.PendingExif(ctx, "alice", 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %d, err=%v", len(pending), err)
	}

	// a failed read must keep the metadata of the row
	if err := st.MarkExifDone(ctx, id); err != nil {
		t.Fatal(err)
	}
	a, err := st.AssetByID(ctx, "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Camera != "Apple iPhone" || a.Width != 4032 || a.Height != 3024 {
		t.Errorf("the metadata was wiped: %+v", a)
	}
	if !a.ExifDone {
		t.Error("the asset was not marked as processed")
	}
}
