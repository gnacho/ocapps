// Package news: wiring del módulo news de ocapps (SPEC §4.5, §8 H4).
//
// El módulo satisface la interfaz unificada common/module.Module (SPEC §4.5,
// promovida a common en H5) con las firmas exactas del SPEC.
//
// Constructor para H5: m, err := news.New(cfg, log) donde cfg es el
// *common/config.Config ya cargado y log el logger BASE (New le añade
// module=news vía common/log.Module). Un error de New significa "módulo
// failed": H5 registra el handler 503 en su namespace (D3) y sigue.
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

// dbFileName es el nombre histórico del fichero SQLite de news (SPEC §5.1:
// se conserva para que la copia 1:1 de la migración funcione tal cual).
const dbFileName = "ocnews.db"

var _ commonmodule.Module = (*Module)(nil)

// Module es el módulo news: News API v1.3 + API propia + scheduler de
// refresco/retención/WebSub.
type Module struct {
	enabled bool
	server  *api.Server
	sched   *scheduler.Scheduler
	store   *store.Store
	log     *slog.Logger

	runErr atomic.Value // error: lo fija Run si termina de forma anómala
}

// New construye el módulo a partir de la config unificada. Equivale al
// arranque histórico de ocnews (cmd/ocnews/main.go) pero sobre los paquetes
// common: store.Open+Migrate, cred (feedsecret), imgproxy (imgsecret +
// imgcache en <NewsDataDir>) y el validador Graph común con MultiTenant +
// glue de shadow users (internal/news/auth). En modo local se conserva el
// bootstrap del primer admin con OCAPPS_NEWS_AUTH_USER/PASS.
//
// log es el logger base del proceso; New le añade module=news.
func New(cfg *config.Config, log *slog.Logger) (*Module, error) {
	if log == nil {
		log = slog.Default()
	}
	log = commonlog.Module(log, "news")
	m := &Module{enabled: cfg.NewsEnabled, log: log}
	if !cfg.NewsEnabled {
		return m, nil
	}
	if err := cfg.ModuleErr("news"); err != nil {
		return nil, fmt.Errorf("config de news inválida: %w", err)
	}

	dataDir := cfg.NewsDataDir
	st, err := store.Open(filepath.Join(dataDir, dbFileName))
	if err != nil {
		return nil, err
	}
	m.store = st

	favicons, err := favicon.NewCache(filepath.Join(dataDir, "favicons"), log)
	if err != nil {
		return nil, err
	}
	imgs, err := imgproxy.New(dataDir, log)
	if err != nil {
		return nil, err
	}
	feedKey, err := cred.LoadOrCreateSecret(filepath.Join(dataDir, "feedsecret"))
	if err != nil {
		return nil, err
	}
	creds, err := cred.NewCipher(feedKey)
	if err != nil {
		return nil, err
	}

	// validador: opencloud (Graph común + shadow users) o local (bcrypt)
	var validator auth.Validator
	switch cfg.AuthMode {
	case "opencloud":
		gv := commonauth.NewGraphValidator(cfg.OpenCloudURL, commonauth.MultiTenant(), log)
		validator = auth.NewShadowValidator(gv, st, log)
		log.Info("auth: opencloud", "server", cfg.OpenCloudURL)
	default:
		validator = &auth.LocalValidator{Store: st}
		if cfg.News.AuthUser != "" && cfg.News.AuthPass != "" {
			hash, err := auth.HashPassword(cfg.News.AuthPass)
			if err != nil {
				return nil, err
			}
			created, err := st.BootstrapUser(cfg.News.AuthUser, hash)
			if err != nil {
				return nil, err
			}
			if created {
				log.Info("usuario admin bootstrap", "username", cfg.News.AuthUser)
			}
		} else {
			var count int
			if err := st.BootstrapCount(&count); err != nil {
				return nil, err
			}
			if count == 0 {
				return nil, errors.New("sin usuarios en BD: define OCAPPS_NEWS_AUTH_USER y OCAPPS_NEWS_AUTH_PASS para el bootstrap del primer admin")
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

	log.Info("módulo news listo",
		"db", filepath.Join(dataDir, dbFileName), "api", api.Base,
		"interval", cfg.News.FeedInterval, "max_gap", cfg.News.MaxGap,
		"retencion", cfg.News.Retention().String())
	return m, nil
}

// Name devuelve el nombre del módulo ("news").
func (m *Module) Name() string { return "news" }

// Enabled informa de si el módulo está habilitado (OCAPPS_NEWS_ENABLED).
func (m *Module) Enabled() bool { return m.enabled }

// Register monta las rutas del módulo en el mux compartido (SPEC §4.1).
// /healthz y /readyz los sirve common; aquí no se registran.
func (m *Module) Register(mux *http.ServeMux) {
	m.server.RegisterOn(mux)
}

// Run ejecuta los loops de background (scheduler: refresco de feeds,
// retención y suscripciones/renovaciones WebSub — las notificaciones ntfy
// las emite el refresher dentro de esos ciclos). Bloquea hasta que ctx se
// cancela (shutdown gracioso: el scheduler drena sus goroutines) y devuelve
// nil en apagado normal.
func (m *Module) Run(ctx context.Context) error {
	m.sched.Run(ctx)
	return nil
}

// Healthy informa a /healthz y /readyz (SPEC §4.5): error de una ejecución
// anómala de Run si la hubo; si no, ping a la SQLite del módulo.
func (m *Module) Healthy() error {
	if err, ok := m.runErr.Load().(error); ok && err != nil {
		return err
	}
	if m.store == nil { // módulo disabled: no degrada el readyz global
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.store.Ping(ctx); err != nil {
		return fmt.Errorf("sqlite: %w", err)
	}
	return nil
}
