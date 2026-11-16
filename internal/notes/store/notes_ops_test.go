package store

// Note operations and settings against the real SQLite file: the writes the
// Notes API performs on every edit, and the defaults a new user gets.

import (
	"database/sql"
	"errors"
	"testing"
)

func newNote(t *testing.T, s *Store, user, title, content, category string) *Note {
	t.Helper()
	n, err := s.CreateNote(user, title, content, category, 1000)
	if err != nil {
		t.Fatalf("CreateNote: %v", err)
	}
	return n
}

func TestUpdateNoteFields(t *testing.T) {
	s := openTestStore(t)
	n := newNote(t, s, "user-a", "Title", "Body", "work")

	// One field at a time, leaving the rest untouched.
	title := "New title"
	got, err := s.UpdateNote("user-a", n.ID, &title, nil, nil, nil, 2000)
	if err != nil {
		t.Fatalf("UpdateNote(title): %v", err)
	}
	if got.Title != title || got.Content != "Body" || got.Category != "work" {
		t.Fatalf("note = %+v, want only the title changed", got)
	}
	if got.Etag != "7d0" || got.Modified != 2000 {
		t.Fatalf("etag/modified = %q/%d, want the ones of the new stamp", got.Etag, got.Modified)
	}

	content := "New body"
	if got, err = s.UpdateNote("user-a", n.ID, nil, &content, nil, nil, 3000); err != nil {
		t.Fatalf("UpdateNote(content): %v", err)
	}
	if got.Content != content || got.Title != title {
		t.Fatalf("note = %+v, want only the content changed", got)
	}

	category := "life"
	if got, err = s.UpdateNote("user-a", n.ID, nil, nil, &category, nil, 4000); err != nil {
		t.Fatalf("UpdateNote(category): %v", err)
	}
	if got.Category != category {
		t.Fatalf("category = %q", got.Category)
	}

	// The favourite is written on its own and also survives the batch update.
	fav := true
	if got, err = s.UpdateNote("user-a", n.ID, nil, nil, nil, &fav, 5000); err != nil {
		t.Fatalf("UpdateNote(favorite): %v", err)
	}
	if !got.Favorite {
		t.Fatalf("note = %+v, want it starred", got)
	}
	unsfav := false
	if got, err = s.UpdateNote("user-a", n.ID, &title, &content, &category, &unsfav, 6000); err != nil {
		t.Fatalf("UpdateNote(all): %v", err)
	}
	if got.Favorite {
		t.Fatalf("note = %+v, want the star removed", got)
	}

	// The changes are on disk, not only in the returned struct.
	stored, err := s.GetNote("user-a", n.ID)
	if err != nil {
		t.Fatalf("GetNote: %v", err)
	}
	if stored.Title != title || stored.Content != content || stored.Category != category || stored.Favorite {
		t.Fatalf("stored = %+v, want the updated note", stored)
	}
	if stored.Etag != got.Etag {
		t.Fatalf("stored etag = %q, want %q", stored.Etag, got.Etag)
	}

	// Another user cannot touch it.
	if _, err := s.UpdateNote("user-b", n.ID, &title, nil, nil, nil, 7000); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("updating a foreign note = %v, want ErrNoRows", err)
	}
	// A note that does not exist either.
	if _, err := s.UpdateNote("user-a", 999999, &title, nil, nil, nil, 8000); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("updating a missing note = %v, want ErrNoRows", err)
	}
}

func TestUpdateNoteWithTheSameTitleKeepsTheRow(t *testing.T) {
	s := openTestStore(t)
	n := newNote(t, s, "user-a", "Same", "Body", "")

	title := "Same"
	got, err := s.UpdateNote("user-a", n.ID, &title, nil, nil, nil, 2000)
	if err != nil {
		t.Fatalf("UpdateNote: %v", err)
	}
	if got.Title != "Same" {
		t.Fatalf("title = %q", got.Title)
	}
	// The etag still moves: the client is telling us the note was modified.
	if got.Etag == n.Etag {
		t.Fatalf("etag = %q, want a new one after the update", got.Etag)
	}
}

func TestDeleteNote(t *testing.T) {
	s := openTestStore(t)
	n := newNote(t, s, "user-a", "Bye", "Body", "")

	// A foreign user cannot delete it.
	if err := s.DeleteNote("user-b", n.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign delete = %v, want ErrNoRows", err)
	}
	if err := s.DeleteNote("user-a", n.ID); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}
	if err := s.DeleteNote("user-a", n.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second delete = %v, want ErrNoRows", err)
	}
	if _, err := s.GetNote("user-a", n.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("the note is still there: %v", err)
	}
}

