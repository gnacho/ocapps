package photos

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/gnacho/ocapps/internal/common/config"
	"github.com/gnacho/ocapps/internal/common/live"
	"github.com/gnacho/ocapps/internal/common/webdav"
	"github.com/gnacho/ocapps/internal/photos/exif"
	"github.com/gnacho/ocapps/internal/photos/index"
	"github.com/gnacho/ocapps/internal/photos/store"
)

// scheduler is the multi-owner scan scheduler (H8 §7). It replaces the global
// single-tenant scanLoop:
//   - Every authenticated request (Touch in the registry) marks activity: if
//     the owner is not being scanned and its last scan is older than ScanEvery
//     (or it was never scanned) its scan is enqueued - the index grows with
//     activity.
//   - A ScanEvery ticker enqueues the scans of the users seeded by config
//     (OCAPPS_PHOTOS_USERS) and of the active durable Basic sessions.
//   - The queue dedupes by owner and has a global semaphore of 2 concurrent
//     scans, each scan has a 2h timeout (as before).
type scheduler struct {
	reg    *registry
	st     *store.Store
	log    *slog.Logger
	exifW  *exif.Worker
	root   string
	every  time.Duration
	tmo    time.Duration // timeout per scan (2h)
	sweep  time.Duration // purge period of Bearer sessions >24h
	maxPar chan struct{} // global semaphore of concurrent scans
	hub    *live.Hub     // optional: change notification to connected clients

	mu       sync.Mutex
	queued   map[string]bool
	scanning map[string]bool
	lastScan map[string]time.Time
	queue    chan string
}

func newScheduler(reg *registry, st *store.Store, exifW *exif.Worker, root string, every time.Duration, log *slog.Logger) *scheduler {
	return &scheduler{
		reg: reg, st: st, exifW: exifW, log: log,
		root: root, every: every,
		tmo:      2 * time.Hour,
		sweep:    time.Hour,
		maxPar:   make(chan struct{}, 2),
		queued:   map[string]bool{},
		scanning: map[string]bool{},
		lastScan: map[string]time.Time{},
		queue:    make(chan string, 64),
	}
}

// setHub connects the scheduler with the notification channel: a scan that
// changes the index notifies the owner so that the open tabs reload without
// waiting for their next poll. Without a hub (tests) it simply does not notify.
func (s *scheduler) setHub(h *live.Hub) { s.hub = h }

// publishIndexChange notifies the owner that its photo index has changed.
func (s *scheduler) publishIndexChange(owner string) {
	if s.hub == nil {
		return
	}
	s.hub.Publish(owner, live.Event{Topic: photosTopic})
}

// maybeScan is the activity hook (Touch of the registry): it enqueues the scan
// of the owner if there is none running or queued and one is due by age (or it
// is the first one).
func (s *scheduler) maybeScan(owner string) {
	s.mu.Lock()
	due := !s.scanning[owner] && (s.lastScan[owner].IsZero() || time.Since(s.lastScan[owner]) > s.every)
	s.mu.Unlock()
	if due {
		s.enqueue(owner)
	}
}

// requestScan enqueues an explicit rescan (POST /api/admin/rescan) of the owner
// of the request only.
func (s *scheduler) requestScan(owner string) { s.enqueue(owner) }

// enqueue puts the owner into the queue with dedupe (if it is already queued or
// being scanned, no-op, if the queue is full it is dropped with a log entry -
// it will be retried on the next activity or tick).
func (s *scheduler) enqueue(owner string) {
	if owner == "" {
		return
	}
	s.mu.Lock()
	if s.queued[owner] || s.scanning[owner] {
		s.mu.Unlock()
		return
	}
	s.queued[owner] = true
	s.mu.Unlock()
	select {
	case s.queue <- owner:
	default:
		s.mu.Lock()
		delete(s.queued, owner)
		s.mu.Unlock()
		s.log.Warn("scan queue full: dropped", "owner", owner)
	}
}

