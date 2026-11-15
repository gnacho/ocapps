// Package notes: Notes module of the unified ocapps service (SPEC §8 H2).
// It wires the shared SQLite opening plus database adoption (pre-migration
// backup and owner backfill, SPEC §5.3), the shared imgproxy (its own instance
// with its imgsecret/imgcache in the notes subdir), attachments and the
// Notes/OCS API server on top of the shared Graph validator (SPEC §6).
//
// The Module interface (SPEC §4.5) lives in internal/common/module since H5.
// This module satisfies it (see the var _ below).
package notes

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/config"
	"github.com/gnacho/ocapps/internal/common/imgproxy"
	oclog "github.com/gnacho/ocapps/internal/common/log"
	commonmodule "github.com/gnacho/ocapps/internal/common/module"
	commonstore "github.com/gnacho/ocapps/internal/common/store"
	"github.com/gnacho/ocapps/internal/notes/api"
	"github.com/gnacho/ocapps/internal/notes/attachments"
	"github.com/gnacho/ocapps/internal/notes/store"
)

var _ commonmodule.Module = (*notesModule)(nil)

// Public routes of the module (Notes API plus OCS contract, SPEC §4.1/§4.2):
//   - /index.php/apps/notes/api/v1/   (subtree, GET /v1/img is public and signed)
//   - /index.php/apps/notes/api/v1.4/ (subtree, attachments)
//   - /ocs/v2.php/cloud/capabilities  (OCS, exclusive to notes after §4.2)
//   - /ocs/v2.php/cloud/user          (OCS)
const (
	ocsCapabilitiesPath = "/ocs/v2.php/cloud/capabilities"
	ocsUserPath         = "/ocs/v2.php/cloud/user"
)

// notesModule implements common/module.Module. (The type is NOT called "module"
// to avoid a collision with the imported common package.)
type notesModule struct {
	enabled bool
	log     *slog.Logger
	db      *sql.DB
	server  *api.Server
}

// New builds the notes module with injected dependencies (SPEC §4.5):
//   - cfg: unified configuration (uses NotesEnabled, NotesDataDir and
//     Notes.Owner = OCAPPS_NOTES_OWNER for the backfill).
//   - logger: base logger, the module adds the module=notes attribute.
//   - validator: SHARED Graph validator (common/auth, MultiTenant policy):
//     the in-memory shadow user semantics and the scoping by graph ID on the
//     `user` column are preserved.
//
// An error here (database that does not open, broken migration, backfill
// without owner, corrupt imgsecret) must degrade the module to 503, never take
// the process down (D3): registering the replacement handler is the job of the
// H5 wiring.
func New(cfg *config.Config, logger *slog.Logger, validator *auth.GraphValidator) (commonmodule.Module, error) {
	if logger == nil {
		logger = slog.Default()
	}
	m := &notesModule{enabled: cfg.NotesEnabled, log: oclog.Module(logger, "notes")}
	if !m.enabled {
		return m, nil
	}

	dataDir := cfg.NotesDataDir
	dbPath := filepath.Join(dataDir, "notes.db")

	db, err := commonstore.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("notes: open sqlite: %w", err)
	}
	fail := func(err error) (commonmodule.Module, error) {
		_ = db.Close()
		return nil, err
	}

	st, err := store.Open(db, dbPath, cfg.Notes.Owner)
	if err != nil {
		return fail(fmt.Errorf("notes: %w", err))
	}

	images, err := imgproxy.New(dataDir, m.log)
	if err != nil {
		return fail(fmt.Errorf("notes: image proxy: %w", err))
	}

	atts, err := attachments.New(dataDir, m.log)
	if err != nil {
		return fail(fmt.Errorf("notes: attachments: %w", err))
	}

	m.db = db
	m.server = api.NewServer(api.Base, cfg.OpenCloudURL, st, validator, images, atts)
	return m, nil
}

func (m *notesModule) Name() string { return "notes" }

func (m *notesModule) Enabled() bool { return m.enabled }

// Register mounts the Notes contract routes on the shared mux. The OPTIONS
// preflight is answered by the CORS middleware of the server chain itself (204
// with no auth, SPEC §4.4), so no extra registration is needed.
func (m *notesModule) Register(mux *http.ServeMux) {
	if !m.enabled || m.server == nil {
		return
	}
	h := m.server.Router()
	mux.Handle(api.Base, h)
	mux.Handle(api.Base14, h)
	mux.Handle(ocsCapabilitiesPath, h)
	mux.Handle(ocsUserPath, h)
}

// Run has no background tasks in notes: it blocks until cancellation.
func (m *notesModule) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Healthy pings the module database (SPEC §4.5).
func (m *notesModule) Healthy() error {
	if !m.enabled {
		return nil
	}
	if m.db == nil {
		return fmt.Errorf("notes: module not initialized")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.db.PingContext(ctx); err != nil {
		return fmt.Errorf("notes: sqlite ping: %w", err)
	}
	return nil
}

// Close closes the module database (for an orderly shutdown in H5).
func (m *notesModule) Close() error {
	if m.db != nil {
		return m.db.Close()
	}
	return nil
}
