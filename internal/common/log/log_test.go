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
		"weird": false, // unknown level -> info
	} {
		log := New(level)
		if got := log.Enabled(context.Background(), slog.LevelDebug); got != debugVisible {
			t.Errorf("level %q: debug enabled = %v, want %v", level, got, debugVisible)
		}
	}
}

func TestModuleAddsAttribute(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	Module(base, "news").Info("hello")
	out := buf.String()
	if !strings.Contains(out, `"module":"news"`) {
		t.Fatalf("missing module attr: %s", out)
	}
	if !strings.Contains(out, `"msg":"hello"`) {
		t.Fatalf("output is not slog JSON: %s", out)
	}
}
