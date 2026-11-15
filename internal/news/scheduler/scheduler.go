// Package scheduler: daemon that refreshes the due feeds and runs the
// nightly retention. Each cycle: feeds with next_update <= now, with
// bounded concurrency, the interval of each feed is decided by the refresher
// (adaptive by novelty + backoff on error, with jitter).
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

// feedRefresher is the part of refresher.Refresher that the scheduler uses,
// an interface to be able to inject doubles in tests (I3: one that panics).
type feedRefresher interface {
	Refresh(ctx context.Context, f *store.Feed) refresher.Result
}

type Scheduler struct {
	store       *store.Store
	refresher   feedRefresher
	favicons    *favicon.Cache
	log         *slog.Logger
	tick        time.Duration // check period
	concurrency int           // feeds in parallel
	retention   time.Duration // retention of read items, 0 = infinite
	websub      *websub.Client
	pubBase     string // public URL of the backend for WebSub callbacks (#44)

	cycleHook func() // tests only (I3): called at the start of each cycle
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

// guarded runs fn recovering any panic (I3): a panic in an internal
// scheduler goroutine is logged with a stack and does NOT take down the
// process, the loop stays alive.
func (s *Scheduler) guarded(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("panic recovered in a scheduler goroutine; the loop continues",
				"goroutine", name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// Run blocks until ctx is cancelled (returns nil on a normal shutdown).
// Every internal goroutine is a child of ctx, it is protected with recover (I3) and
// it is drained with the WaitGroup before returning. A panic in the MAIN loop
// is recovered and returned as an error: the wiring marks the module failed
// (D3) without taking down the process.
func (s *Scheduler) Run(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("panic in the scheduler main loop; module failed",
				"panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic in the scheduler main loop: %v", r)
		}
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	// retention: on start and every 24h
	retentionCtx, retentionCancel := context.WithCancel(ctx)
	defer retentionCancel()
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.guarded("retention", func() { s.runRetention(retentionCtx) })
	}()

	// WebSub: subscriptions and lease renewals (#44)
	if s.websub != nil && s.pubBase != "" {
		wsCtx, wsCancel := context.WithCancel(ctx)
		defer wsCancel()
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.guarded("websub", func() { s.runWebSub(wsCtx) })
		}()
	}

	s.cycle(ctx) // immediate sweep on start
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

// cycle refreshes the due feeds with bounded concurrency.
func (s *Scheduler) cycle(ctx context.Context) {
	if s.cycleHook != nil { // tests only (I3)
		s.cycleHook()
	}
	due, err := s.store.ListDueFeeds(time.Now().Unix(), 50)
	if err != nil {
		s.log.Error("list due feeds", "err", err)
		return
	}
	if len(due) == 0 {
		return
	}
	s.log.Debug("refresh cycle", "feeds", len(due))

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
				// WebSub: record the detected hub to subscribe in the loop
				if res.Err == nil && res.Hub != "" {
					if err := s.store.UpsertWebSub(f.ID, res.Hub, f.URL); err != nil {
						s.log.Warn("websub: record hub", "feed", f.ID, "err", err)
					}
				}
				// best-effort favicon: only when the feed brings novelty and it is not
				// cached (a site without favicon is not retried every cycle)
				if s.favicons != nil && res.Inserted > 0 && !s.favicons.Has(favicon.Hash(f.URL)) {
					s.favicons.Fetch(ctx, f.URL, f.Link)
				}
			})
		}()
	}
	wg.Wait()
}

// runRetention purges read non-starred items. The global one (s.retention) applies
// to all feeds, but those with their own override (retention_days>0) are
// purged before (shorter overrides) and EXCLUDED from the global cutoff, a feed
// with a retention override is purged ONLY with its own rule. 0 = disabled.
func (s *Scheduler) runRetention(ctx context.Context) {
	if s.retention <= 0 {
		return
	}
	purge := func() {
		now := time.Now()
		// feeds with their own override
		over, err := s.store.FeedsWithRetentionOverride()
		if err != nil {
			s.log.Error("per-feed retention: list overrides", "err", err)
		} else {
			for _, f := range over {
				cutoff := now.Add(-time.Duration(f.Days) * 24 * time.Hour).Unix()
				n, err := s.store.PurgeOldItemsByFeed(f.ID, cutoff)
				if err != nil {
					s.log.Error("per-feed retention", "feed", f.ID, "err", err)
					continue
				}
				if n > 0 {
					s.log.Info("per-feed retention", "feed", f.ID, "days", f.Days, "deleted", n)
				}
			}
		}
		// feeds without override: global cutoff
		cutoff := now.Add(-s.retention).Unix()
		n, err := s.store.PurgeOldItems(cutoff)
		if err != nil {
			s.log.Error("retention failed", "err", err)
			return
		}
		if n > 0 {
			s.log.Info("retention", "items deleted", n)
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

// runWebSub subscribes and renews the subscriptions of the feeds with a hub.
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

// websubCycle re-subscribes the subscriptions without a lease or about to expire.
func (s *Scheduler) websubCycle(ctx context.Context) {
	due, err := s.store.WebSubDue(time.Now().Unix(), time.Hour)
	if err != nil {
		s.log.Error("websub: list pending subscriptions", "err", err)
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
			s.log.Warn("websub: store secret", "feed", sub.FeedID, "err", err)
			continue
		}
		if err := s.websub.Subscribe(ctx, sub.Hub, sub.Topic, callback, secret); err != nil {
			s.log.Warn("websub: subscribe failed", "feed", sub.FeedID, "hub", sub.Hub, "err", err)
			_ = s.store.SaveWebSubStatus(sub.FeedID, "error")
			continue
		}
		s.log.Info("websub: subscription sent", "feed", sub.FeedID, "hub", sub.Hub)
		_ = s.store.SaveWebSubStatus(sub.FeedID, "pending")
		// provisional lease while the hub verifies, the verification adjusts it
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
