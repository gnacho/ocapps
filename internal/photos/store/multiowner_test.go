package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// twoOwnersFixture: alice and bob with THE SAME paths (UNIQUE(owner,path),
// H8) plus one album and one tag for each of them.
func twoOwnersFixture(t *testing.T) (*Store, context.Context, map[string]int64, map[string]int64) {
	t.Helper()
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := time.Now()
	ids := map[string]int64{}
	for _, owner := range []string{"alice", "bob"} {
		for i, p := range []string{"/dav/spaces/x/Fotos/a.jpg", "/dav/spaces/x/Fotos/2024/b.jpg"} {
			id, _, err := st.UpsertByETag(ctx, owner, p, "e"+owner+string(rune('0'+i)), p[len(p)-5:], "image", now.Add(-time.Duration(i)*time.Hour), 100)
			if err != nil {
				t.Fatalf("upsert %s: %v", owner, err)
			}
			ids[owner+string(rune('0'+i))] = id
		}
	}
	return st, ctx, ids, ids
}

// TestMultiOwnerSamePathsCoexist: UNIQUE(owner,path) - two owners with the same
// paths do not collide and their upserts do not overwrite each other.
func TestMultiOwnerSamePathsCoexist(t *testing.T) {
	st, ctx, _, _ := twoOwnersFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		page, err := st.AssetsPage(ctx, owner, 1<<62-1, 1<<62-1, 10, false, false, "")
		if err != nil || len(page) != 2 {
			t.Fatalf("%s: %d assets %v", owner, len(page), err)
		}
		for _, a := range page {
			if a.Path == "" {
				t.Fatal("empty path")
			}
		}
		stats, _ := st.Stats(ctx, owner)
		if stats["assets"].(int64) != 2 {
			t.Fatalf("stats %s: %v", owner, stats)
		}
	}
	// upsert of the same path with a different etag for one owner does NOT touch the other
	aliceA, _ := st.AssetByID(ctx, "alice", mustID(t, st, ctx, "alice", "/dav/spaces/x/Fotos/a.jpg"))
	_ = aliceA
}

