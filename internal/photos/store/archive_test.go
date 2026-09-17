package store

import (
	"context"
	"testing"
	"time"
)

func TestArchiveCalendarPHash(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	owner := "user-a"

	t1 := time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 3, 20, 12, 0, 0, 0, time.UTC)
	t3 := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	id1, _, _ := st.UpsertByETag(ctx, owner, "/dav/spaces/x/Fotos/a.jpg", "e1", "a.jpg", "image", t1, 100)
	id2, _, _ := st.UpsertByETag(ctx, owner, "/dav/spaces/x/Fotos/b.jpg", "e2", "b.jpg", "image", t2, 100)
	_, _, _ = st.UpsertByETag(ctx, owner, "/dav/spaces/x/Fotos/c.jpg", "e3", "c.jpg", "image", t3, 100)

	// --- archivado ---
	if err := st.SetArchived(ctx, owner, id1, true); err != nil {
		t.Fatal(err)
	}
	normal, _ := st.AssetsPage(ctx, owner, 1<<62-1, 1<<62-1, 10, false, false, "")
	if len(normal) != 2 {
		t.Fatalf("timeline sin archivadas: %d", len(normal))
	}
	archived, _ := st.AssetsPage(ctx, owner, 1<<62-1, 1<<62-1, 10, false, true, "")
	if len(archived) != 1 || archived[0].ID != id1 || !archived[0].IsArchived {
		t.Fatalf("archivadas: %+v", archived)
	}

	// --- calendario con meses ---
	years, err := st.Calendar(ctx, owner)
	if err != nil || len(years) != 2 {
		t.Fatalf("calendar: %+v %v", years, err)
	}
	if years[0].Year != 2025 || years[0].Count != 1 {
		t.Fatalf("año 2025: %+v", years[0])
	}
	// 2024: marzo con 2 fotos (la archivada se excluye -> 1)
	if years[1].Year != 2024 || len(years[1].Months) != 1 || years[1].Months[0].Month != 3 || years[1].Months[0].Count != 1 {
		t.Fatalf("año 2024: %+v", years[1])
	}

	// --- phash ---
	if err := st.SetPHash(ctx, owner, id1, "0000000000000000"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPHash(ctx, owner, id2, "0000000000000001"); err != nil {
		t.Fatal(err)
	}
	missing, err := st.AssetsWithoutPHash(ctx, owner, 10)
	if err != nil || len(missing) != 1 {
		t.Fatalf("sin phash: %d %v", len(missing), err)
	}
	hashes, err := st.AllPHashes(ctx, owner)
	if err != nil || len(hashes) != 2 {
		t.Fatalf("all phash: %d %v", len(hashes), err)
	}
}
