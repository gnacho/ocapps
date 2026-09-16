// Package notes: módulo Notes del servicio unificado ocapps (SPEC §8 H2).
// Cablea la apertura SQLite común + adopción de la BD (backup pre-migración y
// backfill de owner, SPEC §5.3), el imgproxy común (instancia propia con su
// imgsecret/imgcache en el subdir de notes), los adjuntos y el servidor de la
// API Notes/OCS sobre el validador Graph compartido (SPEC §6).
//
// NOTA para H5: la interfaz Module (SPEC §4.5) no quedó definida en common
// tras H1, así que se define aquí de forma autocontenida. Si H5 la promueve a
// un paquete común, basta borrar esta definición e importar la común: las
// firmas del módulo no cambian.
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
	commonstore "github.com/gnacho/ocapps/internal/common/store"
	"github.com/gnacho/ocapps/internal/notes/api"
	"github.com/gnacho/ocapps/internal/notes/attachments"
	"github.com/gnacho/ocapps/internal/notes/store"
)

// Module es la interfaz de módulo del SPEC §4.5 que cmd/ocapps (H5) consume.
type Module interface {
	Name() string
	Enabled() bool
	// Register monta las rutas del módulo en el mux (o el handler 503 si failed).
	Register(mux *http.ServeMux)
	// Run ejecuta loops de background; debe retornar al cancelar ctx.
	Run(ctx context.Context) error
	// Healthy informa a /healthz y /readyz.
	Healthy() error
}

// Rutas públicas del módulo (contrato Notes API + OCS, SPEC §4.1/§4.2):
//   - /index.php/apps/notes/api/v1/   (subárbol; GET /v1/img público firmado)
//   - /index.php/apps/notes/api/v1.4/ (subárbol; adjuntos)
//   - /ocs/v2.php/cloud/capabilities  (OCS, exclusivo de notes tras §4.2)
//   - /ocs/v2.php/cloud/user          (OCS)
const (
	ocsCapabilitiesPath = "/ocs/v2.php/cloud/capabilities"
	ocsUserPath         = "/ocs/v2.php/cloud/user"
)

type module struct {
	enabled bool
	log     *slog.Logger
	db      *sql.DB
	server  *api.Server
}

// New construye el módulo notes con dependencias inyectadas (SPEC §4.5):
//   - cfg: configuración unificada (usa NotesEnabled, NotesDataDir y
//     Notes.Owner = OCAPPS_NOTES_OWNER para el backfill).
//   - logger: logger base; el módulo añade el atributo module=notes.
//   - validator: validador Graph COMPARTIDO (common/auth, política
//     MultiTenant): la semántica de shadow user en memoria y el escopado por
//     graph ID en la columna `user` se conservan.
//
// Un error aquí (BD que no abre, migración rota, backfill sin owner,
// imgsecret corrupto) debe degradar el módulo a 503, no tumbar el proceso
// (D3): es responsabilidad del wiring de H5 registrar el handler sustituto.
func New(cfg *config.Config, logger *slog.Logger, validator *auth.GraphValidator) (Module, error) {
	if logger == nil {
		logger = slog.Default()
	}
	m := &module{enabled: cfg.NotesEnabled, log: oclog.Module(logger, "notes")}
	if !m.enabled {
		return m, nil
	}

	dataDir := cfg.NotesDataDir
	dbPath := filepath.Join(dataDir, "notes.db")

	db, err := commonstore.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("notes: open sqlite: %w", err)
	}
	fail := func(err error) (Module, error) {
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
	m.server = api.NewServer(api.Base, st, validator, images, atts)
	return m, nil
}

func (m *module) Name() string { return "notes" }

func (m *module) Enabled() bool { return m.enabled }

// Register monta las rutas del contrato Notes en el mux compartido. El
// preflight OPTIONS lo responde el middleware CORS de la propia cadena del
// servidor (204 sin auth, SPEC §4.4), así que no hace falta registro aparte.
func (m *module) Register(mux *http.ServeMux) {
	if !m.enabled || m.server == nil {
		return
	}
	h := m.server.Router()
	mux.Handle(api.Base, h)
	mux.Handle(api.Base14, h)
	mux.Handle(ocsCapabilitiesPath, h)
	mux.Handle(ocsUserPath, h)
}

// Run no tiene tareas de fondo en notes: bloquea hasta la cancelación.
func (m *module) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Healthy hace ping a la BD del módulo (SPEC §4.5).
func (m *module) Healthy() error {
	if !m.enabled {
		return nil
	}
	if m.db == nil {
		return fmt.Errorf("notes: módulo no inicializado")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.db.PingContext(ctx); err != nil {
		return fmt.Errorf("notes: sqlite ping: %w", err)
	}
	return nil
}

// Close cierra la BD del módulo (para apagado ordenado en H5).
func (m *module) Close() error {
	if m.db != nil {
		return m.db.Close()
	}
	return nil
}
