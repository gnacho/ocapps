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
	owner := "user-a"

	now := time.Now()
	// 3 fotos: una hoy, una "on this day" de hace 2 años, una favorita con GPS
	for i, d := range []time.Time{now, now.AddDate(-2, 0, 0), now.AddDate(-1, 0, -5)} {
		_, ch, err := st.UpsertByETag(ctx, owner, "/dav/spaces/x/IMG_"+string(rune('A'+i))+".jpg", "e"+string(rune('0'+i)), "IMG.jpg", "image", d, 1000)
		if err != nil || !ch {
			t.Fatalf("upsert %d: ch=%v err=%v", i, ch, err)
		}
	}
	// mismo path, mismo etag → sin cambio
	if _, ch, _ := st.UpsertByETag(ctx, owner, "/dav/spaces/x/IMG_A.jpg", "e0", "IMG.jpg", "image", now, 1000); ch {
		t.Fatal("etag igual no debería marcar cambio")
	}
	// etag distinto → cambio
	if _, ch, _ := st.UpsertByETag(ctx, owner, "/dav/spaces/x/IMG_A.jpg", "e0b", "IMG.jpg", "image", now, 1000); !ch {
		t.Fatal("etag distinto debería marcar cambio")
	}

	stats, err := st.Stats(ctx, owner)
	if err != nil || stats["assets"].(int64) != 3 {
		t.Fatalf("stats: %v %v", stats, err)
	}

	page, err := st.AssetsPage(ctx, owner, 1<<62-1, 1<<62-1, 10, false, false, "")
	if err != nil || len(page) != 3 {
		t.Fatalf("page: %d %v", len(page), err)
	}

	if err := st.SetFavorite(ctx, owner, page[0].ID, true); err != nil {
		t.Fatal(err)
	}
	favs, _ := st.AssetsPage(ctx, owner, 1<<62-1, 1<<62-1, 10, true, false, "")
	if len(favs) != 1 {
		t.Fatalf("favs: %d", len(favs))
	}

	otd, err := st.OnThisDay(ctx, owner, int(now.Month()), now.Day(), now.Year(), 0)
	if err != nil || len(otd) != 1 {
		t.Fatalf("onThisDay: %d %v", len(otd), err)
	}
	// con rango ±3 debe seguir encontrando la del mismo día
	otdR, err := st.OnThisDay(ctx, owner, int(now.Month()), now.Day(), now.Year(), 3)
	if err != nil || len(otdR) != 1 {
		t.Fatalf("onThisDay rango: %d %v", len(otdR), err)
	}

	lat, lon := 40.4, -3.7
	if err := st.SaveExif(ctx, page[1].ID, ExifResult{Camera: "Sony α7 III", Lat: &lat, Lon: &lon, Width: 6000, Height: 4000}); err != nil {
		t.Fatal(err)
	}
	geo, _ := st.GeoAssets(ctx, owner, 10)
	if len(geo) != 1 || geo[0].Camera != "Sony α7 III" {
		t.Fatalf("geo: %+v", geo)
	}

	// soft-delete de lo no visto
	n, err := st.SoftDeleteExcept(ctx, owner, "", map[string]bool{"e0b": true})
	if err != nil || n != 2 {
		t.Fatalf("softdelete: %d %v", n, err)
	}
	stats, _ = st.Stats(ctx, owner)
	if stats["assets"].(int64) != 1 {
		t.Fatalf("tras softdelete: %v", stats)
	}
}

// Un scan acotado a una carpeta no puede marcar como borrado lo que vive fuera
// de ella: es el incidente de la issue #36 (root inexistente deja el índice a
// cero porque el soft-delete era global al owner).
func TestSoftDeleteExceptHonoursPrefix(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()

	for path, etag := range map[string]string{
		"/dav/spaces/x/Fotos/a.jpg":         "ea",
		"/dav/spaces/x/gestion/factura.jpg": "eg",
	} {
		if _, _, err := st.UpsertByETag(ctx, "alice", path, etag, "f.jpg", "image", now, 1000); err != nil {
			t.Fatal(err)
		}
	}

	// Root inexistente: prefijo que no cubre ningún asset. No debe borrar nada.
	if n, err := st.SoftDeleteExcept(ctx, "alice", "/dav/spaces/x/NoExiste/", map[string]bool{}); err != nil || n != 0 {
		t.Fatalf("softdelete fuera de ámbito: %d %v", n, err)
	}
	// Re-escanear /Fotos sin ver el antiguo: solo cae ese.
	if n, err := st.SoftDeleteExcept(ctx, "alice", "/dav/spaces/x/Fotos/", map[string]bool{}); err != nil || n != 1 {
		t.Fatalf("softdelete acotado: %d %v", n, err)
	}
	stats, _ := st.Stats(ctx, "alice")
	if stats["assets"].(int64) != 1 {
		t.Fatalf("tras softdelete acotado: %v", stats)
	}
}
