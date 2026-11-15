// Package news: wiring of the ocapps news module (SPEC §4.5, §8 H4).
//
// The module satisfies the unified common/module.Module interface (SPEC §4.5,
// promoted to common in H5) with the exact SPEC signatures.
//
// Constructor for the wiring (H5): m, err := news.New(cfg, log, gv) where
// cfg is the already loaded *common/config.Config, log the BASE logger (New
// adds module=news through common/log.Module) and gv the process SHARED
// Graph validator (common/auth, MultiTenant policy, a single cache
// for the three modules, SPEC §6.2). gv is only used with AuthMode=opencloud,
// in local mode it can be nil. An error from New means "module failed":
// the wiring registers the 503 handler in its namespace (D3) and goes on.
package news

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"time"

	commonauth "github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/config"
	"github.com/gnacho/ocapps/internal/common/cred"
	"github.com/gnacho/ocapps/internal/common/imgproxy"
	commonlog "github.com/gnacho/ocapps/internal/common/log"
	commonmodule "github.com/gnacho/ocapps/internal/common/module"
	"github.com/gnacho/ocapps/internal/news/api"
	"github.com/gnacho/ocapps/internal/news/auth"
	"github.com/gnacho/ocapps/internal/news/extract"
	"github.com/gnacho/ocapps/internal/news/favicon"
	"github.com/gnacho/ocapps/internal/news/feed"
	"github.com/gnacho/ocapps/internal/news/notify"
	"github.com/gnacho/ocapps/internal/news/refresher"
	"github.com/gnacho/ocapps/internal/news/scheduler"
	"github.com/gnacho/ocapps/internal/news/store"
	"github.com/gnacho/ocapps/internal/news/websub"
)

// dbFileName is the historical name of the news SQLite file (SPEC §5.1:
// it is kept so that the 1:1 copy of the migration works as is).
const dbFileName = "ocnews.db"

var _ commonmodule.Module = (*Module)(nil)

// Module is the news module: News API v1.3 + own API + refresh/
// retention/WebSub scheduler.
type Module struct {
	enabled bool
	server  *api.Server
	sched   *scheduler.Scheduler
	store   *store.Store
	log     *slog.Logger

	runErr atomic.Value // error: Run sets it if it ends abnormally
}

