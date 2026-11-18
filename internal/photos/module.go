// Package photos - photos module of the unified ocapps service (SPEC §4.5,
// §8-H3), MULTI-TENANT since H8 (SPEC §6.2): every OpenCloud user has its index
// (scoping by owner = oc_id), its in-memory DAV session and its scan on
// activity. The wiring is:
//
//	store (baseline §5.3 + multi-owner migration 002) -> mediasecret (cred,
//	Q3) -> ffmpeg check (Q4) -> session registry + scheduler -> api.Server.
//
// The module NO LONGER depends on OpenCloud to start: there is no global
// initBackend and no retry loop against Graph (H8 §7). Healthy() = SQLite ping.
// The resolution of personal spaces is lazy (first request/scan of the user).
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
	"github.com/gnacho/ocapps/internal/common/live"
	commonmodule "github.com/gnacho/ocapps/internal/common/module"
	"github.com/gnacho/ocapps/internal/photos/api"
	"github.com/gnacho/ocapps/internal/photos/exif"
	"github.com/gnacho/ocapps/internal/photos/geo"
	"github.com/gnacho/ocapps/internal/photos/store"
	"github.com/gnacho/ocapps/internal/photos/thumb"
)

// PublicPrefix is the public namespace of the module (SPEC §4.3). Alias of the
// constant in the api package (single source: route registration and video
// signing).
const PublicPrefix = api.PublicPrefix

// photosTopic is the topic of the stream events published by this module.
const photosTopic = api.TopicPhotos

// Module implements the common interface of SPEC §4.5 (internal/common/module).
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
	hub    *live.Hub
}

// New wires the module. cfg is the full unified config (the Photos section,
// the Common one and ModuleErr("photos") are used: a config error of the
// module - for example an invalid OCAPPS_PHOTOS_SCAN_EVERY or a malformed
// OCAPPS_PHOTOS_USERS - is an error of New and the module ends up failed in
// init, pattern D3 like news, I1) and gv the SHARED Graph validator of the
// process (since H5, the module derives its own with gv.WithPolicy(MultiTenant())
// - same cache, multiuser tenancy since H8, SPEC §6.2). If gv is nil an
// independent one is created (tests/standalone). Hard wiring errors (invalid
// config, database, mediasecret) -> error: the module is not built and the
// wiring registers its namespace as failed (SPEC §4.5/D3). The module does NOT
// touch OpenCloud in New (H8): it always starts and serves even when OpenCloud
// is down (the requests will fail in auth against Graph, but the process and
// /health live on).
//
// OCAPPS_PHOTOS_USER/APP_TOKEN are NO LONGER mandatory (H8). The static Bearer
// OCAPPS_PHOTOS_TOKEN is DEPRECATED: it keeps working only if Photos.User is
// configured (it maps to the legacy oc_id), Token without User is a config
// error (the token no longer has an identity).
func New(cfg *config.Config, log *slog.Logger, gv *auth.GraphValidator) (*Module, error) {
	// I1: without this check, an invalid module config that PARSES fine (for
	// example SCAN_EVERY=0s) built a "healthy" module that panicked in
	// time.NewTicker(<=0).
	if err := cfg.ModuleErr("photos"); err != nil {
		return nil, fmt.Errorf("invalid photos config: %w", err)
	}
	pcfg, common := cfg.Photos, cfg.Common
	if pcfg.Token != "" && pcfg.User == "" {
		return nil, fmt.Errorf("photos: OCAPPS_PHOTOS_TOKEN (deprecated) requires OCAPPS_PHOTOS_USER to map the token to the legacy owner")
	}
	dataDir := common.PhotosDataDir
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("photos: data dir: %w", err)
	}
	st, err := store.Open(filepath.Join(dataDir, "memories.db"))
	if err != nil {
		return nil, fmt.Errorf("photos: sqlite: %w", err)
	}
	// Q3: no hardcoded fallback, if there is no secret the module does not start.
	secret, err := cred.LoadOrCreateSecret(filepath.Join(dataDir, "mediasecret"))
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("photos: mediasecret: %w", err)
	}
	// Q4: ffmpeg is the only system dependency, its absence degrades the
	// service (video posters: the original is served) but does not prevent
	// serving.
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Warn("ffmpeg not found: video posters degraded (the original will be served)", "err", err)
	}
	if pcfg.Token != "" {
		log.Warn("OCAPPS_PHOTOS_TOKEN is DEPRECATED (H8): clients must use the OpenCloud OIDC Bearer, the static token maps to the legacy owner of OCAPPS_PHOTOS_USER",
			"user", pcfg.User)
	}

	if gv == nil { // standalone/tests: own validator with no shared cache
		gv = auth.NewGraphValidator(common.OpenCloudURL, auth.MultiTenant(), log)
	}
	validator := gv.WithPolicy(auth.MultiTenant())

	cacheDir := pcfg.CacheDir
	if cacheDir == "" {
		cacheDir = filepath.Join(dataDir, "thumbs")
	}
	thumbs, err := thumb.New(cacheDir)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("photos: thumb cache: %w", err)
	}
	reg := newRegistry(common.OpenCloudURL, pcfg.User, pcfg.AppToken, pcfg.ScanSpaces, log)
	exifW := exif.NewWorker(st, log)
	sched := newScheduler(reg, st, exifW, pcfg.ScanRoot, pcfg.ScanEvery, log)
	reg.touchHook = sched.maybeScan
	reg.scanHook = sched.requestScan
	geocoder := geo.New(st, log)
	// Invalidation channel (SSE): the client stops polling and reloads when
	// the server notifies a change of the owner.
	hub := live.NewHub()
	srv := api.New(st, thumbs, geocoder, validator, reg,
		api.Config{Token: pcfg.Token, Secret: secret}, log)
	srv.SetExifWorker(exifW)
	srv.SetHub(hub)
	sched.setHub(hub)

	return &Module{
		cfg: pcfg, dataDir: dataDir, log: log,
		st: st, secret: secret, validator: validator,
		apiSrv: srv, reg: reg, sched: sched, hub: hub,
	}, nil
}

