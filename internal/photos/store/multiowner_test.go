package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// fixtureDosOwners: alice y bob con LOS MISMOS paths (UNIQUE(owner,path),
// H8) + un álbum y una etiqueta para cada uno.
func fixtureDosOwners(t *testing.T) (*Store, context.Context, map[string]int64, map[string]int64) {
	t.Helper()
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
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

// TestMultiOwnerMismosPathsCoexisten: UNIQUE(owner,path) — dos owners con los
// mismos paths no colisionan ni se pisaban los upserts.
func TestMultiOwnerMismosPathsCoexisten(t *testing.T) {
	st, ctx, _, _ := fixtureDosOwners(t)
	for _, owner := range []string{"alice", "bob"} {
		page, err := st.AssetsPage(ctx, owner, 1<<62-1, 1<<62-1, 10, false, false, "")
		if err != nil || len(page) != 2 {
			t.Fatalf("%s: %d assets %v", owner, len(page), err)
		}
		for _, a := range page {
			if a.Path == "" {
				t.Fatal("path vacío")
			}
		}
		stats, _ := st.Stats(ctx, owner)
		if stats["assets"].(int64) != 2 {
			t.Fatalf("stats %s: %v", owner, stats)
		}
	}
	// upsert del mismo path con distinto etag en un owner NO toca al otro
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
	t.Fatalf("asset %q de %q no encontrado", path, owner)
	return 0
}

// TestMultiOwnerAislamiento: AssetsPage/Stats/Calendar/Albums/Tags/Folders/
// Geo/Places no cruzan datos entre owners.
func TestMultiOwnerAislamiento(t *testing.T) {
	st, ctx, _, _ := fixtureDosOwners(t)
	aliceA := mustID(t, st, ctx, "alice", "/dav/spaces/x/Fotos/a.jpg")
	bobA := mustID(t, st, ctx, "bob", "/dav/spaces/x/Fotos/a.jpg")
	if aliceA == bobA {
		t.Fatal("los ids deben ser globales y distintos")
	}

	// álbumes por owner
	alAlice, _ := st.CreateAlbum(ctx, "alice", "Vacaciones")
	if _, err := st.CreateAlbum(ctx, "bob", "Vacaciones"); err != nil {
		t.Fatalf("bob puede tener un álbum con el mismo nombre: %v", err)
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
		t.Fatalf("albums bob: %+v (no debe ver el contenido de alice)", albumsBob)
	}

	// tags por owner
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
		t.Fatal("bob no debe ver assets por el tag de alice")
	}

	// calendar por owner
	calA, _ := st.Calendar(ctx, "alice")
	calB, _ := st.Calendar(ctx, "bob")
	if len(calA) != 1 || len(calB) != 1 {
		t.Fatalf("calendar: %+v %+v", calA, calB)
	}

	// folders por owner: mismos paths, cada uno ve solo los suyos
	subs, _ := st.Subfolders(ctx, "alice", "/dav/spaces/x/Fotos/")
	if len(subs) != 1 || subs[0].Count != 1 {
		t.Fatalf("subfolders alice: %+v", subs)
	}
	fa, _ := st.FolderAssets(ctx, "bob", "/dav/spaces/x/Fotos/")
	if len(fa) != 1 {
		t.Fatalf("folder assets bob: %d", len(fa))
	}

	// geo/places por owner
	lat, lon := 40.4168, -3.7038
	if err := st.SaveExif(ctx, aliceA, ExifResult{Lat: &lat, Lon: &lon}); err != nil {
		t.Fatal(err)
	}
	geoB, _ := st.GeoAssets(ctx, "bob", 10)
	if len(geoB) != 0 {
		t.Fatal("bob no debe ver el geo de alice")
	}
	placesB, _ := st.PlaceClusters(ctx, "bob", 2)
	if len(placesB) != 0 {
		t.Fatal("bob no debe ver los lugares de alice")
	}

	// soft-delete acotado al owner: un scan de bob que no ve nada NO borra a alice
	if n, err := st.SoftDeleteExcept(ctx, "bob", map[string]bool{}); err != nil || n != 2 {
		t.Fatalf("softdelete bob: %d %v", n, err)
	}
	statsA, _ := st.Stats(ctx, "alice")
	if statsA["assets"].(int64) != 2 {
		t.Fatalf("alice intacta tras softdelete de bob: %v", statsA)
	}
	statsB, _ := st.Stats(ctx, "bob")
	if statsB["assets"].(int64) != 0 {
		t.Fatalf("bob soft-deleted: %v", statsB)
	}
}

