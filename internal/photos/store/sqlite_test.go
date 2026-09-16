package store

import (
	"context"
	"testing"
	"time"
)

func TestStoreRoundtrip(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	now := time.Now()
	// 3 fotos: una hoy, una "on this day" de hace 2 años, una favorita con GPS
	for i, d := range []time.Time{now, now.AddDate(-2, 0, 0), now.AddDate(-1, 0, -5)} {
		_, ch, err := st.UpsertByETag(ctx, "/dav/spaces/x/IMG_"+string(rune('A'+i))+".jpg", "e"+string(rune('0'+i)), "IMG.jpg", "image", d, 1000)
		if err != nil || !ch {
			t.Fatalf("upsert %d: ch=%v err=%v", i, ch, err)
		}
	}
	// mismo path, mismo etag → sin cambio
	if _, ch, _ := st.UpsertByETag(ctx, "/dav/spaces/x/IMG_A.jpg", "e0", "IMG.jpg", "image", now, 1000); ch {
		t.Fatal("etag igual no debería marcar cambio")
	}
	// etag distinto → cambio
	if _, ch, _ := st.UpsertByETag(ctx, "/dav/spaces/x/IMG_A.jpg", "e0b", "IMG.jpg", "image", now, 1000); !ch {
		t.Fatal("etag distinto debería marcar cambio")
	}

	stats, err := st.Stats(ctx)
	if err != nil || stats["assets"].(int64) != 3 {
		t.Fatalf("stats: %v %v", stats, err)
	}

	page, err := st.AssetsPage(ctx, 1<<62-1, 1<<62-1, 10, false, false, "")
	if err != nil || len(page) != 3 {
		t.Fatalf("page: %d %v", len(page), err)
	}

	if err := st.SetFavorite(ctx, page[0].ID, true); err != nil {
		t.Fatal(err)
	}
	favs, _ := st.AssetsPage(ctx, 1<<62-1, 1<<62-1, 10, true, false, "")
	if len(favs) != 1 {
		t.Fatalf("favs: %d", len(favs))
	}

	otd, err := st.OnThisDay(ctx, int(now.Month()), now.Day(), now.Year(), 0)
	if err != nil || len(otd) != 1 {
		t.Fatalf("onThisDay: %d %v", len(otd), err)
	}
	// con rango ±3 debe seguir encontrando la del mismo día
	otdR, err := st.OnThisDay(ctx, int(now.Month()), now.Day(), now.Year(), 3)
	if err != nil || len(otdR) != 1 {
		t.Fatalf("onThisDay rango: %d %v", len(otdR), err)
	}

	lat, lon := 40.4, -3.7
	if err := st.SaveExif(ctx, page[1].ID, ExifResult{Camera: "Sony α7 III", Lat: &lat, Lon: &lon, Width: 6000, Height: 4000}); err != nil {
		t.Fatal(err)
	}
	geo, _ := st.GeoAssets(ctx, 10)
	if len(geo) != 1 || geo[0].Camera != "Sony α7 III" {
		t.Fatalf("geo: %+v", geo)
	}

	// soft-delete de lo no visto
	n, err := st.SoftDeleteExcept(ctx, map[string]bool{"e0b": true})
	if err != nil || n != 2 {
		t.Fatalf("softdelete: %d %v", n, err)
	}
	stats, _ = st.Stats(ctx)
	if stats["assets"].(int64) != 1 {
		t.Fatalf("tras softdelete: %v", stats)
	}
}
