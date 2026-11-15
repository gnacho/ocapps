// Package log: unified slog JSON (SPEC §2.1). A single slog.SetDefault in
// cmd/ocapps/main.go, the packages receive the logger by constructor and the
// modules add the "module" attribute with Module.
package log

import (
	"log/slog"
	"os"
)

// New returns a JSON logger to stdout with the requested level
// (debug|info|warn|error). An unknown level falls back to info - the strict
// validation of the level is the responsibility of common/config.
func New(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

// Module returns the logger with the "module"=<name> attribute so that every
// log line identifies the module that emits it (news|notes|photos|...).
func Module(log *slog.Logger, name string) *slog.Logger {
	return log.With("module", name)
}
