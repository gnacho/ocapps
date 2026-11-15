package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/live"
)

const testToken = "Bearer test-token"

// TestStreamSinAuth: el stream no es una excepción a la autenticación.
func TestStreamSinAuth(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetHub(live.NewHub())
	rr := do(t, s.Handler(), http.MethodGet, PublicPrefix+"/api/stream", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, esperado 401", rr.Code)
	}
}

// TestStreamSinHub: sin hub montado (módulo degradado, tests) la ruta existe
// pero no hay nada que servir.
func TestStreamSinHub(t *testing.T) {
	s, _, _ := newTestServer(t)
	rr := do(t, s.Handler(), http.MethodGet, PublicPrefix+"/api/stream", testToken)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, esperado 503", rr.Code)
	}
}

// TestMutacionPublicaAlOwner: una mutación correcta avisa al stream de SU owner
// y lleva el id del cliente que la hizo (para que esa pestaña no recargue).
func TestMutacionPublicaAlOwner(t *testing.T) {
	s, _, _ := newTestServer(t)
	hub := live.NewHub()
	s.SetHub(hub)

	ch, unsubscribe, ok := hub.Subscribe("legacy-owner") // owner del token estático del test
	if !ok {
		t.Fatal("subscribe")
	}
	defer unsubscribe()

	req := httptest.NewRequest(http.MethodPost, PublicPrefix+"/api/albums", strings.NewReader(`{"name":"viaje"}`))
	req.Header.Set("Authorization", testToken)
	req.Header.Set("X-Ocapps-Client", "tab-1")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}

	select {
	case ev := <-ch:
		if ev.Topic != TopicAlbums {
			t.Fatalf("topic = %q, esperado %q", ev.Topic, TopicAlbums)
		}
		if ev.Client != "tab-1" {
			t.Fatalf("client = %q, esperado tab-1", ev.Client)
		}
	case <-time.After(time.Second):
		t.Fatal("no llegó el evento del álbum creado")
	}
}

// TestMutacionFallidaNoPublica: si la operación no se aplica, no se avisa.
func TestMutacionFallidaNoPublica(t *testing.T) {
	s, _, _ := newTestServer(t)
	hub := live.NewHub()
	s.SetHub(hub)

	ch, unsubscribe, _ := hub.Subscribe("legacy-owner")
	defer unsubscribe()

	// Asset inexistente: SetFavorite devuelve not found y no se publica nada.
	req := httptest.NewRequest(http.MethodPost, PublicPrefix+"/api/assets/999/favorite", strings.NewReader(`{"favorite":true}`))
	req.Header.Set("Authorization", testToken)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code == http.StatusNoContent {
		t.Fatalf("un favorito sobre un asset inexistente no debería ser 204")
	}

	select {
	case ev := <-ch:
		t.Fatalf("evento inesperado: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}
