package refresher

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/cred"
	"github.com/gnacho/ocapps/internal/news/auth"
	"github.com/gnacho/ocapps/internal/news/feed"
	"github.com/gnacho/ocapps/internal/news/store"
)

// testCipher: cipher with an ephemeral feedsecret in a temp dir (common/cred,
// it replaces the old cred.Load of ocnews).
func testCipher(t *testing.T) *cred.Cipher {
	t.Helper()
	key, err := cred.LoadOrCreateSecret(filepath.Join(t.TempDir(), "feedsecret"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := cred.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type fakeFetcher struct {
	items  int
	fail   bool
	fetchN int
}

func (f *fakeFetcher) Fetch(_ context.Context, _, _, _ string) (*store.Feed, []store.NewItem, error) {
	f.fetchN++
	if f.fail {
		return nil, nil, errFake
	}
	fd := &store.Feed{Title: "T", Link: "https://s.example"}
	items := make([]store.NewItem, 0, f.items)
	for i := 0; i < f.items; i++ {
		guid := strings.Repeat(string(rune('a'+i)), 3) + string(rune(f.fetchN+'0'))
		items = append(items, store.NewItem{GUID: guid, GUIDHash: guid, Title: "n" + guid})
	}
	return fd, items, nil
}

func (f *fakeFetcher) Discover(_ context.Context, _ string) ([]feed.DiscoveredFeed, error) {
	return nil, nil
}

type errFetch struct{}

func (errFetch) Error() string { return "fake fetch error" }

var errFake error = errFetch{}

func newEnv(t *testing.T) (*store.Store, int64, *fakeFetcher, *Refresher) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	hash, err := auth.HashPassword("x")
	if err != nil {
		t.Fatal(err)
	}
	uid, err := st.CreateUser("u", hash, "u", "user")
	if err != nil {
		t.Fatal(err)
	}
	ff := &fakeFetcher{items: 3}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	creds := testCipher(t)
	r := New(st, ff, creds, log, time.Minute, 10*time.Minute, nil)
	return st, uid, ff, r
}

func gapUntil(nextUpdate int64) time.Duration {
	return time.Until(time.Unix(nextUpdate, 0))
}

func TestRefreshNewItemsResetsStreak(t *testing.T) {
	st, uid, _, r := newEnv(t)
	f, err := st.CreateFeed(uid, "https://a.example/f", nil, "T", "https://a.example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	res := r.Refresh(context.Background(), f)
	if res.Err != nil || res.Inserted == 0 {
		t.Fatalf("res: %+v", res)
	}
	got, err := st.GetFeed(uid, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.NoNewStreak != 0 {
		t.Errorf("streak must reset: %d", got.NoNewStreak)
	}
	// with novelty -> base gap (1m ±20%): between 48s and 72s
	if g := gapUntil(got.NextUpdateTime); g < 48*time.Second || g > 72*time.Second {
		t.Errorf("gap with novelty: %v", g)
	}
}

func TestRefreshNoNewItemsDoubles(t *testing.T) {
	st, uid, ff, r := newEnv(t)
	f, err := st.CreateFeed(uid, "https://b.example/f", nil, "T", "https://b.example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Refresh(context.Background(), f) // brings items, schedules base
	ff.items = 0                       // next fetch without novelty

	got, _ := st.GetFeed(uid, f.ID)
	res := r.Refresh(context.Background(), got)
	if res.Inserted != 0 {
		t.Fatalf("expected 0 new: %+v", res)
	}
	after, _ := st.GetFeed(uid, f.ID)
	if after.NoNewStreak != 1 {
		t.Errorf("streak must be 1: %d", after.NoNewStreak)
	}
	// without novelty (streak 1) -> 2m ±20%: between 96s and 144s.
	// Lower margin 95s: gapUntil measures after operations (it subtracts real latency).
	if g := gapUntil(after.NextUpdateTime); g < 95*time.Second || g > 145*time.Second {
		t.Errorf("gap without novelty: %v", g)
	}
}

func TestRefreshErrorBackoff(t *testing.T) {
	st, uid, ff, r := newEnv(t)
	f, err := st.CreateFeed(uid, "https://c.example/f", nil, "T", "https://c.example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ff.fail = true
	res := r.Refresh(context.Background(), f)
	if res.Err == nil {
		t.Fatal("expected an error")
	}
	got, _ := st.GetFeed(uid, f.ID)
	if got.UpdateErrorCount != 1 || got.LastUpdateError == nil {
		t.Fatalf("error not recorded: %+v", got)
	}
	// 1st error -> 2m ±20% (interval 1m << 1). Lower margin 95s because of the
	// gapUntil measurement latency (time.Until after the operations).
	if g := gapUntil(got.NextUpdateTime); g < 95*time.Second || g > 145*time.Second {
		t.Errorf("backoff gap 1st error: %v", g)
	}
}

func TestRefreshSanitizesBody(t *testing.T) {
	st, uid, _, _ := newEnv(t)
	dirty := &dirtyFetcher{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	creds := testCipher(t)
	r2 := New(st, dirty, creds, log, time.Minute, time.Minute, nil)
	f, err := st.CreateFeed(uid, "https://d.example/f", nil, "T", "https://d.example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r2.Refresh(context.Background(), f)
	items, err := st.ListItems(store.ItemFilter{UserID: uid, Type: 3, BatchSize: -1})
	if err != nil || len(items) == 0 {
		t.Fatalf("items: %v %d", err, len(items))
	}
	body := items[0].Body
	if strings.Contains(body, "<script") || strings.Contains(body, "onerror") {
		t.Errorf("unsanitized body: %s", body)
	}
	if !strings.Contains(body, "hola") {
		t.Errorf("readable content lost: %s", body)
	}
}

type dirtyFetcher struct{}

func (d *dirtyFetcher) Fetch(_ context.Context, _, _, _ string) (*store.Feed, []store.NewItem, error) {
	fd := &store.Feed{Title: "T", Link: "https://s.example"}
	items := []store.NewItem{{
		GUID: "g1", GUIDHash: "g1", Title: "t",
		Body: `<p>hola</p><script>alert(1)</script><img src="https://x/i.png" onerror="evil()">`,
	}}
	return fd, items, nil
}

func (d *dirtyFetcher) Discover(_ context.Context, _ string) ([]feed.DiscoveredFeed, error) {
	return nil, nil
}