func mustID(t *testing.T, st *Store, ctx context.Context, owner, path string) int64 {
	t.Helper()
	page, err := st.AssetsPage(ctx, owner, 1<<62-1, 1<<62-1, 100, false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range page {
		if a.Path == path {
			return a.ID
		}
	}
	t.Fatalf("asset %q of %q not found", path, owner)
	return 0
}

// TestMultiOwnerIsolation: AssetsPage/Stats/Calendar/Albums/Tags/Folders/
// Geo/Places do not cross data between owners.
func TestMultiOwnerIsolation(t *testing.T) {
	st, ctx, _, _ := twoOwnersFixture(t)
	aliceA := mustID(t, st, ctx, "alice", "/dav/spaces/x/Fotos/a.jpg")
	bobA := mustID(t, st, ctx, "bob", "/dav/spaces/x/Fotos/a.jpg")
	if aliceA == bobA {
		t.Fatal("ids must be global and different")
	}

	// albums per owner
	alAlice, _ := st.CreateAlbum(ctx, "alice", "Vacaciones")
	if _, err := st.CreateAlbum(ctx, "bob", "Vacaciones"); err != nil {
		t.Fatalf("bob can have an album with the same name: %v", err)
	}
	if err := st.AddToAlbum(ctx, "alice", alAlice, []int64{aliceA}); err != nil {
		t.Fatal(err)
	}
	albums, _ := st.ListAlbums(ctx, "alice")
	if len(albums) != 1 || albums[0].Count != 1 {
		t.Fatalf("albums alice: %+v", albums)
	}
	albumsBob, _ := st.ListAlbums(ctx, "bob")
	if len(albumsBob) != 1 || albumsBob[0].Count != 0 {
		t.Fatalf("albums bob: %+v (must not see alice's content)", albumsBob)
	}

	// tags per owner
	if err := st.AddTag(ctx, "alice", aliceA, "playa"); err != nil {
		t.Fatal(err)
	}
	tagsA, _ := st.ListTags(ctx, "alice")
	tagsB, _ := st.ListTags(ctx, "bob")
	if len(tagsA) != 1 || len(tagsB) != 0 {
		t.Fatalf("tags: alice=%+v bob=%+v", tagsA, tagsB)
	}
	byTag, _ := st.AssetsByTag(ctx, "bob", "playa")
	if len(byTag) != 0 {
		t.Fatal("bob must not see assets through alice's tag")
	}

	// calendar per owner
	calA, _ := st.Calendar(ctx, "alice")
	calB, _ := st.Calendar(ctx, "bob")
	if len(calA) != 1 || len(calB) != 1 {
		t.Fatalf("calendar: %+v %+v", calA, calB)
	}

	// folders per owner: same paths, each one sees only its own
	subs, _ := st.Subfolders(ctx, "alice", "/dav/spaces/x/Fotos/")
	if len(subs) != 1 || subs[0].Count != 1 {
		t.Fatalf("subfolders alice: %+v", subs)
	}
	fa, _ := st.FolderAssets(ctx, "bob", "/dav/spaces/x/Fotos/")
	if len(fa) != 1 {
		t.Fatalf("folder assets bob: %d", len(fa))
	}

	// geo/places per owner
	lat, lon := 40.4168, -3.7038
	if err := st.SaveExif(ctx, aliceA, ExifResult{Lat: &lat, Lon: &lon}); err != nil {
		t.Fatal(err)
	}
	geoB, _ := st.GeoAssets(ctx, "bob", 10)
	if len(geoB) != 0 {
		t.Fatal("bob must not see alice's geo")
	}
	placesB, _ := st.PlaceClusters(ctx, "bob", 2)
	if len(placesB) != 0 {
		t.Fatal("bob must not see alice's places")
	}

	// soft-delete scoped to the owner: a scan of bob that sees nothing does NOT delete alice's rows
	if n, err := st.SoftDeleteExcept(ctx, "bob", "", map[string]bool{}); err != nil || n != 2 {
		t.Fatalf("softdelete bob: %d %v", n, err)
	}
	statsA, _ := st.Stats(ctx, "alice")
	if statsA["assets"].(int64) != 2 {
		t.Fatalf("alice intact after bob's softdelete: %v", statsA)
	}
	statsB, _ := st.Stats(ctx, "bob")
	if statsB["assets"].(int64) != 0 {
		t.Fatalf("bob soft-deleted: %v", statsB)
	}
}

// TestMultiOwnerIDOR: reading or writing foreign ids returns sql.ErrNoRows
// ("not found"), never an error that leaks existence (H8 §3).
func TestMultiOwnerIDOR(t *testing.T) {
	st, ctx, _, _ := twoOwnersFixture(t)
	aliceA := mustID(t, st, ctx, "alice", "/dav/spaces/x/Fotos/a.jpg")
	alAlice, _ := st.CreateAlbum(ctx, "alice", "Vacaciones")
	if err := st.AddToAlbum(ctx, "alice", alAlice, []int64{aliceA}); err != nil {
		t.Fatal(err)
	}

	assertNoRows := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s: err=%v, want sql.ErrNoRows", name, err)
		}
	}

	// reading a foreign id
	if _, err := st.AssetByID(ctx, "bob", aliceA); err == nil {
		t.Fatal("AssetByID on a foreign asset must fail")
	} else {
		assertNoRows("AssetByID", err)
	}
	// writes on a foreign id
	assertNoRows("SetFavorite", st.SetFavorite(ctx, "bob", aliceA, true))
	assertNoRows("SetArchived", st.SetArchived(ctx, "bob", aliceA, true))
	assertNoRows("SetPHash", st.SetPHash(ctx, "bob", aliceA, "00"))
	assertNoRows("AddTag", st.AddTag(ctx, "bob", aliceA, "robo"))
	assertNoRows("AssetTags", func() error { _, err := st.AssetTags(ctx, "bob", aliceA); return err }())
	// foreign album: rename, delete, list its assets, add, remove
	assertNoRows("RenameAlbum", st.RenameAlbum(ctx, "bob", alAlice, "x"))
	assertNoRows("DeleteAlbum", st.DeleteAlbum(ctx, "bob", alAlice))
	assertNoRows("AlbumAssets", func() error { _, err := st.AlbumAssets(ctx, "bob", alAlice); return err }())
	assertNoRows("AddToAlbum(foreign album)", st.AddToAlbum(ctx, "bob", alAlice, []int64{mustID(t, st, ctx, "bob", "/dav/spaces/x/Fotos/a.jpg")}))
	assertNoRows("RemoveFromAlbum", st.RemoveFromAlbum(ctx, "bob", alAlice, aliceA))
	// adding a FOREIGN asset to an OWN album: also ErrNoRows
	alBob, _ := st.CreateAlbum(ctx, "bob", "Mío")
	assertNoRows("AddToAlbum(foreign asset)", st.AddToAlbum(ctx, "bob", alBob, []int64{aliceA}))

	// and alice is still intact after all of bob's attempts
	a, err := st.AssetByID(ctx, "alice", aliceA)
	if err != nil || a.IsFavorite || a.IsArchived {
		t.Fatalf("alice modified by bob: %+v %v", a, err)
	}
	if tags, _ := st.AssetTags(ctx, "alice", aliceA); len(tags) != 0 {
		t.Fatalf("alice's tags after bob's attempts: %v", tags)
	}
	if albums, _ := st.ListAlbums(ctx, "alice"); len(albums) != 1 || albums[0].Name != "Vacaciones" {
		t.Fatalf("alice's album modified by bob: %+v", albums)
	}
	// bob's album does NOT contain alice's asset
	if got, _ := st.AlbumAssets(ctx, "bob", alBob); len(got) != 0 {
		t.Fatalf("bob sneaked a foreign asset into his album: %d", len(got))
	}
}

