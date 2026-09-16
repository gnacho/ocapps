// Package module define la interfaz Module unificada del SPEC §4.5 que
// cmd/ocapps consume en el wiring (H5): cada módulo (news, notes, photos)
// registra sus rutas bajo su namespace en el mux compartido, ejecuta sus
// loops de background en Run e informa de su salud a /healthz y /readyz.
//
// Decisión de ubicación (H5): la interfaz vive en este mini-paquete y NO en
// common/httpx (la otra opción del SPEC) porque httpx es una colección de
// helpers de petición/respuesta HTTP (CORS, WriteJSON, DecodeBody) mientras
// que Module es un contrato de ciclo de vida del proceso (Run con context,
// Healthy); separarlos evita que httpx acumule responsabilidades ajenas y
// permite colgar aquí los helpers de wiring (Unavailable) sin arrastrar
// "context" al paquete de helpers HTTP.
package module

import (
	"context"
	"net/http"

	"github.com/gnacho/ocapps/internal/common/httpx"
)

// Module es la interfaz de módulo del SPEC §4.5.
type Module interface {
	Name() string
	Enabled() bool
	// Register monta las rutas del módulo en el mux (o el handler 503 si failed).
	Register(mux *http.ServeMux)
	// Run ejecuta loops de background; debe retornar al cancelar ctx.
	Run(ctx context.Context) error
	// Healthy informa a /healthz y /readyz.
	Healthy() error
}

// Unavailable devuelve el handler sustituto de un módulo failed (política de
// fallos D3, SPEC §4.5): 503 con {"error":"module <name> unavailable"} en
// todo su namespace. El proceso sigue sirviendo el resto de módulos.
func Unavailable(name string) http.Handler {
	body := map[string]any{"error": "module " + name + " unavailable"}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteJSON(w, http.StatusServiceUnavailable, body)
	})
}
