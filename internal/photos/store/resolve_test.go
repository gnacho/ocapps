package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// TestAssetByPath: resolution by DAV href for the "Open with" flow
// (gnacho/ocapps #9). Covers: owner hit, miss for a non-existent path,
// miss for a foreign owner (IDOR, same answer) and miss after a soft-delete.
func TestAssetByPath(t *testing.T) {
	st, ctx, _, _ := twoOwnersFixture(t)
	const p = "/dav/spaces/x/Fotos/a.jpg"

	a, err := st.AssetByPath(ctx, "alice", p)
	if err != nil {
		t.Fatal(err)
	}
	if a.Filename != "a.jpg" || a.Path != p {
		t.Fatalf("unexpected asset: %+v", a)
	}

	// miss: non-existent path
	if _, err := st.AssetByPath(ctx, "alice", "/dav/spaces/x/Fotos/otra.jpg"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("miss path -> %v", err)
	}

	// miss: a path that only exists in alice's index, asked as bob
	// (nothing is leaked). The fixture gives bob THE SAME paths, so this
	// case needs a path exclusive to alice.
	if _, _, err := st.UpsertByETag(ctx, "alice", "/dav/spaces/x/Fotos/solo-alice.jpg", "eA", "solo-alice.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssetByPath(ctx, "bob", "/dav/spaces/x/Fotos/solo-alice.jpg"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("miss foreign -> %v", err)
	}

	// miss after a soft-delete (SoftDeleteExcept marks everything unseen)
	if n, err := st.SoftDeleteExcept(ctx, "alice", "", map[string]bool{}); err != nil || n == 0 {
		t.Fatalf("soft-delete: n=%d err=%v", n, err)
	}
	if _, err := st.AssetByPath(ctx, "alice", p); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("miss soft-deleted -> %v", err)
	}
}

// TestAssetByPathEscaped: the indexed href comes from url.PathUnescape in the
// scanner, so the lookup with the already unescaped path must hit
// (names with spaces and accents).
func TestAssetByPathEscaped(t *testing.T) {
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
		t.Fatalf("hit with unescaped path -> %v", err)
	}
}

// TestAssetBySpaceItem: fallback by spaceId plus relative path for aliases in
// the form <type>/<name>/<path> (OpenCloud 8.0.1, gnacho/ocapps #11). The
// indexed path carries the real alias "$<spaceId>", the lookup does not know
// the type.
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

	// miss: different path or different spaceId, same answer
	for _, tc := range [][2]string{
		{"7b251a66-5d40-4f4d-b1d0-01266acea88d", "Fotos/otra.jpg"},
		{"11111111-2222-3333-4444-555555555555", "Fotos/la playa.jpg"},
	} {
		if _, err := st.AssetBySpaceItem(ctx, "id-alice", tc[0], tc[1]); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("miss %v -> %v", tc, err)
		}
	}

	// literal wildcards: an itemPath with % does not behave as a wildcard
	if _, _, err := st.UpsertByETag(ctx, "id-alice",
		"/dav/spaces/personal$7b251a66-5d40-4f4d-b1d0-01266acea88d/Fotos/100%.jpg",
		"e2", "100%.jpg", "image", time.Now(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AssetBySpaceItem(ctx, "id-alice", "7b251a66-5d40-4f4d-b1d0-01266acea88d", "Fotos/100%.jpg"); err != nil {
		t.Fatalf("hit with literal %% -> %v", err)
	}
}
