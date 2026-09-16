// Package log: slog JSON unificado (SPEC §2.1). Un solo slog.SetDefault en
// cmd/ocapps/main.go; los paquetes reciben el logger por constructor y los
// módulos añaden el atributo "module" con Module.
package log

import (
	"log/slog"
	"os"
)

// New devuelve un logger JSON a stdout con el nivel pedido
// (debug|info|warn|error). Un nivel desconocido cae a info — la validación
// estricta del nivel es responsabilidad de common/config.
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

// Module devuelve el logger con el atributo "module"=<name> para que cada
// línea de log identifique el módulo que la emite (news|notes|photos|...).
func Module(log *slog.Logger, name string) *slog.Logger {
	return log.With("module", name)
}
