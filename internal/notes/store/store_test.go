package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	commonstore "github.com/gnacho/ocapps/internal/common/store"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := openStoreAt(t, t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// openStoreAt opens the notes store in dir through the common SQLite opening
// (pragmas unified, SPEC §5.2) plus the notes adoption (backup + migrate +
// backfill).
func openStoreAt(t *testing.T, dir string) (*Store, error) {
	t.Helper()
	path := filepath.Join(dir, "notes.db")
	db, err := commonstore.Open(path)
	if err != nil {
		return nil, err
	}
	s, err := Open(db, path, "")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func TestNotesIsolatedPerUser(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CreateNote("user-a", "alice note", "content", "cat", 1); err != nil {
		t.Fatalf("CreateNote: %v", err)
	}

	alice, err := s.ListNotes("user-a", "", nil, 0, 0)
	if err != nil {
		t.Fatalf("ListNotes alice: %v", err)
	}
	if len(alice) != 1 {
		t.Fatalf("alice sees %d notes, want 1", len(alice))
	}

	bob, err := s.ListNotes("user-b", "", nil, 0, 0)
	if err != nil {
		t.Fatalf("ListNotes bob: %v", err)
	}
	if len(bob) != 0 {
		t.Fatalf("bob sees %d notes, want 0", len(bob))
	}
}

func TestGetNoteCrossUserNotFound(t *testing.T) {
	s := openTestStore(t)
	n, err := s.CreateNote("user-a", "t", "c", "cat", 1)
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if _, err := s.GetNote("user-b", n.ID); err != sql.ErrNoRows {
		t.Fatalf("GetNote cross-user err = %v, want sql.ErrNoRows", err)
	}
	if _, err := s.GetNote("user-a", n.ID); err != nil {
		t.Fatalf("GetNote owner err = %v, want nil", err)
	}
}

func TestUpdateNoteCrossUserNotFound(t *testing.T) {
	s := openTestStore(t)
	n, err := s.CreateNote("user-a", "t", "c", "cat", 1)
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	title := "hijack"
	if _, err := s.UpdateNote("user-b", n.ID, &title, nil, nil, nil, 2); err != sql.ErrNoRows {
		t.Fatalf("UpdateNote cross-user err = %v, want sql.ErrNoRows", err)
	}
	got, err := s.GetNote("user-a", n.ID)
	if err != nil {
		t.Fatalf("GetNote owner: %v", err)
	}
	if got.Title != "t" {
		t.Fatalf("title = %q, want %q (cross-user update leaked)", got.Title, "t")
	}
}

func TestDeleteNoteCrossUserNotFound(t *testing.T) {
	s := openTestStore(t)
	n, err := s.CreateNote("user-a", "t", "c", "cat", 1)
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if err := s.DeleteNote("user-b", n.ID); err != sql.ErrNoRows {
		t.Fatalf("DeleteNote cross-user err = %v, want sql.ErrNoRows", err)
	}
	if _, err := s.GetNote("user-a", n.ID); err != nil {
		t.Fatalf("note should still exist for alice: %v", err)
	}
	if err := s.DeleteNote("user-a", n.ID); err != nil {
		t.Fatalf("DeleteNote owner err = %v, want nil", err)
	}
}

func TestSetFavoriteCrossUserNotFound(t *testing.T) {
	s := openTestStore(t)
	n, err := s.CreateNote("user-a", "t", "c", "cat", 1)
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	if _, err := s.SetFavorite("user-b", n.ID, true); err != sql.ErrNoRows {
		t.Fatalf("SetFavorite cross-user err = %v, want sql.ErrNoRows", err)
	}
	got, err := s.GetNote("user-a", n.ID)
	if err != nil {
		t.Fatalf("GetNote owner: %v", err)
	}
	if got.Favorite {
		t.Fatal("favorite leaked across users")
	}
}

func TestSettingsIsolatedPerUser(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.UpdateSettings("user-a", Settings{"notesPath": "AliceNotes"}); err != nil {
		t.Fatalf("UpdateSettings alice: %v", err)
	}
	bob, err := s.GetSettings("user-b")
	if err != nil {
		t.Fatalf("GetSettings bob: %v", err)
	}
	if bob["notesPath"] != "Notes" {
		t.Fatalf("bob notesPath = %q, want default %q", bob["notesPath"], "Notes")
	}
	alice, err := s.GetSettings("user-a")
	if err != nil {
		t.Fatalf("GetSettings alice: %v", err)
	}
	if alice["notesPath"] != "AliceNotes" {
		t.Fatalf("alice notesPath = %q, want %q", alice["notesPath"], "AliceNotes")
	}
}

func TestSettingsLazyDefaults(t *testing.T) {
	s := openTestStore(t)
	got, err := s.GetSettings("user-c")
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got["notesPath"] != "Notes" || got["fileSuffix"] != ".md" {
		t.Fatalf("lazy defaults = %v, want notesPath=Notes fileSuffix=.md", got)
	}
}