// New builds the module from the unified config. It is equivalent to the
// historical boot of ocnews (cmd/ocnews/main.go) but on top of the common
// packages: store.Open+Migrate, cred (feedsecret), imgproxy (imgsecret +
// imgcache in <NewsDataDir>) and the SHARED Graph validator (gv, received
// from the wiring since H5, before it the module created it itself) with MultiTenant +
// shadow users glue (internal/news/auth). In local mode the bootstrap of the
// first admin with OCAPPS_NEWS_AUTH_USER/PASS is kept.
//
// log is the base logger of the process, New adds module=news to it.
func New(cfg *config.Config, log *slog.Logger, gv *commonauth.GraphValidator) (*Module, error) {
	if log == nil {
		log = slog.Default()
	}
	log = commonlog.Module(log, "news")
	m := &Module{enabled: cfg.NewsEnabled, log: log}
	if !cfg.NewsEnabled {
		return m, nil
	}
	if err := cfg.ModuleErr("news"); err != nil {
		return nil, fmt.Errorf("invalid news config: %w", err)
	}

	dataDir := cfg.NewsDataDir
	st, err := store.Open(filepath.Join(dataDir, dbFileName))
	if err != nil {
		return nil, err
	}
	// I2: every failure after opening the DB must close it before returning
	// (pattern of internal/notes/module.go), otherwise the *sql.DB is left
	// open and leaked when the wiring registers the module failed.
	fail := func(err error) (*Module, error) {
		_ = st.Close()
		return nil, err
	}
	m.store = st

	favicons, err := favicon.NewCache(filepath.Join(dataDir, "favicons"), log)
	if err != nil {
		return fail(err)
	}
	imgs, err := imgproxy.New(dataDir, log)
	if err != nil {
		return fail(err)
	}
	feedKey, err := cred.LoadOrCreateSecret(filepath.Join(dataDir, "feedsecret"))
	if err != nil {
		return fail(err)
	}
	creds, err := cred.NewCipher(feedKey)
	if err != nil {
		return fail(err)
	}

	// validator: opencloud (SHARED common Graph + shadow users) or local
	// (bcrypt). The wiring passes gv with the MultiTenant policy (SPEC §6.2).
	var validator auth.Validator
	switch cfg.AuthMode {
	case "opencloud":
		if gv == nil {
			return fail(errors.New("opencloud mode requires the shared Graph validator (gv nil)"))
		}
		validator = auth.NewShadowValidator(gv, st, log)
		log.Info("auth: opencloud", "server", cfg.OpenCloudURL)
	default:
		validator = &auth.LocalValidator{Store: st}
		if cfg.News.AuthUser != "" && cfg.News.AuthPass != "" {
			hash, err := auth.HashPassword(cfg.News.AuthPass)
			if err != nil {
				return fail(err)
			}
			created, err := st.BootstrapUser(cfg.News.AuthUser, hash)
			if err != nil {
				return fail(err)
			}
			if created {
				log.Info("admin bootstrap user", "username", cfg.News.AuthUser)
			}
		} else {
			var count int
			if err := st.BootstrapCount(&count); err != nil {
				return fail(err)
			}
			if count == 0 {
				return fail(errors.New("no users in the DB: set OCAPPS_NEWS_AUTH_USER and OCAPPS_NEWS_AUTH_PASS for the first admin bootstrap"))
			}
		}
	}

	fetcher := feed.NewHTTPFetcher(cfg.News.FetchTimeout)
	extractor := extract.New(cfg.News.FetchTimeout)
	notifier := notify.New(cfg.News.NtfyURL, cfg.News.NtfyTopic, log)
	refresh := refresher.New(st, fetcher, creds, log, cfg.News.FeedInterval, cfg.News.MaxGap, notifier)

	ws := websub.New()
	m.sched = scheduler.New(st, refresh, favicons, log, 30*time.Second, 4, cfg.News.Retention(), ws, cfg.News.PublicURL)
	m.server = api.NewServer(st, validator, fetcher, refresh, favicons, imgs, extractor, creds,
		cfg.News.Retention(), log)

	log.Info("news module ready",
		"db", filepath.Join(dataDir, dbFileName), "api", api.Base,
		"interval", cfg.News.FeedInterval, "max_gap", cfg.News.MaxGap,
		"retention", cfg.News.Retention().String())
	return m, nil
}

// Name returns the module name ("news").
func (m *Module) Name() string { return "news" }

// Enabled reports whether the module is enabled (OCAPPS_NEWS_ENABLED).
func (m *Module) Enabled() bool { return m.enabled }

// Register mounts the module routes on the shared mux (SPEC §4.1).
// /healthz and /readyz are served by common, they are not registered here.
func (m *Module) Register(mux *http.ServeMux) {
	m.server.RegisterOn(mux)
}

// Run runs the background loops (scheduler: feed refresh,
// retention and WebSub subscriptions/renewals, the ntfy notifications
// are emitted by the refresher inside those cycles). It blocks until ctx is
// cancelled (graceful shutdown: the scheduler drains its goroutines) and returns
// nil on a normal shutdown. A panic recovered in the scheduler main loop
// (I3) comes back as an error: it stays in runErr (Healthy) and the wiring
// marks the module failed without taking down the process (D3).
func (m *Module) Run(ctx context.Context) error {
	err := m.sched.Run(ctx)
	if err != nil && ctx.Err() == nil {
		m.runErr.Store(err)
	}
	return err
}

// Close closes the module SQLite (orderly shutdown, the wiring calls it
// by io.Closer type assertion after draining the Run, SPEC §4.5/H5).
func (m *Module) Close() error {
	if m.store != nil {
		return m.store.Close()
	}
	return nil
}

// Healthy reports to /healthz and /readyz (SPEC §4.5): error of an abnormal
// Run if there was one, otherwise a ping to the module SQLite.
func (m *Module) Healthy() error {
	if err, ok := m.runErr.Load().(error); ok && err != nil {
		return err
	}
	if m.store == nil { // disabled module: it does not degrade the global readyz
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.store.Ping(ctx); err != nil {
		return fmt.Errorf("sqlite: %w", err)
	}
	return nil
}
