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

// TestPublishOnlyToOwner: events do not cross between users (multi-tenant).
func TestPublishOnlyToOwner(t *testing.T) {
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
			t.Fatalf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("owner-a did not receive its event")
	}
	select {
	case ev := <-chB:
		t.Fatalf("owner-b must not receive events from owner-a: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestPublishWithoutOwner: without an owner or without a topic nothing is
// published (there are no "global" streams that could leak changes of others).
func TestPublishWithoutOwner(t *testing.T) {
	h := NewHub()
	ch, unsub, _ := h.Subscribe("")
	defer unsub()
	h.Publish("", Event{Topic: "photos"})
	h.Publish("owner-a", Event{})
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestServeCabecerasYEventos: the stream goes out with the headers the proxies
// need, sends the published event and ends with Close.
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

	// Wait for the handler to be subscribed before publishing.
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
				t.Fatal("stream closed before the prelude")
			}
			prelude = append(prelude, line)
			if strings.HasPrefix(line, "retry:") {
				gotRetry = true
			}
			if line == ": connected" {
				gotConnected = true
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("incomplete prelude: %q", prelude)
		}
	}

	h.Publish("owner-a", Event{Topic: "photos", Client: "tab-1"})
	var data string
	for data == "" {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed without an event")
			}
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the event did not arrive")
		}
	}
	var ev Event
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Fatalf("event is not JSON: %q", data)
	}
	if ev.Topic != "photos" || ev.Client != "tab-1" {
		t.Fatalf("unexpected event: %+v", ev)
	}

	// Close ends the stream (otherwise Shutdown would exhaust its budget).
	h.Close()
	for {
		select {
		case _, ok := <-lines:
			if !ok {
				return // EOF: the stream ended
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the stream did not end after Close")
		}
	}
}

// TestServeRequiereOwner: without an owner no stream is opened (all handlers
// are behind auth, but the hub does not trust that).
func TestServeRequiereOwner(t *testing.T) {
	h := NewHub()
	rr := httptest.NewRecorder()
	h.Serve(rr, httptest.NewRequest(http.MethodGet, "/stream", nil), "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

// TestSubscribeTrasClose: a closed hub does not accept new subscribers.
func TestSubscribeTrasClose(t *testing.T) {
	h := NewHub()
	h.Close()
	if _, _, ok := h.Subscribe("owner-a"); ok {
		t.Fatal("Subscribe returned ok with the hub closed")
	}
	rr := httptest.NewRecorder()
	h.Serve(rr, httptest.NewRequest(http.MethodGet, "/stream", nil), "owner-a")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

// TestSubscribeCierraCanal: on unsubscribe the channel is closed and stops
// counting, unsubscribing twice does not panic.
func TestSubscribeCierraCanal(t *testing.T) {
	h := NewHub()
	ch, unsubscribe, _ := h.Subscribe("owner-a")
	if n := h.Subscribers("owner-a"); n != 1 {
		t.Fatalf("Subscribers = %d", n)
	}
	unsubscribe()
	unsubscribe()
	if _, ok := <-ch; ok {
		t.Fatal("the channel should be closed")
	}
	if n := h.Subscribers("owner-a"); n != 0 {
		t.Fatalf("Subscribers after closing = %d", n)
	}
}

// TestClientID: the client id is capped and ignored if it is too long.
func TestClientID(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if got := ClientID(r); got != "" {
		t.Fatalf("no header = %q", got)
	}
	r.Header.Set(clientHeader, "tab-1")
	if got := ClientID(r); got != "tab-1" {
		t.Fatalf("ClientID = %q", got)
	}
	r.Header.Set(clientHeader, strings.Repeat("x", maxClientIDLen+1))
	if got := ClientID(r); got != "" {
		t.Fatalf("a long id should be discarded, got %q", got)
	}
}
