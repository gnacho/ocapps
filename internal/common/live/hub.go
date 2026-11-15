// Package live: canal de avisos de cambio por usuario (Server-Sent Events),
// compartido por los módulos de ocapps.
//
// El cliente mantiene UNA petición abierta contra el stream; cuando algo
// cambia, el servidor manda un mensaje pequeño con el topic que ha cambiado y
// el cliente recarga ESE dato con la API normal (no se envían datos por el
// stream, solo invalidaciones). Ventajas frente a que el cliente pregunte:
// latencia de milisegundos en vez de "cada N segundos", cero peticiones cuando
// no pasa nada, y el coste del escaneo no se multiplica por pestañas abiertas.
//
// Es multi-tenant: cada suscriptor está atado a un owner (oc_id) y solo recibe
// los eventos de SU owner. Un hub es del proceso, no de un request.
package live

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// clientHeader lo manda el cliente en cada petición de escritura, para que el
// evento diga qué cliente hizo el cambio y ese cliente no recargue lo que ya
// tiene. Es opcional: sin él, todos recargan.
const clientHeader = "X-Ocapps-Client"

// maxClientIDLen acota el id de cliente: se refleja en el stream y no queremos
// reenviar cadenas arbitrariamente largas.
const maxClientIDLen = 64

// heartbeatEvery mantiene viva la conexión a través de proxies que cierran
// streams ociosos (nginx proxy_read_timeout por defecto: 60 s).
const heartbeatEvery = 25 * time.Second

// Event es un aviso de cambio: qué cambió y quién lo cambió (vacío si no se
// sabe o si lo cambió el servidor, p. ej. un scan de fondo).
type Event struct {
	Topic  string `json:"topic"`
	Client string `json:"client,omitempty"`
}

// Hub reparte eventos entre los streams abiertos.
type Hub struct {
	mu     sync.Mutex
	subs   map[string]map[chan Event]struct{}
	closed bool
	done   chan struct{}
}

// NewHub construye un hub sin suscriptores.
func NewHub() *Hub {
	return &Hub{subs: map[string]map[chan Event]struct{}{}, done: make(chan struct{})}
}

// ClientID lee el id de cliente de la petición ("" si falta o es demasiado
// largo).
func ClientID(r *http.Request) string {
	id := r.Header.Get(clientHeader)
	if len(id) > maxClientIDLen {
		return ""
	}
	return id
}

// Subscribe abre un canal para el owner. Devuelve el canal, la función para
// cerrarlo (idempotente) y false si el hub ya está cerrado.
//
// Subscribe existe para el propio stream y para los tests; los módulos que
// quieran reaccionar a eventos en proceso pueden usarlo igualmente.
func (h *Hub) Subscribe(owner string) (<-chan Event, func(), bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, func() {}, false
	}
	ch := make(chan Event, 16)
	if h.subs[owner] == nil {
		h.subs[owner] = map[chan Event]struct{}{}
	}
	h.subs[owner][ch] = struct{}{}
	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(h.subs[owner], ch)
			if len(h.subs[owner]) == 0 {
				delete(h.subs, owner)
			}
			close(ch)
		})
	}
	return ch, unsubscribe, true
}

// Publish manda el evento a los streams de ESE owner. Un suscriptor que no
// lee a tiempo pierde el mensaje (y recibirá el siguiente): preferimos perder
// una invalidación a bloquear una escritura. Sin owner no se publica nada.
func (h *Hub) Publish(owner string, ev Event) {
	if owner == "" || ev.Topic == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[owner] {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribers devuelve cuántos streams tiene abiertos el owner (métricas y
// tests).
func (h *Hub) Subscribers(owner string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[owner])
}

// Close termina todos los streams (apagado del proceso). Sin esto,
// http.Server.Shutdown espera a los handlers en curso hasta agotar su
// presupuesto, porque un stream no termina nunca por sí solo.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	close(h.done)
}

// Serve atiende el stream SSE del owner. No escribe datos de la aplicación:
// solo eventos de invalidación y pings.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, owner string) {
	if owner == "" {
		http.Error(w, "owner requerido", http.StatusUnauthorized)
		return
	}
	ch, unsubscribe, ok := h.Subscribe(owner)
	if !ok {
		http.Error(w, "servidor apagándose", http.StatusServiceUnavailable)
		return
	}
	defer unsubscribe()

	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no") // nginx: no acumular el stream
	w.WriteHeader(http.StatusOK)
	// retry: si el navegador reconecta por su cuenta, que espere 3 s.
	if _, err := w.Write([]byte("retry: 3000\n: connected\n\n")); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}

	beat := time.NewTicker(heartbeatEvery)
	defer beat.Stop()
	for {
		var msg []byte
		select {
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		case <-beat.C:
			msg = []byte(": ping\n\n")
		case ev := <-ch:
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			msg = append(append([]byte("data: "), b...), '\n', '\n')
		}
		if _, err := w.Write(msg); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}
