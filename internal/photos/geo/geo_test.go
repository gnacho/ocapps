package geo

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/photos/store"
)

// TestNameConcurrentThrottle exercises the historical data race on
// Geocoder.last (quick win Q2): two concurrent Name() calls on coordinates
// that are not cached must serialise the throttle (1 req/s) with no race (-race
// validates it) and honour the 1 req/s against the endpoint.
func TestNameConcurrentThrottle(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"name":"Madrid","display_name":"Madrid, España"}`)
	}))
	defer srv.Close()

	g := New(st, slog.Default())
	g.baseURL = srv.URL

	start := time.Now()
	var wg sync.WaitGroup
	results := make([]string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// different coords so that none comes from the cache
			name, err := g.Name(context.Background(), 40.4168+float64(i)*0.01, -3.7038, 2)
			if err != nil {
				t.Errorf("Name: %v", err)
				return
			}
			results[i] = name
		}(i)
	}
	wg.Wait()

	if calls.Load() != 2 {
		t.Fatalf("endpoint calls = %d, want 2", calls.Load())
	}
	for i, n := range results {
		if n != "Madrid" {
			t.Fatalf("results[%d] = %q, want Madrid", i, n)
		}
	}
	// the global throttle is ~1.1 s: two serialised requests take >= 1 s
	if d := time.Since(start); d < time.Second {
		t.Fatalf("throttle not honoured under concurrency: %s", d)
	}
}
