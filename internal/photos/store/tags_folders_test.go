package store

import (
	"context"
	"testing"
	"time"
)

func TestTagsAndFolders(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	now := time.Now()
	id1, _, _ := st.UpsertByETag(ctx, "/dav/spaces/x/Fotos/a.jpg", "e1", "a.jpg", "image", now, 100)
	id2, _, _ := st.UpsertByETag(ctx, "/dav/spaces/x/Fotos/2024/b.jpg", "e2", "b.jpg", "image", now.Add(-time.Hour), 100)
	_, _, _ = st.UpsertByETag(ctx, "/dav/spaces/x/Fotos/2024/c.jpg", "e3", "c.jpg", "image", now.Add(-2*time.Hour), 100)

	// --- etiquetas ---
	if err := st.AddTag(ctx, id1, "playa"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTag(ctx, id1, "familia"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddTag(ctx, id2, "playa"); err != nil {
		t.Fatal(err)
	}
	tags, err := st.AssetTags(ctx, id1)
	if err != nil || len(tags) != 2 {
		t.Fatalf("asset tags: %v %v", tags, err)
	}
	list, err := st.ListTags(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list tags: %+v %v", list, err)
	}
	// "familia" (1) y "playa" (2), ordenadas alfabéticamente
	if list[0].Name != "familia" || list[0].Count != 1 || list[1].Name != "playa" || list[1].Count != 2 {
		t.Fatalf("recuentos: %+v", list)
	}
	byTag, err := st.AssetsByTag(ctx, "playa")
	if err != nil || len(byTag) != 2 {
		t.Fatalf("assets by tag: %d %v", len(byTag), err)
	}
	if err := st.RemoveTag(ctx, id1, "playa"); err != nil {
		t.Fatal(err)
	}
	if byTag, _ := st.AssetsByTag(ctx, "playa"); len(byTag) != 1 {
		t.Fatalf("remove tag: %d", len(byTag))
	}

	// --- carpetas ---
	base := "/dav/spaces/x/Fotos/"
	subs, err := st.Subfolders(ctx, base)
	if err != nil || len(subs) != 1 || subs[0].Name != "2024" || subs[0].Count != 2 {
		t.Fatalf("subfolders: %+v %v", subs, err)
	}
	assets, err := st.FolderAssets(ctx, base)
	if err != nil || len(assets) != 1 || assets[0].Filename != "a.jpg" {
		t.Fatalf("folder assets: %+v %v", assets, err)
	}
	// la subcarpeta no devuelve las fotos del nivel superior
	subAssets, err := st.FolderAssets(ctx, base+"2024/")
	if err != nil || len(subAssets) != 2 {
		t.Fatalf("subfolder assets: %d %v", len(subAssets), err)
	}
}
