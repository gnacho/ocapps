package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// TestSetTagsReemplaza: el scanner necesita sustituir el conjunto entero, no
// sumar etiquetas, para que un borrado hecho fuera de la web llegue al índice.
func TestSetTagsReemplaza(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	id, _, err := st.UpsertByETag(ctx, "alice", "/dav/x/a.jpg", "e1", "a.jpg", "image", time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetTags(ctx, "alice", id, []string{"playa", "viaje"}); err != nil {
		t.Fatal(err)
	}
	tags, err := st.AssetTags(ctx, "alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 || tags[0] != "playa" || tags[1] != "viaje" {
		t.Fatalf("tags=%v", tags)
	}

	// reemplazo: "playa" desaparece y se ignoran blancos y duplicados
	if err := st.SetTags(ctx, "alice", id, []string{" viaje ", "", "pets", "pets"}); err != nil {
		t.Fatal(err)
	}
	tags, _ = st.AssetTags(ctx, "alice", id)
	if len(tags) != 2 || tags[0] != "pets" || tags[1] != "viaje" {
		t.Fatalf("tags tras reemplazo=%v", tags)
	}

	// conjunto vacío: no deja etiquetas
	if err := st.SetTags(ctx, "alice", id, nil); err != nil {
		t.Fatal(err)
	}
	if tags, _ = st.AssetTags(ctx, "alice", id); len(tags) != 0 {
		t.Fatalf("tags tras vaciar=%v", tags)
	}
}

// TestSetTagsIDOR: un asset de otro owner se comporta como inexistente.
func TestSetTagsIDOR(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	id, _, err := st.UpsertByETag(ctx, "alice", "/dav/x/a.jpg", "e1", "a.jpg", "image", time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetTags(ctx, "bob", id, []string{"robo"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err=%v, quiero ErrNoRows", err)
	}
	if tags, _ := st.AssetTags(ctx, "alice", id); len(tags) != 0 {
		t.Fatalf("el asset ajeno cambió: %v", tags)
	}
}

// TestSidecarETag: arranca vacío, guarda el aplicado y no deja escribir en un
// asset ajeno.
func TestSidecarETag(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	id, _, err := st.UpsertByETag(ctx, "alice", "/dav/x/a.jpg", "e1", "a.jpg", "image", time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.SidecarETag(ctx, "alice", id); err != nil || got != "" {
		t.Fatalf("etag inicial=%q err=%v, quiero vacío", got, err)
	}
	if err := st.SetSidecarETag(ctx, "alice", id, `"xmp-1"`); err != nil {
		t.Fatal(err)
	}
	if got, err := st.SidecarETag(ctx, "alice", id); err != nil || got != `"xmp-1"` {
		t.Fatalf("etag=%q err=%v", got, err)
	}
	if err := st.SetSidecarETag(ctx, "bob", id, "robo"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err=%v, quiero ErrNoRows", err)
	}
}
