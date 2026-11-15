// Package websub: WebSub (PubSubHubbub) client to receive items from feeds
// published by hubs (WordPress, Blogger, Medium...) in real time.
// Subscription: form-urlencoded POST to the hub with hub.mode/hub.topic/hub.callback.
package websub

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gnacho/ocapps/internal/common/netguard"
)

type Client struct {
	http *http.Client
}

func New() *Client {
	return &Client{http: netguard.Client(15 * time.Second)}
}

// NewAllowLocal is like New but allows loopback. TESTS only.
func NewAllowLocal() *Client {
	return &Client{http: netguard.ClientAllowLocal(15 * time.Second)}
}

// Subscribe asks the hub to subscribe to (or renew) a topic.
// secret allows signing the deliveries (X-Hub-Signature).
func (c *Client) Subscribe(ctx context.Context, hub, topic, callback, secret string) error {
	u, err := url.Parse(hub)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("invalid hub: %q", hub)
	}
	form := url.Values{}
	form.Set("hub.mode", "subscribe")
	form.Set("hub.topic", topic)
	form.Set("hub.callback", callback)
	if secret != "" {
		form.Set("hub.secret", secret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hub, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("subscribe to hub %s: %w", hub, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("hub %s responded HTTP %d", hub, resp.StatusCode)
	}
	return nil
}
