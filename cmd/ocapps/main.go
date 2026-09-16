// ocapps: servicio Go unificado (news + notes + photos) para OpenCloud.
//
// H0 — esqueleto: config común (fail-fast) → logger JSON → mux con /healthz
// → shutdown gracioso. El wiring de módulos llega en H2-H5 (SPEC §8).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gnacho/ocapps/internal/common/config"
	"github.com/gnacho/ocapps/internal/common/httpx"
	oclog "github.com/gnacho/ocapps/internal/common/log"
)

// Versión por ldflags (goreleaser / Dockerfile):
//
//	-ldflags "-X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE"
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// Errores de la base común = fatal (SPEC §4.5/D3).
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := oclog.New(cfg.LogLevel)
	slog.SetDefault(log) // único SetDefault del proceso (Q9)

	mux := http.NewServeMux()
	// /healthz: liveness + estado por módulo (§4.5). H0: sin módulos aún.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"version": version,
			"commit":  commit,
			"date":    date,
			"modules": map[string]string{},
		})
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("ocapps escuchando",
			"version", version, "commit", commit, "date", date,
			"addr", cfg.ListenAddr, "data_dir", cfg.DataDir, "auth", cfg.AuthMode)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Info("apagando (señal)…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
