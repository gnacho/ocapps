package photos

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/gnacho/ocapps/internal/common/config"
	"github.com/gnacho/ocapps/internal/common/webdav"
	"github.com/gnacho/ocapps/internal/photos/exif"
	"github.com/gnacho/ocapps/internal/photos/index"
	"github.com/gnacho/ocapps/internal/photos/store"
)

// scheduler es el planificador de scans multi-owner (H8 §7). Reemplaza al
// scanLoop global single-tenant:
//   - Cada request autenticado (Touch en el registry) marca actividad: si el
//     owner no está escaneándose y su último scan es más viejo que ScanEvery
//     (o nunca se escaneó) se encola su scan — el índice crece por actividad.
//   - Un ticker ScanEvery encola los scans de los usuarios sembrados por
//     config (OCAPPS_PHOTOS_USERS) y de las sesiones Basic duraderas activas.
//   - La cola tiene dedupe por owner y un semáforo global de 2 scans
//     concurrentes; cada scan tiene timeout de 2h (como antes).
type scheduler struct {
	reg    *registry
	st     *store.Store
	log    *slog.Logger
	exifW  *exif.Worker
	root   string
	every  time.Duration
	tmo    time.Duration // timeout por scan (2h)
	sweep  time.Duration // periodo de purga de sesiones Bearer >24h
	maxPar chan struct{} // semáforo global de scans concurrentes

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

// maybeScan es el hook de actividad (Touch del registry): encola el scan del
// owner si no hay uno en curso/cola y toca por antigüedad (o es el primero).
func (s *scheduler) maybeScan(owner string) {
	s.mu.Lock()
	due := !s.scanning[owner] && (s.lastScan[owner].IsZero() || time.Since(s.lastScan[owner]) > s.every)
	s.mu.Unlock()
	if due {
		s.enqueue(owner)
	}
}

// requestScan encola un rescan explícito (POST /api/admin/rescan) solo del
// owner del request.
func (s *scheduler) requestScan(owner string) { s.enqueue(owner) }

// enqueue mete al owner en la cola con dedupe (si ya está encolado o
// escaneándose, no-op; si la cola está llena, se descarta con log — se
// reintentará con la próxima actividad o tick).
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
		s.log.Warn("cola de scans llena: se descarta", "owner", owner)
	}
}

// run es el loop del scheduler: ticker programado (usuarios sembrados +
// sesiones Basic activas) y purga de sesiones Bearer >24h. Retorna al
// cancelar ctx.
func (s *scheduler) run(ctx context.Context) {
	// Segunda línea de defensa de I1 (config inválida en un Module construido
	// a mano): degrada al default con log, no paniquea en NewTicker(≤0).
	every := s.every
	if every <= 0 {
		s.log.Error("OCAPPS_PHOTOS_SCAN_EVERY <= 0 (config inválida); se usa el default",
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
				s.log.Info("sesiones Bearer purgadas por inactividad (>24h)", "n", n)
			}
		case owner := <-s.queue:
			s.mu.Lock()
			delete(s.queued, owner)
			s.mu.Unlock()
			go func() {
				s.maxPar <- struct{}{} // semáforo global: 2 scans concurrentes
				defer func() { <-s.maxPar }()
				s.scanOne(ctx, owner)
			}()
		}
	}
}

// scanOne ejecuta el scan + EXIF de un owner con la credencial de su sesión.
// Ante webdav.ErrUnauthorized: invalida la sesión Bearer y NO cuenta el scan
// (se reanudará con la próxima actividad del usuario); el índice queda
// intacto porque el scanner aborta antes del soft-delete (H8 §4).
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
		return // sesión purgada entre el encolado y el inicio
	}
	sctx, cancel := context.WithTimeout(ctx, s.tmo)
	defer cancel()

	webdavURL, err := s.reg.resolveSpace(sctx, sess, owner)
	if err != nil {
		if errors.Is(err, webdav.ErrUnauthorized) {
			s.reg.markExpired(owner)
			s.log.Info("scan pospuesto: token caducado, se reanudará con su próxima actividad", "owner", owner)
		} else {
			s.log.Error("scan: espacio no resoluble", "owner", owner, "err", err)
		}
		return
	}

	scanner := index.NewScanner(sess.dav, webdav.DefaultOptions(), s.st, s.log)
	if err := scanner.ScanSpace(sctx, owner, webdavURL, s.root); err != nil {
		if errors.Is(err, webdav.ErrUnauthorized) {
			s.reg.markExpired(owner)
			s.log.Info("scan pospuesto: token caducado, se reanudará con su próxima actividad", "owner", owner)
			return // NO cuenta como lastScan: se reintenta con actividad fresca
		}
		s.log.Error("scan", "owner", owner, "err", err)
	}
	// El scan (o su intento no-auth) cuenta para el ritmo ScanEvery.
	s.mu.Lock()
	s.lastScan[owner] = time.Now()
	s.mu.Unlock()

	if err := s.exifW.Run(sctx, owner, sess.dav); err != nil && errors.Is(err, webdav.ErrUnauthorized) {
		s.reg.markExpired(owner)
		s.log.Info("exif pospuesto: token caducado, se reanudará con la próxima actividad", "owner", owner)
	}
}
