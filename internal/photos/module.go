// Package photos — módulo photos del servicio unificado ocapps (SPEC §4.5,
// §8-H3), MULTI-TENANT desde H8 (SPEC §6.2): cualquier usuario de OpenCloud
// tiene su índice (scoping por owner = oc_id), su sesión DAV en memoria y su
// scan por actividad. El wiring es:
//
//	store (baseline §5.3 + migración 002 multi-owner) → mediasecret (cred,
//	Q3) → ffmpeg check (Q4) → registry de sesiones + scheduler → api.Server.
//
// El módulo YA NO depende de OpenCloud para arrancar: no hay initBackend
// global ni retry loop contra Graph (H8 §7). Healthy() = ping SQLite. La
// resolución de espacios personales es lazy (primer request/scan del
// usuario).
package photos

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/config"
	"github.com/gnacho/ocapps/internal/common/cred"
	commonmodule "github.com/gnacho/ocapps/internal/common/module"
	"github.com/gnacho/ocapps/internal/photos/api"
	"github.com/gnacho/ocapps/internal/photos/exif"
	"github.com/gnacho/ocapps/internal/photos/geo"
	"github.com/gnacho/ocapps/internal/photos/store"
	"github.com/gnacho/ocapps/internal/photos/thumb"
)

// PublicPrefix es el namespace público del módulo (SPEC §4.3). Alias de la
// constante del paquete api (fuente única: registro de rutas y firma de vídeo).
const PublicPrefix = api.PublicPrefix

// Module implementa la interfaz común de SPEC §4.5 (internal/common/module).
var _ commonmodule.Module = (*Module)(nil)

type Module struct {
	cfg     config.PhotosConfig
	dataDir string
	log     *slog.Logger

	st        *store.Store
	secret    []byte
	validator *auth.GraphValidator

	apiSrv *api.Server
	reg    *registry
	sched  *scheduler
}

// New cablea el módulo. cfg es la config unificada completa (se usa la
// sección Photos, la Common y ModuleErr("photos"): un error de config del
// módulo — p. ej. OCAPPS_PHOTOS_SCAN_EVERY inválido u OCAPPS_PHOTOS_USERS
// malformado — es error de New y el módulo queda failed en init, patrón D3
// como news, I1) y gv el validador Graph COMPARTIDO del proceso (desde H5;
// el módulo deriva el suyo con gv.WithPolicy(MultiTenant()) — misma caché,
// tenancy multiusuario desde H8, SPEC §6.2). Si gv es nil se crea uno
// independiente (tests/standalone). Errores de wiring duro (config inválida,
// BD, mediasecret) → error: el módulo no se construye y el wiring registra
// su namespace como failed (SPEC §4.5/D3). El módulo NO toca OpenCloud en
// New (H8): arranca siempre y sirve aunque OpenCloud esté caído (los
// requests fallarán en auth contra Graph, pero el proceso y /health viven).
//
// OCAPPS_PHOTOS_USER/APP_TOKEN ya NO son obligatorios (H8). El Bearer
// estático OCAPPS_PHOTOS_TOKEN está DEPRECATED: sigue funcionando solo si
// Photos.User está configurado (mapea al oc_id legacy); Token sin User es
// error de config (el token ya no tiene identidad).
func New(cfg *config.Config, log *slog.Logger, gv *auth.GraphValidator) (*Module, error) {
	// I1: sin este chequeo, una config de módulo inválida que PARSEA bien
	// (p. ej. SCAN_EVERY=0s) construía un módulo "sano" que paniqueaba en
	// time.NewTicker(≤0).
	if err := cfg.ModuleErr("photos"); err != nil {
		return nil, fmt.Errorf("config de photos inválida: %w", err)
	}
	pcfg, common := cfg.Photos, cfg.Common
	if pcfg.Token != "" && pcfg.User == "" {
		return nil, fmt.Errorf("photos: OCAPPS_PHOTOS_TOKEN (deprecated) exige OCAPPS_PHOTOS_USER para mapear el token al owner legacy")
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
	// (pósters de vídeo: se sirve el original) pero no impide servir.
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Warn("ffmpeg no encontrado: pósters de vídeo degradados (se servirá el original)", "err", err)
	}
	if pcfg.Token != "" {
		log.Warn("OCAPPS_PHOTOS_TOKEN está DEPRECATED (H8): los clientes deben usar el Bearer OIDC de OpenCloud; el token estático mapea al owner legacy de OCAPPS_PHOTOS_USER",
			"user", pcfg.User)
	}

	if gv == nil { // standalone/tests: validador propio sin caché compartida
		gv = auth.NewGraphValidator(common.OpenCloudURL, auth.MultiTenant(), log)
	}
	validator := gv.WithPolicy(auth.MultiTenant())

	thumbs, err := thumb.New(filepath.Join(dataDir, "thumbs"))
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("photos: thumb cache: %w", err)
	}
	reg := newRegistry(common.OpenCloudURL, pcfg.User, pcfg.AppToken, log)
	exifW := exif.NewWorker(st, log)
	sched := newScheduler(reg, st, exifW, pcfg.ScanRoot, pcfg.ScanEvery, log)
	reg.touchHook = sched.maybeScan
	reg.scanHook = sched.requestScan
	geocoder := geo.New(st, log)
	srv := api.New(st, thumbs, geocoder, validator, reg,
		api.Config{Token: pcfg.Token, Secret: secret}, log)

	return &Module{
		cfg: pcfg, dataDir: dataDir, log: log,
		st: st, secret: secret, validator: validator,
		apiSrv: srv, reg: reg, sched: sched,
	}, nil
}

