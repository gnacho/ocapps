// Package scheduler: daemon que refresa los feeds vencidos y ejecuta la
// retención nocturna. Cada ciclo: feeds con next_update <= ahora, con
// concurrencia acotada; el intervalo de cada feed lo decide el refresher
// (adaptativo por novedad + backoff por error, con jitter).
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/gnacho/ocapps/internal/news/favicon"
	"github.com/gnacho/ocapps/internal/news/refresher"
	"github.com/gnacho/ocapps/internal/news/store"
	"github.com/gnacho/ocapps/internal/news/websub"
)

// feedRefresher es la parte de refresher.Refresher que usa el scheduler;
// interfaz para poder inyectar dobles en tests (I3: uno que panique).
type feedRefresher interface {
	Refresh(ctx context.Context, f *store.Feed) refresher.Result
}

type Scheduler struct {
	store       *store.Store
	refresher   feedRefresher
	favicons    *favicon.Cache
	log         *slog.Logger
	tick        time.Duration // periodo de comprobación
	concurrency int           // feeds en paralelo
	retention   time.Duration // retención de items leídos; 0 = infinita
	websub      *websub.Client
	pubBase     string // URL pública del backend para callbacks WebSub (#44)

	cycleHook func() // solo tests (I3): se invoca al inicio de cada cycle
}

func New(st *store.Store, r feedRefresher, fc *favicon.Cache, log *slog.Logger,
	tick time.Duration, concurrency int, retention time.Duration, ws *websub.Client, pubBase string) *Scheduler {
	if concurrency < 1 {
		concurrency = 4
	}
	if tick <= 0 {
		tick = 30 * time.Second
	}
	return &Scheduler{store: st, refresher: r, favicons: fc, log: log,
		tick: tick, concurrency: concurrency, retention: retention, websub: ws, pubBase: pubBase}
}

// guarded ejecuta fn recuperando cualquier pánico (I3): un pánico en una
// goroutine interna del scheduler se loguea con stack y NO tumba el
// proceso — el loop sigue vivo.
func (s *Scheduler) guarded(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("pánico recuperado en goroutine del scheduler; el loop continúa",
				"goroutine", name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// Run bloquea hasta que ctx se cancela (devuelve nil en apagado normal).
// Toda goroutine interna es hija del ctx, va protegida con recover (I3) y
// se drena con el WaitGroup antes de volver. Un pánico en el loop PRINCIPAL
// se recupera y se devuelve como error: el wiring marca el módulo failed
// (D3) sin tumbar el proceso.
func (s *Scheduler) Run(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("pánico en el loop principal del scheduler; módulo failed",
				"panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("pánico en el loop principal del scheduler: %v", r)
		}
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	// retención: al arrancar y cada 24h
	retentionCtx, retentionCancel := context.WithCancel(ctx)
	defer retentionCancel()
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.guarded("retention", func() { s.runRetention(retentionCtx) })
	}()

	// WebSub: suscripciones y renovaciones de lease (#44)
	if s.websub != nil && s.pubBase != "" {
		wsCtx, wsCancel := context.WithCancel(ctx)
		defer wsCancel()
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.guarded("websub", func() { s.runWebSub(wsCtx) })
		}()
	}

	s.cycle(ctx) // barrido inmediato al arrancar
	for {
		select {
		case <-ctx.Done():
			s.log.Info("scheduler detenido")
			return nil
		case <-ticker.C:
			s.cycle(ctx)
		}
	}
}

