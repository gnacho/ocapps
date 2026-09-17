package scheduler

// Tests de I3: un pánico en una goroutine interna del scheduler NO tumba el
// proceso ni el loop; un pánico en el loop principal se recupera y vuelve
// como error de Run (el wiring marca el módulo failed, D3).

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/news/auth"
	"github.com/gnacho/ocapps/internal/news/refresher"
	"github.com/gnacho/ocapps/internal/news/store"
)

// panicRefresher paniquea en cada Refresh (un feed "envenenado").
type panicRefresher struct{ calls atomic.Int32 }

func (p *panicRefresher) Refresh(context.Context, *store.Feed) refresher.Result {
	p.calls.Add(1)
	panic("boom en worker")
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// feedVencido crea usuario + feed con next_update en el pasado para que el
// primer ciclo del scheduler lo recoja.
func feedVencido(t *testing.T, st *store.Store) {
	t.Helper()
	hash, _ := auth.HashPassword("x")
	uid, err := st.CreateUser("u", hash, "u", "user")
	if err != nil {
		t.Fatal(err)
	}
	f, err := st.CreateFeed(uid, "https://x.example/feed", nil, "t", "https://x.example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	st.SetNextUpdate(f.ID, time.Now().Unix()-1)
}

// Un pánico en un worker de cycle() se recupera: el proceso sigue, el loop
// principal continúa programando ciclos (el refresher vuelve a ser llamado)
// y Run retorna nil al cancelar (apagado normal).
func TestPanicoEnWorkerNoTumbaElLoop(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	feedVencido(t, st)

	pr := &panicRefresher{}
	s := New(st, pr, nil, discardLog(), 20*time.Millisecond, 2, 0, nil, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Varios ciclos con pánico recuperado en cada uno: el loop sigue vivo.
	waitFor(t, 5*time.Second, func() bool { return pr.calls.Load() >= 3 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run tras pánico en worker: %v (el pánico era en goroutine interna, debía ser apagado normal)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run no retornó tras cancelar (¿worker colgado por el pánico?)")
	}
}

// Un pánico en el loop PRINCIPAL (aquí: dentro de cycle, vía hook de test)
// se recupera y Run devuelve error → el wiring marca el módulo failed.
func TestPanicoEnLoopPrincipalDevuelveError(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	pr := &panicRefresher{}
	s := New(st, pr, nil, discardLog(), time.Hour, 1, 0, nil, "")
	var once atomic.Bool
	s.cycleHook = func() {
		if once.CompareAndSwap(false, true) {
			panic("boom en el loop principal")
		}
	}

	err = s.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "pánico") {
		t.Fatalf("Run debería devolver el pánico del loop principal como error, got %v", err)
	}
}
