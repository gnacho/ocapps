// ocapps: unified Go service (news + notes + photos) for OpenCloud.
//
// H5 - unified wiring (SPEC §4.5, §8): common config (fail-fast) -> JSON logger
// -> shared Graph validator -> per module init with D3 degradation -> single mux
// with healthz/readyz -> supervisor of Run(ctx) with recover -> graceful
// shutdown (10 s) with Close of the modules.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	commonauth "github.com/gnacho/ocapps/internal/common/auth"
	"github.com/gnacho/ocapps/internal/common/config"
	"github.com/gnacho/ocapps/internal/common/httpx"
	oclog "github.com/gnacho/ocapps/internal/common/log"
	commonmodule "github.com/gnacho/ocapps/internal/common/module"
	"github.com/gnacho/ocapps/internal/extensions"
	"github.com/gnacho/ocapps/internal/news"
	newsapi "github.com/gnacho/ocapps/internal/news/api"
	"github.com/gnacho/ocapps/internal/notes"
	notesapi "github.com/gnacho/ocapps/internal/notes/api"
	"github.com/gnacho/ocapps/internal/photos"
)

// Version by ldflags (goreleaser / Dockerfile):
//
//	-ldflags "-X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE"
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// shutdownBudget is the total time of the graceful shutdown (SPEC §4.5):
// drain HTTP + module loops + Close.
const shutdownBudget = 10 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// Errors of the common base = fatal (SPEC §4.5/D3): without a valid common
	// config or a writable DATA_DIR there is nothing useful to serve.
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := oclog.New(cfg.LogLevel)
	slog.SetDefault(log) // the only SetDefault of the process (Q9)

	a := wire(cfg, log)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           a.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	// SSE streams do not end on their own: they have to be closed when the
	// shutdown starts, otherwise Shutdown would wait for those handlers until
	// the budget is exhausted.
	srv.RegisterOnShutdown(a.closeStreams)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Supervisor: one Run(ctx) per enabled module in its goroutine with
	// recover() (a panic marks the module failed, the process goes on, D3).
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	var wg sync.WaitGroup
	for _, e := range a.modules {
		if e.mod == nil || !e.enabled {
			continue
		}
		wg.Add(1)
		go e.supervise(runCtx, &wg, log)
	}

	mods, _ := a.healthSummary()
	errCh := make(chan error, 1)
	go func() {
		log.Info("ocapps listening",
			"version", version, "commit", commit, "date", date,
			"addr", cfg.ListenAddr, "data_dir", cfg.DataDir, "auth", cfg.AuthMode,
			"modules", mods)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down (signal)...")
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}

	// Graceful shutdown with a total budget of 10 s: first HTTP (stop accepting
	// and drain what is in flight), then the loops (cancel runCtx and wait) and
	// finally Close() of the modules that implement it (io.Closer).
	deadline := time.Now().Add(shutdownBudget)
	shutdownCtx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown http", "err", err)
	}
	runCancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
		log.Warn("shutdown timeout: module loops did not drain in time")
	}
	for _, e := range a.modules {
		c, ok := e.mod.(io.Closer)
		if !ok || e.mod == nil {
			continue
		}
		if err := c.Close(); err != nil {
			log.Warn("module close", "module", e.name, "err", err)
		}
	}
	return nil
}

// entry is the supervised state of a module in the wiring: its instance (or
// nil if the constructor failed, D3) and the first init/run/panic error.
type entry struct {
	name    string
	enabled bool
	mod     commonmodule.Module

	mu  sync.Mutex
	err error // init failed, Run ended with an error or a recovered panic
}

func (e *entry) setErr(err error) {
	e.mu.Lock()
	if e.err == nil {
		e.err = err
	}
	e.mu.Unlock()
}

// healthy: init/run error if there is one, otherwise the Healthy of the
// module. A disabled module never degrades the process.
func (e *entry) healthy() error {
	e.mu.Lock()
	err := e.err
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if !e.enabled || e.mod == nil {
		return nil
	}
	return e.mod.Healthy()
}

// status for /healthz and /readyz: "ok" | "failed: <err>" | "disabled".
func (e *entry) status() string {
	if !e.enabled {
		return "disabled"
	}
	if err := e.healthy(); err != nil {
		return "failed: " + err.Error()
	}
	return "ok"
}

// supervise runs the module Run with recover: a panic or a Run error (outside
// shutdown) marks the module failed without taking the process down (D3).
func (e *entry) supervise(ctx context.Context, wg *sync.WaitGroup, log *slog.Logger) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			e.setErr(fmt.Errorf("panic in Run: %v", r))
			log.Error("panic in module loop, module failed", "module", e.name, "panic", r)
		}
	}()
	if err := e.mod.Run(ctx); err != nil && ctx.Err() == nil {
		e.setErr(err)
		log.Error("module loop ended with an error, module failed", "module", e.name, "err", err)
	}
}

// app is the wired process: single mux + supervised modules.
type app struct {
	cfg     *config.Config
	log     *slog.Logger
	mux     *http.ServeMux
	modules []*entry
}

// streamCloser is implemented by the modules that keep open streams (SSE) and
// need to close them on shutdown.
type streamCloser interface {
	CloseStreams()
}

// closeStreams closes the streams of every module that has them. It runs at the
// beginning of http.Server.Shutdown (RegisterOnShutdown).
func (a *app) closeStreams() {
	for _, e := range a.modules {
		if e.mod == nil {
			continue
		}
		if c, ok := e.mod.(streamCloser); ok {
			c.CloseStreams()
		}
	}
}