// cycle refresca los feeds vencidos con concurrencia acotada.
func (s *Scheduler) cycle(ctx context.Context) {
	if s.cycleHook != nil { // solo tests (I3)
		s.cycleHook()
	}
	due, err := s.store.ListDueFeeds(time.Now().Unix(), 50)
	if err != nil {
		s.log.Error("listar feeds vencidos", "err", err)
		return
	}
	if len(due) == 0 {
		return
	}
	s.log.Debug("ciclo de refresco", "feeds", len(due))

	sem := make(chan struct{}, s.concurrency)
	var wg sync.WaitGroup
	for i := range due {
		f := due[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.guarded("worker", func() {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					return
				}
				res := s.refresher.Refresh(ctx, &f)
				// WebSub: registrar el hub detectado para suscribirse en el loop
				if res.Err == nil && res.Hub != "" {
					if err := s.store.UpsertWebSub(f.ID, res.Hub, f.URL); err != nil {
						s.log.Warn("websub: registrar hub", "feed", f.ID, "err", err)
					}
				}
				// favicon best-effort: solo cuando el feed trae novedades y no
				// está cacheado (un sitio sin favicon no se reintenta cada ciclo)
				if s.favicons != nil && res.Inserted > 0 && !s.favicons.Has(favicon.Hash(f.URL)) {
					s.favicons.Fetch(ctx, f.URL, f.Link)
				}
			})
		}()
	}
	wg.Wait()
}

// runRetention purga items leídos no destacados. El global (s.retention) aplica
// a todos los feeds, pero los que tienen override propio (retention_days>0) se
// purgan antes (overrides más cortos) y se EXCLUYEN del corte global — un feed
// con override de retención se purga SOLO con su propia regla. 0 = desactivada.
func (s *Scheduler) runRetention(ctx context.Context) {
	if s.retention <= 0 {
		return
	}
	purge := func() {
		now := time.Now()
		// feeds con override propio
		over, err := s.store.FeedsWithRetentionOverride()
		if err != nil {
			s.log.Error("retención por feed: listar overrides", "err", err)
		} else {
			for _, f := range over {
				cutoff := now.Add(-time.Duration(f.Days) * 24 * time.Hour).Unix()
				n, err := s.store.PurgeOldItemsByFeed(f.ID, cutoff)
				if err != nil {
					s.log.Error("retención por feed", "feed", f.ID, "err", err)
					continue
				}
				if n > 0 {
					s.log.Info("retención por feed", "feed", f.ID, "días", f.Days, "borrados", n)
				}
			}
		}
		// feeds sin override: corte global
		cutoff := now.Add(-s.retention).Unix()
		n, err := s.store.PurgeOldItems(cutoff)
		if err != nil {
			s.log.Error("retención falló", "err", err)
			return
		}
		if n > 0 {
			s.log.Info("retención", "items borrados", n)
		}
	}
	purge()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			purge()
		}
	}
}

// runWebSub suscribe y renueva las suscripciones de los feeds con hub.
func (s *Scheduler) runWebSub(ctx context.Context) {
	s.websubCycle(ctx)
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.websubCycle(ctx)
		}
	}
}

// websubCycle re-suscribe las suscripciones sin lease o a punto de expirar.
func (s *Scheduler) websubCycle(ctx context.Context) {
	due, err := s.store.WebSubDue(time.Now().Unix(), time.Hour)
	if err != nil {
		s.log.Error("websub: listar suscripciones pendientes", "err", err)
		return
	}
	for _, sub := range due {
		if ctx.Err() != nil {
			return
		}
		secret := sub.Secret
		if secret == "" {
			secret = randomSecret()
		}
		callback := s.pubBase + "/websub/callback/" + strconv.FormatInt(sub.FeedID, 10)
		if err := s.store.SaveWebSubSecret(sub.FeedID, secret, callback); err != nil {
			s.log.Warn("websub: guardar secret", "feed", sub.FeedID, "err", err)
			continue
		}
		if err := s.websub.Subscribe(ctx, sub.Hub, sub.Topic, callback, secret); err != nil {
			s.log.Warn("websub: suscribir falló", "feed", sub.FeedID, "hub", sub.Hub, "err", err)
			_ = s.store.SaveWebSubStatus(sub.FeedID, "error")
			continue
		}
		s.log.Info("websub: suscripción enviada", "feed", sub.FeedID, "hub", sub.Hub)
		_ = s.store.SaveWebSubStatus(sub.FeedID, "pending")
		// lease provisional mientras el hub verifica; la verificación lo ajusta
		_ = s.store.SaveWebSubLease(sub.FeedID, time.Now().Add(6*time.Hour).Unix())
	}
}

func randomSecret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "ocnews-secret-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b)
}