func TestSetFavorite(t *testing.T) {
	s := openTestStore(t)
	n := newNote(t, s, "user-a", "Fav", "Body", "")

	got, err := s.SetFavorite("user-a", n.ID, true)
	if err != nil {
		t.Fatalf("SetFavorite: %v", err)
	}
	if !got.Favorite || got.Etag == n.Etag {
		t.Fatalf("note = %+v, want it starred with a new etag", got)
	}
	if got, err = s.SetFavorite("user-a", n.ID, false); err != nil || got.Favorite {
		t.Fatalf("unstar = %+v, %v", got, err)
	}
	if _, err := s.SetFavorite("user-b", n.ID, true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign star = %v, want ErrNoRows", err)
	}
}

func TestSearchNotes(t *testing.T) {
	s := openTestStore(t)
	newNote(t, s, "user-a", "Shopping list", "milk, bread", "life")
	newNote(t, s, "user-a", "Meeting", "agenda and milk", "work")
	newNote(t, s, "user-a", "Unrelated", "nothing here", "work")
	newNote(t, s, "user-b", "Shopping for bob", "milk", "life")

	// A match in the title and in the content, scoped to the owner.
	got, err := s.SearchNotes("user-a", "milk", 10)
	if err != nil {
		t.Fatalf("SearchNotes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("results = %d, want the two notes that mention milk", len(got))
	}
	for _, n := range got {
		if n.Title == "Unrelated" || n.Title == "Shopping for bob" {
			t.Fatalf("unexpected result %+v", n)
		}
	}

	// The search is case insensitive (LIKE).
	if got, err = s.SearchNotes("user-a", "SHOPPING", 10); err != nil || len(got) != 1 {
		t.Fatalf("case insensitive search = %d, %v", len(got), err)
	}

	// The limit is honoured.
	if got, err = s.SearchNotes("user-a", "milk", 1); err != nil || len(got) != 1 {
		t.Fatalf("limited search = %d, %v, want 1", len(got), err)
	}

	// No match is an empty result, not an error.
	if got, err = s.SearchNotes("user-a", "unicorn", 10); err != nil || len(got) != 0 {
		t.Fatalf("no match = %d, %v", len(got), err)
	}
}

func TestSettingsDefaultsAndUpdates(t *testing.T) {
	s := openTestStore(t)

	// First read seeds the defaults for the user.
	got, err := s.GetSettings("user-a")
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got["notesPath"] != "Notes" || got["fileSuffix"] != ".md" {
		t.Fatalf("defaults = %+v", got)
	}
	if again, err := s.GetSettings("user-a"); err != nil || again["notesPath"] != "Notes" {
		t.Fatalf("second read = %+v, %v, want the same defaults", again, err)
	}

	// An empty value falls back to the default for the known keys and clears
	// the rest.
	updated, err := s.UpdateSettings("user-a", Settings{
		"notesPath":  "",
		"fileSuffix": "",
		"theme":      "dark",
		"other":      "",
	})
	if err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if updated["notesPath"] != "Notes" || updated["fileSuffix"] != ".md" {
		t.Fatalf("known keys = %+v, want the defaults", updated)
	}
	if updated["theme"] != "dark" {
		t.Fatalf("theme = %q, want the stored one", updated["theme"])
	}
	if updated["other"] != "" {
		t.Fatalf("other = %q, want it cleared", updated["other"])
	}

	// The stored settings are the ones read afterwards.
	stored, err := s.GetSettings("user-a")
	if err != nil || stored["theme"] != "dark" {
		t.Fatalf("stored = %+v, %v", stored, err)
	}

	// Settings are per user.
	other, err := s.GetSettings("user-b")
	if err != nil || other["theme"] == "dark" {
		t.Fatalf("settings of another user = %+v, %v", other, err)
	}
}

func TestStripComments(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no comments", "CREATE TABLE t (id INTEGER);", "CREATE TABLE t (id INTEGER);"},
		{"line comment", "SELECT 1; -- a note\nSELECT 2;", "SELECT 1; \nSELECT 2;"},
		{"line comment at the end", "SELECT 1; -- trailing", "SELECT 1; "},
		{"block comment", "SELECT /* skip me */ 1;", "SELECT  1;"},
		{"multiline block comment", "SELECT /* one\ntwo */ 1;", "SELECT  1;"},
		// The unterminated block swallows the comment but leaves its last byte:
		// the migrations are embedded, so this never happens in practice.
		{"unterminated block", "SELECT /* never closed", "SELECT d"},
		// The scanner does not track string literals: a marker inside one is
		// stripped as well. The migrations have no such literal, which is why
		// the simple scanner is enough.
		{"marker inside a string is stripped too", "SELECT '--x';", "SELECT '"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripComments(tc.in); got != tc.want {
				t.Fatalf("stripComments(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
