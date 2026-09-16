// Package geo — geocodificación inversa (Lugares) con caché en SQLite.
// Usa Nominatim/OpenStreetMap: requiere User-Agent propio y max 1 req/s
// (https://operations.osmfoundation.org/policies/nominatim/).
package geo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/gnacho/ocapps/internal/photos/store"
)

// nominatimURL es el endpoint por defecto; se puede sustituir en tests.
const nominatimURL = "https://nominatim.openstreetmap.org"

type Geocoder struct {
	client  *http.Client
	store   *store.Store
	log     *slog.Logger
	baseURL string // endpoint Nominatim (tests: httptest)

	// mu protege last (quick-win Q2: data race del throttle). Se mantiene
	// durante la espera del throttle para que el 1 req/s se cumpla también
	// con peticiones concurrentes.
	mu   sync.Mutex
	last time.Time
}

func New(st *store.Store, log *slog.Logger) *Geocoder {
	return &Geocoder{
		client:  &http.Client{Timeout: 15 * time.Second},
		store:   st,
		log:     log,
		baseURL: nominatimURL,
	}
}

func roundKey(v float64, decimals int) float64 {
	p := math.Pow10(decimals)
	return math.Round(v*p) / p
}

// Name devuelve el nombre del lugar (de caché o consultando Nominatim).
func (g *Geocoder) Name(ctx context.Context, lat, lon float64, decimals int) (string, error) {
	latKey, lonKey := roundKey(lat, decimals), roundKey(lon, decimals)
	if name, ok, err := g.store.GetGeocode(ctx, latKey, lonKey); err == nil && ok {
		return name, nil
	}

	// throttle global 1 req/s (bajo lock, Q2)
	g.mu.Lock()
	if since := time.Since(g.last); since < 1100*time.Millisecond {
		select {
		case <-ctx.Done():
			g.mu.Unlock()
			return "", ctx.Err()
		case <-time.After(1100*time.Millisecond - since):
		}
	}
	g.last = time.Now()
	g.mu.Unlock()

	u := fmt.Sprintf("%s/reverse?format=jsonv2&zoom=14&accept-language=es&lat=%f&lon=%f", g.baseURL, latKey, lonKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ocphotos/0.1 (+https://github.com/gnacho/ocphotos)")

	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("nominatim: %s", resp.Status)
	}

	var out struct {
		Name        string            `json:"name"`
		DisplayName string            `json:"display_name"`
		Address     map[string]string `json:"address"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}

	name := out.Name
	if name == "" {
		for _, k := range []string{"city", "town", "village", "municipality", "county", "state", "country"} {
			if v := out.Address[k]; v != "" {
				name = v
				break
			}
		}
	}
	if name == "" {
		name = out.DisplayName
	}
	if name == "" {
		name = fmt.Sprintf("%.2f, %.2f", latKey, lonKey)
	}
	_ = g.store.SaveGeocode(ctx, latKey, lonKey, name)
	return name, nil
}
