package scheduler

// I3 tests: a panic in an internal scheduler goroutine does NOT take down the
// process nor the loop, a panic in the main loop is recovered and returned
// as a Run error (the wiring marks the module failed, D3).

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

// panicRefresher panics on every Refresh (a "poisoned" feed).
type panicRefresher struct{ calls atomic.Int32 }

func (p *panicRefresher) Refresh(context.Context, *store.Feed) refresher.Result {
	p.calls.Add(1)
	panic("boom en worker")
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// expiredFeed creates a user + feed with next_update in the past so that the
// first scheduler cycle picks it up.
func expiredFeed(t *testing.T, st *store.Store) {
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
	_ = st.SetNextUpdate(f.ID, time.Now().Unix()-1)
}

// A panic in a cycle() worker is recovered: the process goes on, the main
// loop keeps scheduling cycles (the refresher is called again)
// and Run returns nil on cancel (normal shutdown).
func TestPanicInWorkerDoesNotKillLoop(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	expiredFeed(t, st)

	pr := &panicRefresher{}
	s := New(st, pr, nil, discardLog(), 20*time.Millisecond, 2, 0, nil, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Several cycles with a recovered panic in each one: the loop stays alive.
	waitFor(t, 5*time.Second, func() bool { return pr.calls.Load() >= 3 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after a worker panic: %v (the panic was in an internal goroutine, it should be a normal shutdown)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancelling (worker hung by the panic?)")
	}
}

// A panic in the MAIN loop (here: inside cycle, through the test hook)
// is recovered and Run returns an error -> the wiring marks the module failed.
func TestPanicInMainLoopReturnsError(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	pr := &panicRefresher{}
	s := New(st, pr, nil, discardLog(), time.Hour, 1, 0, nil, "")
	var once atomic.Bool
	s.cycleHook = func() {
		if once.CompareAndSwap(false, true) {
			panic("boom in the main loop")
		}
	}

	err = s.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("Run should return the main loop panic as an error, got %v", err)
	}
}
