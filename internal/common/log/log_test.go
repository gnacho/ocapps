package log

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestNewRespetaNivel(t *testing.T) {
	for level, debugVisible := range map[string]bool{
		"debug": true,
		"info":  false,
		"warn":  false,
		"error": false,
		"raro":  false, // nivel desconocido → info
	} {
		log := New(level)
		if got := log.Enabled(context.Background(), slog.LevelDebug); got != debugVisible {
			t.Errorf("level %q: debug enabled = %v, want %v", level, got, debugVisible)
		}
	}
}

func TestModuleAñadeAtributo(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	Module(base, "news").Info("hola")
	out := buf.String()
	if !strings.Contains(out, `"module":"news"`) {
		t.Fatalf("falta attr module: %s", out)
	}
	if !strings.Contains(out, `"msg":"hola"`) {
		t.Fatalf("salida no es JSON slog: %s", out)
	}
}
