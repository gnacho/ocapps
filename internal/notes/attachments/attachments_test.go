package attachments

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, dir
}

func TestSaveOpenRoundtrip(t *testing.T) {
	s, _ := newTestStore(t)

	apiPath, err := s.Save(7, "nota.txt", strings.NewReader("hola mundo"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if apiPath != ".attachments.7/nota.txt" {
		t.Fatalf("apiPath = %q, want %q", apiPath, ".attachments.7/nota.txt")
	}

	rc, size, ct, err := s.Open(7, apiPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "hola mundo" {
		t.Fatalf("body = %q, want %q", body, "hola mundo")
	}
	if size != int64(len("hola mundo")) {
		t.Fatalf("size = %d, want %d", size, len("hola mundo"))
	}
	if !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content type = %q, want text/plain", ct)
	}
}

func TestSaveDetectsImageContentType(t *testing.T) {
	s, _ := newTestStore(t)

	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	apiPath, err := s.Save(1, "foto.png", bytes.NewReader(png))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	rc, _, ct, err := s.Open(1, apiPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()

	if ct != "image/png" {
		t.Fatalf("content type = %q, want image/png", ct)
	}
}

func TestSaveDeduplicatesNames(t *testing.T) {
	s, _ := newTestStore(t)

	first, err := s.Save(7, "photo.png", strings.NewReader("first"))
	if err != nil {
		t.Fatalf("Save first: %v", err)
	}
	second, err := s.Save(7, "photo.png", strings.NewReader("second"))
	if err != nil {
		t.Fatalf("Save second: %v", err)
	}
	third, err := s.Save(7, "photo.png", strings.NewReader("third"))
	if err != nil {
		t.Fatalf("Save third: %v", err)
	}

	if first != ".attachments.7/photo.png" {
		t.Fatalf("first = %q", first)
	}
	if second != ".attachments.7/photo (1).png" {
		t.Fatalf("second = %q, want .attachments.7/photo (1).png", second)
	}
	if third != ".attachments.7/photo (2).png" {
		t.Fatalf("third = %q, want .attachments.7/photo (2).png", third)
	}

	// Each upload keeps its own content.
	for want, path := range map[string]string{"first": first, "second": second, "third": third} {
		rc, _, _, err := s.Open(7, path)
		if err != nil {
			t.Fatalf("Open %s: %v", path, err)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		if string(body) != want {
			t.Fatalf("%s content = %q, want %q", path, body, want)
		}
	}
}

func TestSaveDeduplicatesWithoutExtension(t *testing.T) {
	s, _ := newTestStore(t)

	first, err := s.Save(3, "notes", strings.NewReader("a"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	second, err := s.Save(3, "notes", strings.NewReader("b"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if first != ".attachments.3/notes" {
		t.Fatalf("first = %q", first)
	}
	if second != ".attachments.3/notes (1)" {
		t.Fatalf("second = %q, want .attachments.3/notes (1)", second)
	}
}

func TestSaveSanitizesPathLikeName(t *testing.T) {
	s, dataDir := newTestStore(t)

	apiPath, err := s.Save(4, "../../evil.txt", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if apiPath != ".attachments.4/evil.txt" {
		t.Fatalf("apiPath = %q, want .attachments.4/evil.txt", apiPath)
	}

	inside := filepath.Join(dataDir, "attachments", "4", "evil.txt")
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("expected file inside the note dir: %v", err)
	}
	outside := filepath.Join(dataDir, "attachments", "evil.txt")
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file escaped the note dir: %v", err)
	}
}

func TestSaveRejectsEmptyName(t *testing.T) {
	s, _ := newTestStore(t)

	for _, name := range []string{"", ".", ".."} {
		if _, err := s.Save(5, name, strings.NewReader("x")); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("Save(%q) error = %v, want ErrInvalidPath", name, err)
		}
	}
}

func TestSaveTooLarge(t *testing.T) {
	s, _ := newTestStore(t)

	_, err := s.Save(9, "big.bin", io.LimitReader(zeroReader{}, maxAttachmentBytes+1))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}

	// No partial or temp files must be left behind.
	entries, err := os.ReadDir(filepath.Join(s.root, "9"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("leftover files after a failed upload: %v", entries)
	}
}

func TestOpenRejectsInvalidPaths(t *testing.T) {
	s, _ := newTestStore(t)

	if _, err := s.Save(7, "photo.png", strings.NewReader("x")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	bad := []string{
		"",
		"photo.png",
		".attachments.7/",
		".attachments.7/..",
		".attachments.7/../secret",
		".attachments.7/sub/photo.png",
		".attachments.8/photo.png",
		".attachments.7photo.png",
	}
	for _, p := range bad {
		if _, _, _, err := s.Open(7, p); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("Open(%q) error = %v, want ErrInvalidPath", p, err)
		}
	}

	if _, _, _, err := s.Open(7, ".attachments.7/photo.png"); err != nil {
		t.Fatalf("Open of the valid path failed: %v", err)
	}
}

func TestDeleteNote(t *testing.T) {
	s, _ := newTestStore(t)

	apiPath, err := s.Save(11, "photo.png", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.DeleteNote(11); err != nil {
		t.Fatalf("DeleteNote: %v", err)
	}
	if _, _, _, err := s.Open(11, apiPath); err == nil {
		t.Fatal("Open after DeleteNote succeeded, want error")
	}
	// Deleting an unknown note is not an error.
	if err := s.DeleteNote(12); err != nil {
		t.Fatalf("DeleteNote(unknown) = %v, want nil", err)
	}
}

// zeroReader yields zero bytes forever, used to exercise the size cap without
// allocating a large buffer.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}