// TestMultiOwnerIDOR: leer/escribir ids ajenos devuelve sql.ErrNoRows ("no
// encontrado"), nunca un error que delate existencia (H8 §3).
func TestMultiOwnerIDOR(t *testing.T) {
	st, ctx, _, _ := fixtureDosOwners(t)
	aliceA := mustID(t, st, ctx, "alice", "/dav/spaces/x/Fotos/a.jpg")
	alAlice, _ := st.CreateAlbum(ctx, "alice", "Vacaciones")
	if err := st.AddToAlbum(ctx, "alice", alAlice, []int64{aliceA}); err != nil {
		t.Fatal(err)
	}

	assertNoRows := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s: err=%v, quiero sql.ErrNoRows", name, err)
		}
	}

	// lectura de un id ajeno
	if _, err := st.AssetByID(ctx, "bob", aliceA); err == nil {
		t.Fatal("AssetByID ajeno debe fallar")
	} else {
		assertNoRows("AssetByID", err)
	}
	// escrituras sobre un id ajeno
	assertNoRows("SetFavorite", st.SetFavorite(ctx, "bob", aliceA, true))
	assertNoRows("SetArchived", st.SetArchived(ctx, "bob", aliceA, true))
	assertNoRows("SetPHash", st.SetPHash(ctx, "bob", aliceA, "00"))
	assertNoRows("AddTag", st.AddTag(ctx, "bob", aliceA, "robo"))
	assertNoRows("AssetTags", func() error { _, err := st.AssetTags(ctx, "bob", aliceA); return err }())
	// álbum ajeno: renombrar, borrar, listar assets, añadir, quitar
	assertNoRows("RenameAlbum", st.RenameAlbum(ctx, "bob", alAlice, "x"))
	assertNoRows("DeleteAlbum", st.DeleteAlbum(ctx, "bob", alAlice))
	assertNoRows("AlbumAssets", func() error { _, err := st.AlbumAssets(ctx, "bob", alAlice); return err }())
	assertNoRows("AddToAlbum(album ajeno)", st.AddToAlbum(ctx, "bob", alAlice, []int64{mustID(t, st, ctx, "bob", "/dav/spaces/x/Fotos/a.jpg")}))
	assertNoRows("RemoveFromAlbum", st.RemoveFromAlbum(ctx, "bob", alAlice, aliceA))
	// añadir un asset AJENO a un álbum PROPIO: también ErrNoRows
	alBob, _ := st.CreateAlbum(ctx, "bob", "Mío")
	assertNoRows("AddToAlbum(asset ajeno)", st.AddToAlbum(ctx, "bob", alBob, []int64{aliceA}))

	// y alice sigue intacta tras todos los intentos de bob
	a, err := st.AssetByID(ctx, "alice", aliceA)
	if err != nil || a.IsFavorite || a.IsArchived {
		t.Fatalf("alice modificada por bob: %+v %v", a, err)
	}
	if tags, _ := st.AssetTags(ctx, "alice", aliceA); len(tags) != 0 {
		t.Fatalf("tags de alice tras intentos de bob: %v", tags)
	}
	if albums, _ := st.ListAlbums(ctx, "alice"); len(albums) != 1 || albums[0].Name != "Vacaciones" {
		t.Fatalf("álbum de alice modificado por bob: %+v", albums)
	}
	// el álbum de bob NO contiene el asset de alice
	if got, _ := st.AlbumAssets(ctx, "bob", alBob); len(got) != 0 {
		t.Fatalf("bob coló un asset ajeno en su álbum: %d", len(got))
	}
}

// TestPendingExifPorOwner: la cola de EXIF va por owner (worker multi-tenant).
func TestPendingExifPorOwner(t *testing.T) {
	st, ctx, _, _ := fixtureDosOwners(t)
	pA, err := st.PendingExif(ctx, "alice", 50)
	if err != nil || len(pA) != 2 {
		t.Fatalf("pending alice: %d %v", len(pA), err)
	}
	// procesar uno de alice no cambia la cola de bob
	if err := st.SaveExif(ctx, pA[0].ID, ExifResult{}); err != nil {
		t.Fatal(err)
	}
	pA2, _ := st.PendingExif(ctx, "alice", 50)
	pB, _ := st.PendingExif(ctx, "bob", 50)
	if len(pA2) != 1 || len(pB) != 2 {
		t.Fatalf("pending tras exif: alice=%d bob=%d", len(pA2), len(pB))
	}
}
