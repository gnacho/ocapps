package store

import (
	"context"
	"testing"
	"time"
)

func TestAlbumsAndPlaces(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	now := time.Now()
	id1, _, err := st.UpsertByETag(ctx, "/dav/spaces/x/A.jpg", "e1", "A.jpg", "image", now, 100)
	if err != nil {
		t.Fatal(err)
	}
	id2, _, err := st.UpsertByETag(ctx, "/dav/spaces/x/B.jpg", "e2", "B.jpg", "image", now.Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}

	// --- álbumes ---
	al, err := st.CreateAlbum(ctx, "Vacaciones")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddToAlbum(ctx, al, []int64{id1, id2}); err != nil {
		t.Fatal(err)
	}
	albums, err := st.ListAlbums(ctx)
	if err != nil || len(albums) != 1 || albums[0].Count != 2 || albums[0].CoverID != id1 {
		t.Fatalf("albums: %+v %v", albums, err)
	}
	if a, _ := st.AlbumAssets(ctx, al); len(a) != 2 {
		t.Fatalf("album assets: %d", len(a))
	}
	// añadir duplicado es idempotente
	_ = st.AddToAlbum(ctx, al, []int64{id1})
	if a, _ := st.AlbumAssets(ctx, al); len(a) != 2 {
		t.Fatalf("add duplicado: %d", len(a))
	}
	if err := st.RemoveFromAlbum(ctx, al, id1); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.AlbumAssets(ctx, al); len(a) != 1 {
		t.Fatalf("remove: %d", len(a))
	}
	if err := st.RenameAlbum(ctx, al, "Playa"); err != nil {
		t.Fatal(err)
	}
	if albums, _ := st.ListAlbums(ctx); albums[0].Name != "Playa" {
		t.Fatal("rename no aplicado")
	}
	if err := st.DeleteAlbum(ctx, al); err != nil {
		t.Fatal(err)
	}
	if albums, _ := st.ListAlbums(ctx); len(albums) != 0 {
		t.Fatal("delete no aplicado")
	}

	// --- lugares: dos fotos en el mismo sitio (~1 km) ---
	lat, lon := 40.4168, -3.7038
	if err := st.SaveExif(ctx, id1, ExifResult{Lat: &lat, Lon: &lon}); err != nil {
		t.Fatal(err)
	}
	lat2, lon2 := 40.4170, -3.7040
	if err := st.SaveExif(ctx, id2, ExifResult{Lat: &lat2, Lon: &lon2}); err != nil {
		t.Fatal(err)
	}
	places, err := st.PlaceClusters(ctx, 2)
	if err != nil || len(places) != 1 || places[0].Count != 2 {
		t.Fatalf("places: %+v %v", places, err)
	}

	// --- caché de geocodificación ---
	if _, ok, _ := st.GetGeocode(ctx, 40.42, -3.70); ok {
		t.Fatal("geocode no debería existir aún")
	}
	if err := st.SaveGeocode(ctx, 40.42, -3.70, "Madrid"); err != nil {
		t.Fatal(err)
	}
	if n, ok, _ := st.GetGeocode(ctx, 40.42, -3.70); !ok || n != "Madrid" {
		t.Fatalf("geocode cache: %q %v", n, ok)
	}
}