func (m *Module) Name() string { return "photos" }

// CloseStreams closes the open SSE streams. The wiring calls it from
// http.Server.RegisterOnShutdown: without this, Shutdown waits for the running
// handlers and a stream never finishes on its own (it would exhaust the 10 s
// budget on every shutdown with open tabs).
func (m *Module) CloseStreams() {
	if m.hub != nil {
		m.hub.Close()
	}
}

// Enabled: if the module was built it was enabled, the OCAPPS_PHOTOS_ENABLED
// gate is applied by the wiring before calling New.
func (m *Module) Enabled() bool { return true }

// Register mounts the namespace of the module (SPEC §4.5). The module no longer
// has a remote init (H8): the real routes respond from startup.
func (m *Module) Register(mux *http.ServeMux) {
	mux.Handle(PublicPrefix+"/", m.apiSrv.Handler())
}

// Healthy: ping to the SQLite of the module like news/notes (M5). The module no
// longer depends on OpenCloud to be healthy (H8 §7).
func (m *Module) Healthy() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.st.Ping(ctx); err != nil {
		return fmt.Errorf("sqlite: %w", err)
	}
	return nil
}

// Run seeds the sessions of OCAPPS_PHOTOS_USERS, applies the backfill of the
// single-tenant index when applicable (H8 §5.3) and runs the scheduler (scans
// on activity + ticker + session purge). It returns when ctx is cancelled.
func (m *Module) Run(ctx context.Context) error {
	defer func() { _ = m.st.Close() }()
	m.seedUsers(ctx)
	m.backfill(ctx)
	m.sched.run(ctx)
	return nil
}

// seedUsers seeds the sessions of the users with an app token
// (OCAPPS_PHOTOS_USERS + legacy fold): Basic client, MeID and personal space.
// If one fails, the error is logged and that user is left without background
// scan, but the module goes on (H8 §7).
func (m *Module) seedUsers(ctx context.Context) {
	for _, u := range m.cfg.Users {
		owner, err := m.reg.seed(ctx, u.User, u.Token)
		if err != nil {
			m.log.Error("photos: could not seed the background scan user, it is left without a scheduled scan",
				"user", u.User, "err", err)
			continue
		}
		m.log.Info("background scan user seeded", "user", u.User, "owner", owner)
	}
}

// backfill adopts the rows of the single-tenant era (owner empty) into the oc_id
// of the legacy user (H8 §5.3). It cannot be SQL: the oc_id is resolved against
// Graph with the app token. Without OCAPPS_PHOTOS_USER+APP_TOKEN configured the
// module starts anyway and leaves an explicit WARN with the count.
func (m *Module) backfill(ctx context.Context) {
	orphanAssets, orphanAlbums, err := m.st.OrphanCounts(ctx)
	if err != nil {
		m.log.Error("backfill: could not count rows without owner", "err", err)
		return
	}
	if orphanAssets == 0 && orphanAlbums == 0 {
		return
	}
	if m.cfg.User == "" || m.cfg.AppToken == "" {
		m.log.Warn(fmt.Sprintf("there are %d assets and %d albums from the single-tenant era without owner, configure OCAPPS_PHOTOS_USER + OCAPPS_PHOTOS_APP_TOKEN (once) to adopt them", orphanAssets, orphanAlbums),
			"assets", orphanAssets, "albums", orphanAlbums)
		return
	}
	owner, err := m.reg.LegacyOwner(ctx)
	if err != nil {
		m.log.Error("backfill: could not resolve the oc_id of the legacy user, it will be retried on the next start", "err", err)
		return
	}
	nAssets, nAlbums, rmAssets, rmAlbums, err := m.st.BackfillOwner(ctx, owner)
	if err != nil {
		m.log.Error("backfill", "err", err)
		return
	}
	if rmAssets > 0 || rmAlbums > 0 {
		m.log.Warn("multi-owner backfill: conflicting orphan rows discarded (the rescaned rows of the owner already represented them)",
			"owner", owner, "assets", rmAssets, "albums", rmAlbums)
	}
	m.log.Info("backfill multi-owner: single-tenant era rows adopted",
		"owner", owner, "assets", nAssets, "albums", nAlbums)
}
