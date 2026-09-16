// Package attachments stores note attachments on disk, keyed by note id.
// The Nextcloud Notes API v1.4 has no listing endpoint: clients discover
// attachments from the note content, so this package only exposes save, open
// and delete-by-note. Storage is keyed by note id (not by category), which
// makes the returned path category-independent and lets note deletion remove
// its attachments, matching the file-backed behaviour of real Nextcloud.
package attachments

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const maxAttachmentBytes = 32 << 20 // 32 MiB

// MaxAttachmentBytes is the per-attachment size cap, exported for the API
// layer's request-body limit (file cap plus a small multipart overhead).
const MaxAttachmentBytes = maxAttachmentBytes

var (
	// ErrTooLarge is returned by Save when the attachment exceeds the cap.
	ErrTooLarge = errors.New("attachment too large")
	// ErrInvalidPath is returned when a name or API path is not acceptable.
	ErrInvalidPath = errors.New("invalid attachment path")
)

// Store persists attachments under <root>/<noteID>/<storedName>.
type Store struct {
	root string
	log  *slog.Logger
}

// New prepares the attachments root directory under <dataDir>/attachments.
func New(dataDir string, log *slog.Logger) (*Store, error) {
	root := filepath.Join(dataDir, "attachments")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Store{root: root, log: log}, nil
}

// Save sanitizes filename, de-duplicates it, writes the stream atomically and
// returns the category-independent API path .attachments.<noteID>/<storedName>.
func (s *Store) Save(noteID int64, filename string, r io.Reader) (string, error) {
	base := filepath.Base(filename)
	if base == "" || base == "." || base == ".." {
		return "", ErrInvalidPath
	}

	dir := filepath.Join(s.root, strconv.FormatInt(noteID, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	storedName := dedupName(dir, base)
	dst := filepath.Join(dir, storedName)

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	n, err := io.Copy(tmp, io.LimitReader(r, maxAttachmentBytes+1))
	if err != nil {
		cleanup()
		return "", err
	}
	if n > maxAttachmentBytes {
		cleanup()
		return "", ErrTooLarge
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}

	return fmt.Sprintf(".attachments.%d/%s", noteID, storedName), nil
}

// dedupName returns base, or " (n)" inserted before the extension, until a
// free slot is found. It mirrors Nextcloud 6.1+ (names are kept, not randomised).
func dedupName(dir, base string) string {
	name, ext := base, ""
	if i := strings.LastIndexByte(base, '.'); i > 0 {
		name, ext = base[:i], base[i:]
	} else if i := strings.LastIndexByte(base, '.'); i == 0 {
		// Leading dot: the whole thing is the extension (hidden file).
		name, ext = "", base
	}
	if name == "" {
		name = "file"
	}
	candidate := base
	for n := 1; exists(filepath.Join(dir, candidate)); n++ {
		candidate = fmt.Sprintf("%s (%d)%s", name, n, ext)
	}
	return candidate
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Open validates apiPath against the exact form .attachments.<noteID>/<base>
// (single element, no "/", no "..") and opens the file for reading. It returns
// the file size and a content type detected from the first bytes.
func (s *Store) Open(noteID int64, apiPath string) (io.ReadCloser, int64, string, error) {
	prefix := fmt.Sprintf(".attachments.%d/", noteID)
	rest, ok := strings.CutPrefix(apiPath, prefix)
	if !ok {
		return nil, 0, "", ErrInvalidPath
	}
	base := filepath.Base(rest)
	if base == "" || base == "." || base == ".." || base != rest || strings.Contains(rest, "/") {
		return nil, 0, "", ErrInvalidPath
	}

	path := filepath.Join(s.root, strconv.FormatInt(noteID, 10), base)
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, "", err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, "", err
	}

	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		_ = f.Close()
		return nil, 0, "", err
	}
	head = head[:n]

	ct := "application/octet-stream"
	if n > 0 {
		ct = http.DetectContentType(head)
	}

	return &reader{prefix: head, f: f}, info.Size(), ct, nil
}

// reader replays the prefix bytes we consumed for content-type detection and
// then streams the rest of the file. It stays correct for zero-byte files.
type reader struct {
	prefix []byte
	f      *os.File
}

func (r *reader) Read(p []byte) (int, error) {
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	return r.f.Read(p)
}

func (r *reader) Close() error {
	return r.f.Close()
}

// DeleteNote removes <root>/<noteID>, returning nil if it does not exist.
func (s *Store) DeleteNote(noteID int64) error {
	err := os.RemoveAll(filepath.Join(s.root, strconv.FormatInt(noteID, 10)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
