// Package module defines the unified Module interface of SPEC §4.5 that
// cmd/ocapps consumes in the wiring (H5): every module (news, notes, photos)
// registers its routes under its namespace in the shared mux, runs its
// background loops in Run and reports its health to /healthz and /readyz.
//
// Location decision (H5): the interface lives in this mini-package and NOT in
// common/httpx (the other option of the SPEC) because httpx is a collection of
// HTTP request/response helpers (CORS, WriteJSON, DecodeBody) whereas Module
// is a process lifecycle contract (Run with context, Healthy). Keeping them
// apart prevents httpx from accumulating unrelated responsibilities and allows
// hanging the wiring helpers (Unavailable) here without dragging "context"
// into the HTTP helpers package.
package module

import (
	"context"
	"net/http"

	"github.com/gnacho/ocapps/internal/common/httpx"
)

// Module is the module interface of SPEC §4.5.
type Module interface {
	Name() string
	Enabled() bool
	// Register mounts the module routes in the mux (or the 503 handler if failed).
	Register(mux *http.ServeMux)
	// Run executes the background loops, it must return when ctx is cancelled.
	Run(ctx context.Context) error
	// Healthy reports to /healthz and /readyz.
	Healthy() error
}

// Unavailable returns the substitute handler of a failed module (failure
// policy D3, SPEC §4.5): 503 with {"error":"module <name> unavailable"} in its
// whole namespace. The process keeps serving the rest of the modules.
func Unavailable(name string) http.Handler {
	body := map[string]any{"error": "module " + name + " unavailable"}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, body)
	})
}
