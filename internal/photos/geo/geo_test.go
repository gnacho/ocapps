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

// TestNameConcurrentThrottle ejerce el data race histórico de Geocoder.last
// (quick-win Q2): dos Name() concurrentes sobre coordenadas no cacheadas
// deben serializar el throttle (1 req/s) sin carrera (lo valida -race) y
// respetar el 1 req/s contra el endpoint.
func TestNameConcurrentThrottle(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"Madrid","display_name":"Madrid, España"}`)
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
			// coords distintas para que ninguna venga de caché
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
		t.Fatalf("llamadas al endpoint = %d, quiero 2", calls.Load())
	}
	for i, n := range results {
		if n != "Madrid" {
			t.Fatalf("results[%d] = %q, quiero Madrid", i, n)
		}
	}
	// el throttle global es ~1.1 s: dos peticiones serializadas tardan >= 1 s
	if d := time.Since(start); d < time.Second {
		t.Fatalf("throttle no respetado con concurrencia: %s", d)
	}
}
