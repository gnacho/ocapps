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
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	p := "/dav/spaces/sa/Fotos/verano 2026/la playa (1).jpg"
	if _, _, err := st.UpsertByETag(ctx, "id-alice", p, "e1", "la playa (1).jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssetByPath(ctx, "id-alice", p); err != nil {
		t.Fatalf("hit con path desescapado -> %v", err)
	}
}
