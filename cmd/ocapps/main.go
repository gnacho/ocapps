// ocapps: servicio Go unificado (news + notes + photos) para OpenCloud.
//
// H5 — wiring unificado (SPEC §4.5, §8): config común (fail-fast) → logger
// JSON → validador Graph compartido → init por módulo con degradación D3 →
// mux único con healthz/readyz → supervisor de Run(ctx) con recover →
// shutdown gracioso (10 s) con Close de módulos.
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
	"github.com/gnacho/ocapps/internal/news"
	newsapi "github.com/gnacho/ocapps/internal/news/api"
	"github.com/gnacho/ocapps/internal/notes"
	notesapi "github.com/gnacho/ocapps/internal/notes/api"
	"github.com/gnacho/ocapps/internal/photos"
)

// Versión por ldflags (goreleaser / Dockerfile):
//
//	-ldflags "-X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE"
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// shutdownBudget es el tiempo total del apagado gracioso (SPEC §4.5):
// drenar HTTP + loops de módulos + Close.
const shutdownBudget = 10 * time.Second

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// Errores de la base común = fatal (SPEC §4.5/D3): sin config común
	// válida o DATA_DIR escribible no hay nada útil que servir.
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := oclog.New(cfg.LogLevel)
	slog.SetDefault(log) // único SetDefault del proceso (Q9)

	a := wire(cfg, log)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           a.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Supervisor: un Run(ctx) por módulo enabled en su goroutine con
	// recover() (un pánico marca el módulo failed, el proceso sigue, D3).
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
		log.Info("ocapps escuchando",
			"version", version, "commit", commit, "date", date,
			"addr", cfg.ListenAddr, "data_dir", cfg.DataDir, "auth", cfg.AuthMode,
			"modules", mods)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Info("apagando (señal)…")
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}

	// Shutdown gracioso con presupuesto total de 10 s: primero HTTP (deja de
	// aceptar y drena en curso), luego los loops (cancela runCtx y espera) y
	// por último Close() de los módulos que lo implementen (io.Closer).
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
		log.Warn("timeout de shutdown: loops de módulos no drenaron a tiempo")
	}
	for _, e := range a.modules {
		c, ok := e.mod.(io.Closer)
		if !ok || e.mod == nil {
			continue
		}
		if err := c.Close(); err != nil {
			log.Warn("close de módulo", "module", e.name, "err", err)
		}
	}
	return nil
}

// entry es el estado supervisado de un módulo en el wiring: su instancia (o
// nil si el constructor falló, D3) y el primer error de init/run/pánico.
type entry struct {
	name    string
	enabled bool
	mod     commonmodule.Module

	mu  sync.Mutex
	err error // init failed, Run terminó con error o pánico recuperado
}

func (e *entry) setErr(err error) {
	e.mu.Lock()
	if e.err == nil {
		e.err = err
	}
	e.mu.Unlock()
}

// healthy: error de init/run si lo hay; si no, el Healthy del módulo. Un
// módulo disabled nunca degrada el proceso.
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

// status para /healthz y /readyz: "ok" | "failed: <err>" | "disabled".
func (e *entry) status() string {
	if !e.enabled {
		return "disabled"
	}
	if err := e.healthy(); err != nil {
		return "failed: " + err.Error()
	}
	return "ok"
}

// supervise ejecuta Run del módulo con recover: un pánico o un error de Run
// (fuera de shutdown) marca el módulo failed sin tumbar el proceso (D3).
func (e *entry) supervise(ctx context.Context, wg *sync.WaitGroup, log *slog.Logger) {
	defer wg.Done()
	defer func() {
		if r := recover(); r != nil {
			e.setErr(fmt.Errorf("pánico en Run: %v", r))
			log.Error("pánico en loop de módulo; módulo failed", "module", e.name, "panic", r)
		}
	}()
	if err := e.mod.Run(ctx); err != nil && ctx.Err() == nil {
		e.setErr(err)
		log.Error("loop de módulo terminó con error; módulo failed", "module", e.name, "err", err)
	}
}

// app es el proceso cableado: mux único + módulos supervisados.
type app struct {
	cfg     *config.Config
	log     *slog.Logger
	mux     *http.ServeMux
	modules []*entry
}

// failedNamespaces: rutas donde se registra el handler sustituto 503 cuando
// el constructor del módulo falla (D3, SPEC §4.5). Coinciden con los
// namespaces del §4.1 que cada módulo registraría en estado sano.
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

// wire construye el proceso: validador Graph compartido, init por módulo
// (degradación D3), mux único y healthz/readyz. NUNCA devuelve error: los
// fallos de módulo degradan; los fatales los detectó config.Load antes.
func wire(cfg *config.Config, log *slog.Logger) *app {
	a := &app{cfg: cfg, log: log, mux: http.NewServeMux()}

	// Validador Graph COMPARTIDO (SPEC §6.1/§6.2): una sola caché +
	// singleflight para todo el proceso. news/notes lo usan con MultiTenant;
	// photos deriva el suyo con WithPolicy(SingleTenant(meID)). En modo
	// local (solo news) igualmente se crea: con URL vacía toda validación
	// falla y las rutas con auth de notes responden 401, como corresponde.
	gv := commonauth.NewGraphValidator(cfg.OpenCloudURL, commonauth.MultiTenant(), log)

	a.modules = []*entry{
		a.wireNews(gv),
		a.wireNotes(gv),
		a.wirePhotos(gv),
	}

	for _, e := range a.modules {
		switch {
		case !e.enabled:
			// módulo disabled: no registra nada ni degrada readyz
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
	return a
}

func (a *app) wireNews(gv *commonauth.GraphValidator) *entry {
	e := &entry{name: "news", enabled: a.cfg.NewsEnabled}
	if !e.enabled {
		return e
	}
	m, err := news.New(a.cfg, a.log, gv)
	if err != nil {
		e.setErr(err)
		a.log.Error("módulo failed en init; su namespace sirve 503 (D3)", "module", "news", "err", err)
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
		a.log.Error("módulo failed en init; su namespace sirve 503 (D3)", "module", "notes", "err", err)
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
	m, err := photos.New(a.cfg.Photos, a.cfg.Common, a.log, gv)
	if err != nil {
		e.setErr(err)
		a.log.Error("módulo failed en init; su namespace sirve 503 (D3)", "module", "photos", "err", err)
		return e
	}
	e.mod = m
	return e
}

// healthSummary: mapa name → "ok"|"failed: <err>"|"disabled" y si todos los
// módulos ENABLED están ok (criterio de readyz).
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

// handleHealthz: liveness + estado por módulo (SPEC §4.5). 200 SIEMPRE que
// el proceso viva, aunque haya módulos failed.
func (a *app) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	mods, _ := a.healthSummary()
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"version": version,
		"commit":  commit,
		"date":    date,
		"modules": mods,
	})
}

// handleReadyz: 200 solo si TODOS los módulos enabled están ok; 503 si no
// (mismo body que healthz).
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
