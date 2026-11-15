package live

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPublishSoloAlOwner: los eventos no se cruzan entre usuarios (multi-tenant).
func TestPublishSoloAlOwner(t *testing.T) {
	h := NewHub()
	chA, unsubA, ok := h.Subscribe("owner-a")
	if !ok {
		t.Fatal("subscribe owner-a")
	}
	defer unsubA()
	chB, unsubB, ok := h.Subscribe("owner-b")
	if !ok {
		t.Fatal("subscribe owner-b")
	}
	defer unsubB()

	h.Publish("owner-a", Event{Topic: "photos", Client: "c1"})

	select {
	case ev := <-chA:
		if ev.Topic != "photos" || ev.Client != "c1" {
			t.Fatalf("evento inesperado: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("owner-a no recibió su evento")
	}
	select {
	case ev := <-chB:
		t.Fatalf("owner-b no debería recibir eventos de owner-a: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestPublishSinOwner: sin owner o sin topic no se publica nada (no hay
// streams "globales" que puedan filtrar cambios de otros).
func TestPublishSinOwner(t *testing.T) {
	h := NewHub()
	ch, unsub, _ := h.Subscribe("")
	defer unsub()
	h.Publish("", Event{Topic: "photos"})
	h.Publish("owner-a", Event{})
	select {
	case ev := <-ch:
		t.Fatalf("evento inesperado: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestServeCabecerasYEventos: el stream sale con las cabeceras que necesitan
// los proxies, manda el evento publicado y termina con Close.
func TestServeCabecerasYEventos(t *testing.T) {
	h := NewHub()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.Serve(w, r, "owner-a")
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if v := resp.Header.Get("X-Accel-Buffering"); v != "no" {
		t.Fatalf("X-Accel-Buffering = %q", v)
	}
	if v := resp.Header.Get("Cache-Control"); v != "no-cache" {
		t.Fatalf("Cache-Control = %q", v)
	}

	// Espera a que el handler esté suscrito antes de publicar.
	deadline := time.Now().Add(2 * time.Second)
	for h.Subscribers("owner-a") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	lines := make(chan string, 16)
	go func() {
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- strings.TrimRight(line, "\r\n")
		}
	}()

	var prelude []string
	gotRetry, gotConnected := false, false
	for !gotRetry || !gotConnected {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream cerrado antes del preámbulo")
			}
			prelude = append(prelude, line)
			if strings.HasPrefix(line, "retry:") {
				gotRetry = true
			}
			if line == ": connected" {
				gotConnected = true
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("preámbulo incompleto: %q", prelude)
		}
	}

	h.Publish("owner-a", Event{Topic: "photos", Client: "tab-1"})
	var data string
	for data == "" {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream cerrado sin evento")
			}
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no llegó el evento")
		}
	}
	var ev Event
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Fatalf("evento no es JSON: %q", data)
	}
	if ev.Topic != "photos" || ev.Client != "tab-1" {
		t.Fatalf("evento inesperado: %+v", ev)
	}

	// Close termina el stream (si no, Shutdown agotaría su presupuesto).
	h.Close()
	for {
		select {
		case _, ok := <-lines:
			if !ok {
				return // EOF: el stream terminó
			}
		case <-time.After(2 * time.Second):
			t.Fatal("el stream no terminó tras Close")
		}
	}
}

// TestServeRequiereOwner: sin owner no se abre un stream (todos los handlers
// están detrás de auth, pero el hub no se fía).
func TestServeRequiereOwner(t *testing.T) {
	h := NewHub()
	rr := httptest.NewRecorder()
	h.Serve(rr, httptest.NewRequest(http.MethodGet, "/stream", nil), "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, esperado 401", rr.Code)
	}
}

// TestSubscribeTrasClose: el hub cerrado no acepta suscriptores nuevos.
func TestSubscribeTrasClose(t *testing.T) {
	h := NewHub()
	h.Close()
	if _, _, ok := h.Subscribe("owner-a"); ok {
		t.Fatal("Subscribe devolvió ok con el hub cerrado")
	}
	rr := httptest.NewRecorder()
	h.Serve(rr, httptest.NewRequest(http.MethodGet, "/stream", nil), "owner-a")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, esperado 503", rr.Code)
	}
}

// TestSubscribeCierraCanal: al desuscribirse el canal se cierra y deja de
// contar; desuscribirse dos veces no entra en pánico.
func TestSubscribeCierraCanal(t *testing.T) {
	h := NewHub()
	ch, unsubscribe, _ := h.Subscribe("owner-a")
	if n := h.Subscribers("owner-a"); n != 1 {
		t.Fatalf("Subscribers = %d", n)
	}
	unsubscribe()
	unsubscribe()
	if _, ok := <-ch; ok {
		t.Fatal("el canal debería estar cerrado")
	}
	if n := h.Subscribers("owner-a"); n != 0 {
		t.Fatalf("Subscribers tras cerrar = %d", n)
	}
}

// TestClientID: el id de cliente se acota y se ignora si es demasiado largo.
func TestClientID(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := ClientID(r); got != "" {
		t.Fatalf("sin cabecera = %q", got)
	}
	r.Header.Set(clientHeader, "tab-1")
	if got := ClientID(r); got != "tab-1" {
		t.Fatalf("ClientID = %q", got)
	}
	r.Header.Set(clientHeader, strings.Repeat("x", maxClientIDLen+1))
	if got := ClientID(r); got != "" {
		t.Fatalf("id largo debería descartarse, got %q", got)
	}
}
