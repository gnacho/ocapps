// Package photos — módulo photos del servicio unificado ocapps (SPEC §4.5,
// §8-H3). El loop de scan (antes closures de cmd/photos-service/main.go) vive
// en Run; el wiring es:
//
//	store (baseline §5.3) → mediasecret (cred, Q3) → ffmpeg check (Q4) →
//	webdav + MeID (single-tenant §6.2) → ListDrives → api.Server.
//
// Si Graph/ListDrives falla al arrancar, el módulo queda en estado failed
// (503 en su namespace) y reintenta en background desde Run con backoff
// 30 s→5 min, en vez del os.Exit(1) histórico (SPEC §4.5).
package photos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/config"
	"github.com/gnacho/ocapps/internal/common/cred"
	"github.com/gnacho/ocapps/internal/common/httpx"
	"github.com/gnacho/ocapps/internal/common/webdav"
	"github.com/gnacho/ocapps/internal/photos/api"
	"github.com/gnacho/ocapps/internal/photos/exif"
	"github.com/gnacho/ocapps/internal/photos/geo"
	"github.com/gnacho/ocapps/internal/photos/index"
	"github.com/gnacho/ocapps/internal/photos/store"
	"github.com/gnacho/ocapps/internal/photos/thumb"
	"github.com/gnacho/ocapps/internal/photos/video"
)

// PublicPrefix es el namespace público del módulo (SPEC §4.3). Alias de la
// constante del paquete api (fuente única: registro de rutas y firma de vídeo).
const PublicPrefix = api.PublicPrefix

// Interface es la interfaz de módulo de SPEC §4.5 que cmd/ocapps consume en
// el wiring (H5). Se declara aquí a falta de un paquete common/module; las
// firmas son las del SPEC.
type Interface interface {
	Name() string
	Enabled() bool
	// Register monta las rutas del módulo en el mux (o el handler 503 si failed).
	Register(mux *http.ServeMux)
	// Run ejecuta los loops de background; retorna al cancelar ctx.
	Run(ctx context.Context) error
	// Healthy informa a /healthz y /readyz.
	Healthy() error
}

var _ Interface = (*Module)(nil)

// Backoff del reintento de init en background (SPEC §4.5).
const (
	retryInitDelay = 30 * time.Second
	retryInitMax   = 5 * time.Minute
	initTimeout    = 60 * time.Second
)

type Module struct {
	cfg     config.PhotosConfig
	ocURL   string
	dataDir string
	log     *slog.Logger

	st        *store.Store
	secret    []byte
	dav       *webdav.Client
	validator *auth.GraphValidator

	mu        sync.Mutex
	apiSrv    *api.Server // nil hasta que initBackend tiene éxito
	scanner   *index.Scanner
	exifW     *exif.Worker
	webdavURL string
	initErr   error // último error de init (Graph/ListDrives)
}

// New cablea el módulo. cfg es la sección photos de la config unificada y
// common la común (OpenCloudURL + PhotosDataDir). Errores de wiring duro (BD,
// mediasecret, credenciales vacías) → error: el módulo no se construye y el
// wiring debe registrar su namespace como failed (SPEC §4.5/D3). Un fallo de
// Graph/ListDrives NO es error de New: el módulo arranca en estado failed y
// reintenta en background desde Run.
func New(cfg config.PhotosConfig, common config.Common, log *slog.Logger) (*Module, error) {
	if cfg.User == "" || cfg.AppToken == "" {
		return nil, fmt.Errorf("photos: OCAPPS_PHOTOS_USER / OCAPPS_PHOTOS_APP_TOKEN son obligatorios con el módulo enabled")
	}
	dataDir := common.PhotosDataDir
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("photos: data dir: %w", err)
	}
	st, err := store.Open(filepath.Join(dataDir, "memories.db"))
	if err != nil {
		return nil, fmt.Errorf("photos: sqlite: %w", err)
	}
	// Q3: sin fallback hardcodeado; si no hay secreto, el módulo no arranca.
	secret, err := cred.LoadOrCreateSecret(filepath.Join(dataDir, "mediasecret"))
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("photos: mediasecret: %w", err)
	}
	// Q4: ffmpeg es la única dependencia de sistema; su ausencia degrada
	// (pósters de vídeo / HLS) pero no impide servir.
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Warn("ffmpeg no encontrado: pósters de vídeo y HLS degradados", "err", err)
	}

	dc := webdav.New(common.OpenCloudURL, cfg.User, cfg.AppToken)
	m := &Module{
		cfg: cfg, ocURL: common.OpenCloudURL, dataDir: dataDir, log: log,
		st: st, secret: secret, dav: dc,
	}

	// Identidad del usuario configurado: el servicio es single-tenant y solo
	// atiende su sesión. Si MeID falla, se omite el chequeo con log (como hoy).
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	meID, err := dc.MeID(ctx)
	cancel()
	var policy auth.Policy
	if err != nil {
		m.log.Warn("no se pudo resolver el id del usuario (se omite la comprobación de sesión)", "err", err)
		policy = auth.MultiTenant()
	} else {
		m.log.Info("usuario del servicio", "id", meID)
		policy = auth.SingleTenant(meID)
	}
	m.validator = auth.NewGraphValidator(common.OpenCloudURL, policy, log)

	// Primer intento de init contra Graph; si falla, Run reintenta.
	ictx, icancel := context.WithTimeout(context.Background(), initTimeout)
	err = m.initBackend(ictx)
	icancel()
	if err != nil {
		m.setInitErr(err)
		m.log.Error("photos: init contra OpenCloud fallido; se reintentará en background", "err", err)
	}
	return m, nil
}

func (m *Module) Name() string { return "photos" }