// failedNamespaces: routes where the 503 substitute handler is registered when
// the module constructor fails (D3, SPEC §4.5). They match the namespaces of
// §4.1 that each module would register in a healthy state.
var failedNamespaces = map[string][]string{
	"news": {
		newsapi.Base + "/",
		"/api/me", "/api/me/",
		"/api/users", "/api/users/",
	},
	"notes": {
		notesapi.Base, notesapi.Base14,
		"/ocs/",
	},
	"photos": {photos.PublicPrefix + "/"},
}

// wire builds the process: shared Graph validator, per module init (D3
// degradation), single mux and healthz/readyz. It NEVER returns an error:
// module failures degrade, the fatal ones were already detected by config.Load.
func wire(cfg *config.Config, log *slog.Logger) *app {
	a := &app{cfg: cfg, log: log, mux: http.NewServeMux()}

	// SHARED Graph validator (SPEC §6.1/§6.2): a single cache + singleflight
	// for the whole process. news/notes use it with MultiTenant, photos derives
	// its own with WithPolicy(SingleTenant(meID)). In local mode (news only) it
	// is created anyway: with an empty URL every validation fails and the
	// authenticated notes routes respond 401, as it should.
	gv := commonauth.NewGraphValidator(cfg.OpenCloudURL, commonauth.MultiTenant(), log)

	a.modules = []*entry{
		a.wireNews(gv),
		a.wireNotes(gv),
		a.wirePhotos(gv),
	}

	for _, e := range a.modules {
		switch {
		case !e.enabled:
			// disabled module: it registers nothing and does not degrade readyz
		case e.mod == nil:
			for _, ns := range failedNamespaces[e.name] {
				a.mux.Handle(ns, commonmodule.Unavailable(e.name))
			}
		default:
			e.mod.Register(a.mux)
		}
	}

	a.mux.HandleFunc("GET /healthz", a.handleHealthz)
	a.mux.HandleFunc("GET /readyz", a.handleReadyz)
	a.wireExtensions(log)
	return a
}

// wireExtensions mounts the extension installer panel. It stays off unless an
// admin token is configured: the panel can install code and restart OpenCloud,
// so an unset token must mean "not available", never "available to anyone".
func (a *app) wireExtensions(log *slog.Logger) {
	token := os.Getenv("OCAPPS_ADMIN_TOKEN")
	if token == "" {
		return
	}
	confPath := envOr("OCAPPS_EXT_CONF", "/etc/ocapps/extensions.conf")
	appsDir := envOr("OCAPPS_EXT_APPS_DIR", "/etc/opencloud/web/assets/apps")
	helper := envOr("OCAPPS_EXT_HELPER", "/usr/local/sbin/ocapps-ext")

	conf, err := extensions.LoadConf(confPath)
	if err != nil {
		log.Warn("extensions panel disabled", "err", err, "conf", confPath)
		return
	}
	gh := extensions.NewGitHub(nil, os.Getenv("GITHUB_TOKEN"), 10*time.Minute)
	svc := extensions.NewService(appsDir, helper, true, conf, gh)
	extensions.Register(a.mux, svc, token)
	log.Info("extensions panel active", "apps", len(conf), "helper", helper)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (a *app) wireNews(gv *commonauth.GraphValidator) *entry {
	e := &entry{name: "news", enabled: a.cfg.NewsEnabled}
	if !e.enabled {
		return e
	}
	m, err := news.New(a.cfg, a.log, gv)
	if err != nil {
		e.setErr(err)
		a.log.Error("module failed on init, its namespace serves 503 (D3)", "module", "news", "err", err)
		return e
	}
	e.mod = m
	return e
}

func (a *app) wireNotes(gv *commonauth.GraphValidator) *entry {
	e := &entry{name: "notes", enabled: a.cfg.NotesEnabled}
	if !e.enabled {
		return e
	}
	m, err := notes.New(a.cfg, a.log, gv)
	if err != nil {
		e.setErr(err)
		a.log.Error("module failed on init, its namespace serves 503 (D3)", "module", "notes", "err", err)
		return e
	}
	e.mod = m
	return e
}

func (a *app) wirePhotos(gv *commonauth.GraphValidator) *entry {
	e := &entry{name: "photos", enabled: a.cfg.PhotosEnabled}
	if !e.enabled {
		return e
	}
	m, err := photos.New(a.cfg, a.log, gv)
	if err != nil {
		e.setErr(err)
		a.log.Error("module failed on init, its namespace serves 503 (D3)", "module", "photos", "err", err)
		return e
	}
	e.mod = m
	return e
}

// healthSummary: map name -> "ok"|"failed: <err>"|"disabled" and whether all
// the ENABLED modules are ok (readyz criterion).
func (a *app) healthSummary() (map[string]string, bool) {
	mods := make(map[string]string, len(a.modules))
	allOK := true
	for _, e := range a.modules {
		s := e.status()
		mods[e.name] = s
		if e.enabled && s != "ok" {
			allOK = false
		}
	}
	return mods, allOK
}

// handleHealthz: liveness + per module status (SPEC §4.5). 200 ALWAYS as long
// as the process is alive, even if some modules failed.
func (a *app) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	mods, _ := a.healthSummary()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"version": version,
		"commit":  commit,
		"date":    date,
		"modules": mods,
	})
}

// handleReadyz: 200 only if ALL the enabled modules are ok, 503 otherwise (same
// body as healthz).
func (a *app) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	mods, ok := a.healthSummary()
	code := http.StatusOK
	if !ok {
		code = http.StatusServiceUnavailable
	}
	httpx.WriteJSON(w, code, map[string]any{
		"version": version,
		"commit":  commit,
		"date":    date,
		"modules": mods,
	})
}