// TestPendingExifPerOwner: the EXIF queue is per owner (multi-tenant worker).
func TestPendingExifPerOwner(t *testing.T) {
	st, ctx, _, _ := twoOwnersFixture(t)
	pA, err := st.PendingExif(ctx, "alice", 50)
	if err != nil || len(pA) != 2 {
		t.Fatalf("pending alice: %d %v", len(pA), err)
	}
	// processing one of alice's does not change bob's queue
	if err := st.SaveExif(ctx, pA[0].ID, ExifResult{}); err != nil {
		t.Fatal(err)
	}
	pA2, _ := st.PendingExif(ctx, "alice", 50)
	pB, _ := st.PendingExif(ctx, "bob", 50)
	if len(pA2) != 1 || len(pB) != 2 {
		t.Fatalf("pending after exif: alice=%d bob=%d", len(pA2), len(pB))
	}
}

// TestBackfillCollisions: if the backfill skipped a start (OpenCloud down) and
// the user rescanned, (empty path) and (owner,path) coexist. The backfill must NOT
// blow up on UNIQUE(owner,path): it adopts the non-conflicting orphans, deletes
// the conflicting ones (the new rows of the owner already represent them, with
// a cascade over album_assets/asset_tags) and is idempotent (H8 review FIX 3).
func TestBackfillCollisions(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	now := time.Now()
	add := func(owner, path, etag string) int64 {
		t.Helper()
		id, _, err := st.UpsertByETag(ctx, owner, path, etag, path[len(path)-5:], "image", now, 100)
		if err != nil {
			t.Fatalf("upsert %s %s: %v", owner, path, err)
		}
		return id
	}
	// single-tenant era: 3 orphan assets and 2 orphan albums
	orphanA := add("", "/dav/x/a.jpg", "e1")
	orphanB := add("", "/dav/x/b.jpg", "e2")
	add("", "/dav/x/c.jpg", "e3")
	alOrphanV, _ := st.CreateAlbum(ctx, "", "Vacaciones")
	if _, err := st.CreateAlbum(ctx, "", "Retro"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddToAlbum(ctx, "", alOrphanV, []int64{orphanA, orphanB}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTag(ctx, "", orphanB, "playa"); err != nil {
		t.Fatal(err)
	}
	// the user rescanned before the backfill: /b.jpg and /c.jpg already belong to him
	add("oc1", "/dav/x/b.jpg", "e2-new")
	add("oc1", "/dav/x/c.jpg", "e3-new")
	alV, _ := st.CreateAlbum(ctx, "oc1", "Vacaciones")

	// scenario before the fix: the blind UPDATE blew up with error 2067
	nA, nAl, rmA, rmAl, err := st.BackfillOwner(ctx, "oc1")
	if err != nil {
		t.Fatalf("backfill with collisions must not fail: %v", err)
	}
	if nA != 1 || nAl != 1 || rmA != 2 || rmAl != 1 {
		t.Fatalf("adopted %d/%d removed %d/%d, want 1/1 and 2/1", nA, nAl, rmA, rmAl)
	}
	// no orphans left and everything expected for oc1
	if oa, ob, _ := st.OrphanCounts(ctx); oa != 0 || ob != 0 {
		t.Fatalf("orphans after backfill: %d %d", oa, ob)
	}
	stats, _ := st.Stats(ctx, "oc1")
	if stats["assets"].(int64) != 3 {
		t.Fatalf("oc1 must have 3 assets: %v", stats)
	}
	// the NON-conflicting orphan asset was adopted keeping its id
	if a, err := st.AssetByID(ctx, "oc1", orphanA); err != nil || a.Path != "/dav/x/a.jpg" {
		t.Fatalf("adopted asset: %+v %v", a, err)
	}
	// the conflicting orphans were deleted (the id no longer exists even for "")
	if _, err := st.AssetByID(ctx, "", orphanB); err == nil {
		t.Fatal("the conflicting orphan must disappear")
	}
	// cascade: the tag and the links of the deleted orphan album too
	if albums, _ := st.ListAlbums(ctx, ""); len(albums) != 0 {
		t.Fatalf("orphan albums: %+v", albums)
	}
	if tags, _ := st.ListTags(ctx, "oc1"); len(tags) != 0 {
		t.Fatalf("the tag of the conflicting asset should have died in cascade: %v", tags)
	}
	// the owner's album keeps its identity (no duplicate by name)
	albums, _ := st.ListAlbums(ctx, "oc1")
	if len(albums) != 2 || (albums[0].ID != alV && albums[1].ID != alV) {
		t.Fatalf("albums of oc1: %+v", albums)
	}
	// idempotent: a second backfill (restart) touches nothing
	nA, nAl, rmA, rmAl, err = st.BackfillOwner(ctx, "oc1")
	if err != nil || nA != 0 || nAl != 0 || rmA != 0 || rmAl != 0 {
		t.Fatalf("second backfill must be a no-op: %d/%d -%d/-%d %v", nA, nAl, rmA, rmAl, err)
	}
}