// Enabled: si el módulo se construyó es que estaba enabled; el gate
// OCAPPS_PHOTOS_ENABLED lo aplica el wiring antes de llamar a New.
func (m *Module) Enabled() bool { return true }

// Register monta el namespace del módulo. Si el backend aún no está listo
// (init contra Graph pendiente de reintento) sirve 503 en todo el namespace,
// como manda SPEC §4.5; al completarse el reintento las rutas reales empiezan
// a responder sin re-registrar nada.
func (m *Module) Register(mux *http.ServeMux) {
	mux.Handle(PublicPrefix+"/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := m.handler(); h != nil {
			h.ServeHTTP(w, r)
			return
		}
		httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "module photos unavailable"})
	}))
}

// Healthy: nil si el backend está operativo; el error de init si está failed.
func (m *Module) Healthy() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.apiSrv != nil {
		return nil
	}
	if m.initErr != nil {
		return fmt.Errorf("failed: %w", m.initErr)
	}
	return errors.New("inicializando")
}

func (m *Module) handler() http.Handler {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.apiSrv == nil {
		return nil
	}
	return m.apiSrv.Handler()
}

func (m *Module) setInitErr(err error) {
	m.mu.Lock()
	m.initErr = err
	m.mu.Unlock()
}

// initBackend descubre el espacio personal y construye scanner, cachés y la
// API. Es idempotente: los reintentos pisan el estado anterior por completo.
func (m *Module) initBackend(ctx context.Context) error {
	drives, err := m.dav.ListDrives(ctx)
	if err != nil {
		return fmt.Errorf("listar espacios de OpenCloud: %w", err)
	}
	var webdavURL string
	for _, d := range drives {
		if d.DriveType == "personal" {
			webdavURL = d.WebDAVURL
			break
		}
	}
	if webdavURL == "" && len(drives) > 0 {
		webdavURL = drives[0].WebDAVURL
	}
	if webdavURL == "" {
		return errors.New("ningún espacio con webDavUrl disponible")
	}

	thumbs, err := thumb.New(m.dav, filepath.Join(m.dataDir, "thumbs"))
	if err != nil {
		return fmt.Errorf("thumb cache: %w", err)
	}
	transcoder, err := video.New(m.dav, filepath.Join(m.dataDir, "hls"))
	if err != nil {
		return fmt.Errorf("hls cache: %w", err)
	}
	scanner := index.NewScanner(m.dav, webdav.DefaultOptions(), m.st, m.log)
	exifW := exif.NewWorker(m.dav, m.st, m.log)
	geocoder := geo.New(m.st, m.log)
	srv := api.New(m.st, thumbs, m.dav, geocoder, transcoder, m.validator,
		api.Config{WebDAVURL: webdavURL, Token: m.cfg.Token, Secret: m.secret}, m.log)

	m.mu.Lock()
	m.webdavURL = webdavURL
	m.scanner = scanner
	m.exifW = exifW
	m.apiSrv = srv
	m.initErr = nil
	m.mu.Unlock()
	m.log.Info("espacio OpenCloud localizado", "webdav", webdavURL, "root", m.cfg.ScanRoot)
	return nil
}

// Run ejecuta el loop de scan (inicial + ticker SCAN_EVERY + rescans bajo
// demanda con throttle de 20 s). Si el backend no está listo, primero
// reintenta el init en background (backoff 30 s→5 min, SPEC §4.5). Retorna al
// cancelar ctx.
func (m *Module) Run(ctx context.Context) error {
	defer func() { _ = m.st.Close() }()

	if m.handler() == nil && !m.retryInit(ctx) {
		return nil // shutdown durante el reintento
	}
	m.scanLoop(ctx)
	return nil
}

// retryInit reintenta initBackend hasta que tenga éxito o se cancele ctx.
func (m *Module) retryInit(ctx context.Context) bool {
	delay := retryInitDelay
	for {
		ictx, cancel := context.WithTimeout(ctx, initTimeout)
		err := m.initBackend(ictx)
		cancel()
		if err == nil {
			m.log.Info("photos: init recuperado tras reintento en background")
			return true
		}
		m.setInitErr(err)
		m.log.Error("photos: reintento de init fallido", "err", err, "proximo", delay)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(delay):
		}
		if delay < retryInitMax {
			delay *= 2
			if delay > retryInitMax {
				delay = retryInitMax
			}
		}
	}
}

// scanLoop es el port de las closures de cmd/photos-service/main.go: scan
// inicial, ticker programado y rescans bajo demanda (los pide la extensión al
// abrir/enfocar) con throttle de 20 s para no encadenar PROPFINDs completos.
func (m *Module) scanLoop(ctx context.Context) {
	m.mu.Lock()
	scanner, exifW, webdavURL, rescan := m.scanner, m.exifW, m.webdavURL, m.apiSrv.RescanRequests()
	m.mu.Unlock()

	var scanMu sync.Mutex
	lastScan := time.Time{}
	runScan := func(reason string) {
		sctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
		defer cancel()
		m.log.Info("scan", "motivo", reason)
		if err := scanner.ScanSpace(sctx, webdavURL, m.cfg.ScanRoot); err != nil {
			m.log.Error("scan", "err", err)
		}
		exifW.Run(sctx)
		scanMu.Lock()
		lastScan = time.Now()
		scanMu.Unlock()
	}

	runScan("inicial")
	ticker := time.NewTicker(m.cfg.ScanEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runScan("programado")
		case <-rescan:
			scanMu.Lock()
			recent := time.Since(lastScan) < 20*time.Second
			scanMu.Unlock()
			if recent {
				continue
			}
			runScan("bajo demanda")
		}
	}
}
