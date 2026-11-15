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

// TestStreamWithoutAuth: the stream is not an exception to authentication.
func TestStreamWithoutAuth(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.SetHub(live.NewHub())
	rr := do(t, s.Handler(), http.MethodGet, PublicPrefix+"/api/stream", "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

// TestStreamWithoutHub: without a hub mounted (degraded module, tests) the route
// exists but there is nothing to serve.
func TestStreamWithoutHub(t *testing.T) {
	s, _, _ := newTestServer(t)
	rr := do(t, s.Handler(), http.MethodGet, PublicPrefix+"/api/stream", testToken)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

// TestMutationPublishesToOwner: a successful mutation notifies the stream of ITS
// owner and carries the id of the client that made it (so that tab does not
// reload).
func TestMutationPublishesToOwner(t *testing.T) {
	s, _, _ := newTestServer(t)
	hub := live.NewHub()
	s.SetHub(hub)

	ch, unsubscribe, ok := hub.Subscribe("legacy-owner") // owner of the static token in the test
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
			t.Fatalf("topic = %q, want %q", ev.Topic, TopicAlbums)
		}
		if ev.Client != "tab-1" {
			t.Fatalf("client = %q, want tab-1", ev.Client)
		}
	case <-time.After(time.Second):
		t.Fatal("the event of the created album never arrived")
	}
}

// TestFailedMutationDoesNotPublish: if the operation is not applied, nothing is
// notified.
func TestFailedMutationDoesNotPublish(t *testing.T) {
	s, _, _ := newTestServer(t)
	hub := live.NewHub()
	s.SetHub(hub)

	ch, unsubscribe, _ := hub.Subscribe("legacy-owner")
	defer unsubscribe()

	// Non-existent asset: SetFavorite returns not found and nothing is published.
	req := httptest.NewRequest(http.MethodPost, PublicPrefix+"/api/assets/999/favorite", strings.NewReader(`{"favorite":true}`))
	req.Header.Set("Authorization", testToken)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code == http.StatusNoContent {
		t.Fatalf("a favourite on a non-existent asset should not be 204")
	}

	select {
	case ev := <-ch:
		t.Fatalf("unexpected event: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}
