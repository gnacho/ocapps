package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// TestSetTagsReplaces: the scanner needs to replace the whole set, not add
// tags, so that a deletion made outside the web reaches the index.
func TestSetTagsReplaces(t *testing.T) {
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

	// replacement: "playa" disappears and blanks and duplicates are ignored
	if err := st.SetTags(ctx, "alice", id, []string{" viaje ", "", "pets", "pets"}); err != nil {
		t.Fatal(err)
	}
	tags, _ = st.AssetTags(ctx, "alice", id)
	if len(tags) != 2 || tags[0] != "pets" || tags[1] != "viaje" {
		t.Fatalf("tags after replacement=%v", tags)
	}

	// empty set: no tags are left
	if err := st.SetTags(ctx, "alice", id, nil); err != nil {
		t.Fatal(err)
	}
	if tags, _ = st.AssetTags(ctx, "alice", id); len(tags) != 0 {
		t.Fatalf("tags after clearing=%v", tags)
	}
}

// TestSetTagsIDOR: an asset of another owner behaves as if it did not exist.
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
		t.Fatalf("err=%v, want ErrNoRows", err)
	}
	if tags, _ := st.AssetTags(ctx, "alice", id); len(tags) != 0 {
		t.Fatalf("the foreign asset changed: %v", tags)
	}
}

// TestSidecarETag: it starts empty, stores the applied one and refuses to
// write on a foreign asset.
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
		t.Fatalf("initial etag=%q err=%v, want empty", got, err)
	}
	if err := st.SetSidecarETag(ctx, "alice", id, `"xmp-1"`); err != nil {
		t.Fatal(err)
	}
	if got, err := st.SidecarETag(ctx, "alice", id); err != nil || got != `"xmp-1"` {
		t.Fatalf("etag=%q err=%v", got, err)
	}
	if err := st.SetSidecarETag(ctx, "bob", id, "robo"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err=%v, want ErrNoRows", err)
	}
}
