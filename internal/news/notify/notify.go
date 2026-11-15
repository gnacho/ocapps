// Package notify: push notifications through ntfy (https://ntfy.sh or a
// self-hosted server). The topic can be global (env) or per user (user_settings).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/gnacho/ocapps/internal/common/netguard"
)

type Notifier struct {
	baseURL string
	topic   string // global topic (empty = per-user only)
	client  *http.Client
	log     *slog.Logger
}

// New creates a Notifier. topic may be empty (notifications only with a
// per-user topic).
func New(baseURL, topic string, log *slog.Logger) *Notifier {
	if baseURL == "" {
		baseURL = "https://ntfy.sh"
	}
	return &Notifier{
		baseURL: baseURL,
		topic:   topic,
		client:  netguard.Client(10 * time.Second),
		log:     log,
	}
}

// NewAllowLocal is like New but allows loopback. TESTS only (httptest).
func NewAllowLocal(baseURL, topic string, log *slog.Logger) *Notifier {
	if baseURL == "" {
		baseURL = "https://ntfy.sh"
	}
	return &Notifier{
		baseURL: baseURL,
		topic:   topic,
		client:  netguard.ClientAllowLocal(10 * time.Second),
		log:     log,
	}
}

// Topic returns the configured global topic ("" if none).
func (n *Notifier) Topic() string { return n.topic }

// Notify publishes a message on the given topic (ntfy POST JSON).
func (n *Notifier) Notify(ctx context.Context, topic, title, message string) error {
	if topic == "" {
		return fmt.Errorf("empty topic")
	}
	body, err := json.Marshal(map[string]string{
		"topic":   topic,
		"title":   title,
		"message": message,
		"tags":    "inbox_tray",
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		n.baseURL+"/"+url.PathEscape(topic), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("enviar a ntfy: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("ntfy: HTTP %d", resp.StatusCode)
	}
	return nil
}
