package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// TestAssetByPath: resolución por href DAV para el flujo "Abrir con"
// (gnacho/ocapps #9). Cubre: hit del propietario, miss por path inexistente,
// miss por owner ajeno (IDOR, misma respuesta) y miss tras soft-delete.
func TestAssetByPath(t *testing.T) {
	st, ctx, _, _ := fixtureDosOwners(t)
	const p = "/dav/spaces/x/Fotos/a.jpg"

	a, err := st.AssetByPath(ctx, "alice", p)
	if err != nil {
		t.Fatal(err)
	}
	if a.Filename != "a.jpg" || a.Path != p {
		t.Fatalf("asset inesperado: %+v", a)
	}

	// miss: path inexistente
	if _, err := st.AssetByPath(ctx, "alice", "/dav/spaces/x/Fotos/otra.jpg"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("miss path -> %v", err)
	}

	// miss: path que solo existe en el índice de alice, preguntado como bob
	// (no delata nada). El fixture da a bob LOS MISMOS paths, así que este
	// caso necesita un path exclusivo de alice.
	if _, _, err := st.UpsertByETag(ctx, "alice", "/dav/spaces/x/Fotos/solo-alice.jpg", "eA", "solo-alice.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssetByPath(ctx, "bob", "/dav/spaces/x/Fotos/solo-alice.jpg"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("miss ajeno -> %v", err)
	}

	// miss tras soft-delete (SoftDeleteExcept marca todo lo no visto)
	if n, err := st.SoftDeleteExcept(ctx, "alice", map[string]bool{}); err != nil || n == 0 {
		t.Fatalf("soft-delete: n=%d err=%v", n, err)
	}
	if _, err := st.AssetByPath(ctx, "alice", p); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("miss soft-deleted -> %v", err)
	}
}

// TestAssetByPathEscapado: el href indexado viene de url.PathUnescape en el
// scanner, así que el lookup por el path ya desescapado debe dar hit
// (nombres con espacios/acentos).
func TestAssetByPathEscapado(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	p := "/dav/spaces/sa/Fotos/verano 2026/la playa (1).jpg"
	if _, _, err := st.UpsertByETag(ctx, "id-alice", p, "e1", "la playa (1).jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssetByPath(ctx, "id-alice", p); err != nil {
		t.Fatalf("hit con path desescapado -> %v", err)
	}
}

// TestAssetBySpaceItem: fallback por spaceId + ruta relativa para aliases en
// forma <tipo>/<nombre>/<ruta> (OpenCloud 8.0.1, gnacho/ocapps #11). El path
// indexado lleva el alias real "$<spaceId>"; el lookup no conoce el tipo.
func TestAssetBySpaceItem(t *testing.T) {
	st, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	id, _, err := st.UpsertByETag(ctx, "id-alice",
		"/dav/spaces/personal$7b251a66-5d40-4f4d-b1d0-01266acea88d/Fotos/la playa.jpg",
		"e1", "la playa.jpg", "image", time.Now(), 100)
	if err != nil {
		t.Fatal(err)
	}

	a, err := st.AssetBySpaceItem(ctx, "id-alice", "7b251a66-5d40-4f4d-b1d0-01266acea88d", "Fotos/la playa.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != id {
		t.Fatalf("id %d != %d", a.ID, id)
	}

	// miss: ruta distinta o spaceId distinto, misma respuesta
	for _, tc := range [][2]string{
		{"7b251a66-5d40-4f4d-b1d0-01266acea88d", "Fotos/otra.jpg"},
		{"11111111-2222-3333-4444-555555555555", "Fotos/la playa.jpg"},
	} {
		if _, err := st.AssetBySpaceItem(ctx, "id-alice", tc[0], tc[1]); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("miss %v -> %v", tc, err)
		}
	}

	// comodines literales: un itemPath con % no se comporta como wildcard
	if _, _, err := st.UpsertByETag(ctx, "id-alice",
		"/dav/spaces/personal$7b251a66-5d40-4f4d-b1d0-01266acea88d/Fotos/100%.jpg",
		"e2", "100%.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssetBySpaceItem(ctx, "id-alice", "7b251a66-5d40-4f4d-b1d0-01266acea88d", "Fotos/100%.jpg"); err != nil {
		t.Fatalf("hit con %% literal -> %v", err)
	}
}
