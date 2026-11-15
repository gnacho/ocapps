package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNotify(t *testing.T) {
	var got struct {
		Topic   string `json:"topic"`
		Title   string `json:"title"`
		Message string `json:"message"`
		Tags    string `json:"tags"`
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decodificar body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := NewAllowLocal(ts.URL, "", log)
	if err := n.Notify(context.Background(), "mytopic", "Title", "Message"); err != nil {
		t.Fatal(err)
	}
	if got.Topic != "mytopic" || got.Title != "Title" || got.Message != "Message" {
		t.Errorf("wrong payload: %+v", got)
	}
	if got.Tags == "" {
		t.Error("no tags in the payload")
	}
}

func TestNotifyHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := NewAllowLocal(ts.URL, "", log)
	if err := n.Notify(context.Background(), "t", "x", "y"); err == nil {
		t.Error("HTTP 500 should return an error")
	}
}

func TestNotifyEmptyTopic(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := NewAllowLocal("http://127.0.0.1:9", "", log)
	if err := n.Notify(context.Background(), "", "x", "y"); err == nil {
		t.Error("empty topic should fail without touching the network")
	}
}