func (m *Module) Name() string { return "photos" }

// Enabled: si el módulo se construyó es que estaba enabled; el gate
// OCAPPS_PHOTOS_ENABLED lo aplica el wiring antes de llamar a New.
func (m *Module) Enabled() bool { return true }

// Register monta el namespace del módulo (SPEC §4.5). El módulo ya no tiene
// init remoto (H8): las rutas reales responden desde el arranque.
func (m *Module) Register(mux *http.ServeMux) {
	mux.Handle(PublicPrefix+"/", m.apiSrv.Handler())
}

// Healthy: ping a la SQLite del módulo como news/notes (M5). El módulo ya no
// depende de OpenCloud para estar sano (H8 §7).
func (m *Module) Healthy() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.st.Ping(ctx); err != nil {
		return fmt.Errorf("sqlite: %w", err)
	}
	return nil
}

// Run siembra las sesiones de OCAPPS_PHOTOS_USERS, aplica el backfill del
// índice single-tenant si procede (H8 §5.3) y ejecuta el scheduler (scans
// por actividad + ticker + purga de sesiones). Retorna al cancelar ctx.
func (m *Module) Run(ctx context.Context) error {
	defer func() { _ = m.st.Close() }()
	m.seedUsers(ctx)
	m.backfill(ctx)
	m.sched.run(ctx)
	return nil
}

// seedUsers siembra las sesiones de los usuarios con app-token
// (OCAPPS_PHOTOS_USERS + pliegue legacy): cliente Basic, MeID y espacio
// personal. Si uno falla, log ERROR y ese usuario queda sin background scan,
// pero el módulo sigue (H8 §7).
func (m *Module) seedUsers(ctx context.Context) {
	for _, u := range m.cfg.Users {
		owner, err := m.reg.seed(ctx, u.User, u.Token)
		if err != nil {
			m.log.Error("photos: no se pudo sembrar el usuario de background scan; queda sin scan programado",
				"user", u.User, "err", err)
			continue
		}
		m.log.Info("usuario de background scan sembrado", "user", u.User, "owner", owner)
	}
}

// backfill adopta las filas de la era single-tenant (owner=”) al oc_id del
// usuario legacy (H8 §5.3). No puede ser SQL: el oc_id se resuelve contra
// Graph con el app-token. Sin OCAPPS_PHOTOS_USER+APP_TOKEN configurados el
// módulo arranca igual y deja un WARN explícito con el recuento.
func (m *Module) backfill(ctx context.Context) {
	orphanAssets, orphanAlbums, err := m.st.OrphanCounts(ctx)
	if err != nil {
		m.log.Error("backfill: no se pudo contar filas sin owner", "err", err)
		return
	}
	if orphanAssets == 0 && orphanAlbums == 0 {
		return
	}
	if m.cfg.User == "" || m.cfg.AppToken == "" {
		m.log.Warn(fmt.Sprintf("hay %d assets y %d álbumes de la era single-tenant sin owner; configura OCAPPS_PHOTOS_USER + OCAPPS_PHOTOS_APP_TOKEN (una vez) para adoptarlos", orphanAssets, orphanAlbums),
			"assets", orphanAssets, "albums", orphanAlbums)
		return
	}
	owner, err := m.reg.LegacyOwner(ctx)
	if err != nil {
		m.log.Error("backfill: no se pudo resolver el oc_id del usuario legacy; se reintentará en el próximo arranque", "err", err)
		return
	}
	nAssets, nAlbums, rmAssets, rmAlbums, err := m.st.BackfillOwner(ctx, owner)
	if err != nil {
		m.log.Error("backfill", "err", err)
		return
	}
	if rmAssets > 0 || rmAlbums > 0 {
		m.log.Warn("backfill multi-owner: filas huérfanas conflictivas descartadas (ya existían re-escaneadas para el owner)",
			"owner", owner, "assets", rmAssets, "albums", rmAlbums)
	}
	m.log.Info("backfill multi-owner: filas de la era single-tenant adoptadas",
		"owner", owner, "assets", nAssets, "albums", nAlbums)
}