// run is the scheduler loop: scheduled ticker (seeded users plus active Basic
// sessions) and purge of Bearer sessions older than 24h. It returns when ctx is
// cancelled.
func (s *scheduler) run(ctx context.Context) {
	// Second line of defence of I1 (invalid config in a Module built by
	// hand): it degrades to the default with a log entry, it does not panic
	// in NewTicker(<=0).
	every := s.every
	if every <= 0 {
		s.log.Error("OCAPPS_PHOTOS_SCAN_EVERY <= 0 (invalid config), using the default",
			"got", every, "default", config.DefaultScanEvery)
		every = config.DefaultScanEvery
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	sweeper := time.NewTicker(s.sweep)
	defer sweeper.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, owner := range s.reg.activeBasicOwners() {
				s.enqueue(owner)
			}
		case <-sweeper.C:
			if n := s.reg.purge(time.Now()); n > 0 {
				s.log.Info("Bearer sessions purged for inactivity (>24h)", "n", n)
			}
		case owner := <-s.queue:
			s.mu.Lock()
			delete(s.queued, owner)
			s.mu.Unlock()
			go func() {
				s.maxPar <- struct{}{} // global semaphore: 2 concurrent scans
				defer func() { <-s.maxPar }()
				s.scanOne(ctx, owner)
			}()
		}
	}
}

// scanOne runs the scan plus EXIF of one owner with the credential of its
// session. On webdav.ErrUnauthorized: it invalidates the Bearer session and does
// NOT count the scan (it will resume on the next user activity), the index stays
// intact because the scanner aborts before the soft-delete (H8 §4).
func (s *scheduler) scanOne(ctx context.Context, owner string) {
	s.mu.Lock()
	if s.scanning[owner] {
		s.mu.Unlock()
		return
	}
	s.scanning[owner] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.scanning, owner)
		s.mu.Unlock()
	}()

	sess := s.reg.get(owner)
	if sess == nil {
		return // session purged between enqueueing and starting
	}
	sctx, cancel := context.WithTimeout(ctx, s.tmo)
	defer cancel()

	spaces, err := s.reg.resolveSpaces(sctx, sess, owner)
	if err != nil {
		if errors.Is(err, webdav.ErrUnauthorized) {
			s.reg.markExpired(owner)
			s.log.Info("scan postponed: expired token, it will resume on the next activity", "owner", owner)
		} else {
			s.log.Error("scan: spaces not resolvable", "owner", owner, "err", err)
		}
		return
	}

	scanner := index.NewScanner(sess.dav, webdav.DefaultOptions(), s.st, s.log)
	scanner.OnIndexChange = s.publishIndexChange
	// Every space visible to the owner (personal, project spaces and shares,
	// see issue #145) is walked in turn. The soft-delete of each space is
	// scoped to its own prefix, so the spaces do not touch each other's rows.
	// Each space has its OWN timeout (s.tmo): a big personal library cannot eat
	// the budget of the spaces that come after it (measured on
	// drive.domatix.cloud: 11k photos plus their folders took the whole 2h
	// cycle and the second space was never scanned).
	deadline := s.tmo
	for _, d := range spaces {
		space := index.Space{ID: d.ID, Name: d.Name, Type: d.DriveType, WebDAVURL: d.WebDAVURL}
		ssctx, cancel := context.WithTimeout(ctx, deadline)
		started := time.Now()
		err := scanner.ScanSpace(ssctx, owner, space, s.root)
		cancel()
		if err != nil {
			if errors.Is(err, webdav.ErrUnauthorized) {
				s.reg.markExpired(owner)
				s.log.Info("scan postponed: expired token, it will resume on the next activity", "owner", owner)
				return // does NOT count as lastScan: it is retried on fresh activity
			}
			s.log.Error("scan", "owner", owner, "space", d.Name, "err", err, "duration", time.Since(started).Round(time.Second))
			continue
		}
		s.log.Info("space scanned", "owner", owner, "space", d.Name, "duration", time.Since(started).Round(time.Second))
	}
	// The scan (or its non-auth attempt) counts for the ScanEvery rhythm.
	s.mu.Lock()
	s.lastScan[owner] = time.Now()
	s.mu.Unlock()

	// The EXIF backlog gets its own budget: the scan above may have used the
	// whole cycle timeout, and the dates of what was just indexed are what the
	// timeline shows.
	exifCtx, exifCancel := context.WithTimeout(ctx, s.tmo)
	defer exifCancel()
	if err := s.exifW.Run(exifCtx, owner, sess.dav); err != nil && errors.Is(err, webdav.ErrUnauthorized) {
		s.reg.markExpired(owner)
		s.log.Info("exif postponed: expired token, it will resume on the next activity", "owner", owner)
		return
	}
	// The EXIF fixes dates and GPS of what was just scanned: a second
	// notification so that the open tab does not keep the mtime date.
	s.publishIndexChange(owner)
}
