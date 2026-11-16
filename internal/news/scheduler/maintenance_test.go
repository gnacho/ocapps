package scheduler

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	commonstore "github.com/gnacho/ocapps/internal/common/store"
	"github.com/gnacho/ocapps/internal/news/auth"
	"github.com/gnacho/ocapps/internal/news/favicon"
	"github.com/gnacho/ocapps/internal/news/refresher"
	"github.com/gnacho/ocapps/internal/news/store"
	"github.com/gnacho/ocapps/internal/news/websub"
)

// resultRefresher returns a fixed Result, so a cycle can be driven without a
// network fetch.
type resultRefresher struct {
	res refresher.Result
}

func (r *resultRefresher) Refresh(context.Context, *store.Feed) refresher.Result {
	return r.res
}

// newStore opens a news store in a temporary directory.
func newStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

// newUser creates the single test user and returns its id.
func newUser(t *testing.T, st *store.Store) int64 {
	t.Helper()
	hash, err := auth.HashPassword("x")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	uid, err := st.CreateUser("u", hash, "u", "user")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return uid
}

// dueFeed creates a feed with one item and schedules it for the next cycle.
func dueFeed(t *testing.T, st *store.Store, uid int64, url string) *store.Feed {
	t.Helper()
	f, err := st.CreateFeed(uid, url, nil, "t", "https://x.example", "", []store.NewItem{{
		GUID:     "g-1",
		GUIDHash: "h-1",
		URL:      "https://x.example/1",
		Title:    "one",
		PubDate:  time.Now().Unix(),
		Body:     "body",
	}})
	if err != nil {
		t.Fatalf("CreateFeed: %v", err)
	}
	if err := st.SetNextUpdate(f.ID, time.Now().Unix()-1); err != nil {
		t.Fatalf("SetNextUpdate: %v", err)
	}
	return f
}

// markReadAndBackdate marks every item of the feed as read and moves its
// last_modified back by age through a second connection (the store does not
// expose a clock). WAL plus a 10s busy timeout make the concurrent write safe.
func markReadAndBackdate(t *testing.T, st *store.Store, dbPath string, uid, feedID int64, age time.Duration) {
	t.Helper()
	items, err := st.ListItems(store.ItemFilter{UserID: uid, Type: 3, BatchSize: -1})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		if it.FeedID == feedID {
			ids = append(ids, it.ID)
		}
	}
	if len(ids) == 0 {
		t.Fatalf("feed %d has no items to mark", feedID)
	}
	if err := st.MarkItemsUnreadFlag(uid, ids, false); err != nil {
		t.Fatalf("mark read: %v", err)
	}

	db, err := commonstore.Open(dbPath)
	if err != nil {
		t.Fatalf("open second connection: %v", err)
	}
	defer func() { _ = db.Close() }()
	old := time.Now().Add(-age).Unix()
	for _, id := range ids {
		if _, err := db.Exec(`UPDATE items SET last_modified = ? WHERE id = ?`, old, id); err != nil {
			t.Fatalf("backdate item %d: %v", id, err)
		}
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	s := New(nil, nil, nil, discardLog(), 0, 0, 0, nil, "")
	if s.tick != 30*time.Second {
		t.Fatalf("tick = %v, want the 30s default", s.tick)
	}
	if s.concurrency != 4 {
		t.Fatalf("concurrency = %d, want the 4 default", s.concurrency)
	}
}

func TestRunRetentionDisabledReturnsImmediately(t *testing.T) {
	s := New(nil, nil, nil, discardLog(), time.Hour, 1, 0, nil, "")
	done := make(chan struct{})
	go func() {
		s.runRetention(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runRetention must return at once when retention is disabled")
	}
}

func TestRunRetentionPurgesOldReadItems(t *testing.T) {
	st, path := newStore(t)
	uid := newUser(t, st)
	f := dueFeed(t, st, uid, "https://x.example/feed")
	markReadAndBackdate(t, st, path, uid, f.ID, 48*time.Hour)

	// The purge runs once on start, then the loop waits for the daily ticker.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(st, nil, nil, discardLog(), time.Hour, 1, 24*time.Hour, nil, "")
	s.runRetention(ctx)

	n, err := st.CountItems(store.ItemFilter{UserID: uid, Type: 3, GetRead: true, BatchSize: -1})
	if err != nil {
		t.Fatalf("CountItems: %v", err)
	}
	if n != 0 {
		t.Fatalf("items left = %d, want the old read item purged", n)
	}
}

func TestRunRetentionKeepsRecentAndUnreadItems(t *testing.T) {
	st, path := newStore(t)
	uid := newUser(t, st)
	f := dueFeed(t, st, uid, "https://x.example/feed")
	markReadAndBackdate(t, st, path, uid, f.ID, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(st, nil, nil, discardLog(), time.Hour, 1, 24*time.Hour, nil, "")
	s.runRetention(ctx)

	n, _ := st.CountItems(store.ItemFilter{UserID: uid, Type: 3, GetRead: true, BatchSize: -1})
	if n != 1 {
		t.Fatalf("items left = %d, the read item is younger than the retention", n)
	}
}

func TestRunRetentionHonoursThePerFeedOverride(t *testing.T) {
	st, path := newStore(t)
	uid := newUser(t, st)

	// Feed with a 7 day override and an item 10 days old: purged by its own
	// rule even though the global retention is longer.
	own := dueFeed(t, st, uid, "https://own.example/feed")
	if err := st.SetFeedRetentionDays(own.ID, uid, 7); err != nil {
		t.Fatalf("SetFeedRetentionDays: %v", err)
	}
	markReadAndBackdate(t, st, path, uid, own.ID, 10*24*time.Hour)

	// Feed without override and a recent item: the global cutoff keeps it.
	global := dueFeed(t, st, uid, "https://global.example/feed")
	markReadAndBackdate(t, st, path, uid, global.ID, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(st, nil, nil, discardLog(), time.Hour, 1, 30*24*time.Hour, nil, "")
	s.runRetention(ctx)

	left, err := st.ListItems(store.ItemFilter{UserID: uid, Type: 3, GetRead: true, BatchSize: -1})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(left) != 1 || left[0].FeedID != global.ID {
		t.Fatalf("items left = %+v, want only the recent one of the global feed", left)
	}
}

// hubServer records the subscription forms it receives.
type hubServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	forms []url.Values
	code  int
}

func newHubServer(t *testing.T, code int) *hubServer {
	t.Helper()
	h := &hubServer{code: code}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.forms = append(h.forms, r.PostForm)
		h.mu.Unlock()
		w.WriteHeader(h.code)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hubServer) calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.forms)
}

func (h *hubServer) last() url.Values {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.forms) == 0 {
		return nil
	}
	return h.forms[len(h.forms)-1]
}

func TestWebSubCycleSubscribes(t *testing.T) {
	st, _ := newStore(t)
	uid := newUser(t, st)
	f := dueFeed(t, st, uid, "https://x.example/feed")
	hub := newHubServer(t, http.StatusAccepted)
	if err := st.UpsertWebSub(f.ID, hub.srv.URL, "https://x.example/feed"); err != nil {
		t.Fatalf("UpsertWebSub: %v", err)
	}

	s := New(st, nil, nil, discardLog(), time.Hour, 1, 0, websub.NewAllowLocal(), "https://backend.example")
	s.websubCycle(context.Background())

	if hub.calls() != 1 {
		t.Fatalf("hub calls = %d, want 1", hub.calls())
	}
	form := hub.last()
	if form.Get("hub.mode") != "subscribe" {
		t.Fatalf("hub.mode = %q", form.Get("hub.mode"))
	}
	if form.Get("hub.topic") != "https://x.example/feed" {
		t.Fatalf("hub.topic = %q", form.Get("hub.topic"))
	}
	if want := "https://backend.example/websub/callback/" + strconv.FormatInt(f.ID, 10); form.Get("hub.callback") != want {
		t.Fatalf("hub.callback = %q, want %q", form.Get("hub.callback"), want)
	}
	if form.Get("hub.secret") == "" {
		t.Fatal("a subscription must carry a secret")
	}

	sub, err := st.GetWebSub(f.ID)
	if err != nil {
		t.Fatalf("GetWebSub: %v", err)
	}
	if sub.Secret != form.Get("hub.secret") {
		t.Fatalf("stored secret = %q, want the one sent", sub.Secret)
	}
	if sub.Callback != form.Get("hub.callback") {
		t.Fatalf("stored callback = %q", sub.Callback)
	}
	if sub.Status != "pending" {
		t.Fatalf("status = %q, want pending until the hub verifies", sub.Status)
	}
	if sub.LeaseUntil <= time.Now().Unix() {
		t.Fatalf("lease = %d, want a provisional one in the future", sub.LeaseUntil)
	}
}

func TestWebSubCycleKeepsTheExistingSecret(t *testing.T) {
	st, _ := newStore(t)
	uid := newUser(t, st)
	f := dueFeed(t, st, uid, "https://x.example/feed")
	hub := newHubServer(t, http.StatusOK)
	if err := st.UpsertWebSub(f.ID, hub.srv.URL, "https://x.example/feed"); err != nil {
		t.Fatalf("UpsertWebSub: %v", err)
	}
	if err := st.SaveWebSubSecret(f.ID, "known-secret", "https://old.example/cb"); err != nil {
		t.Fatalf("SaveWebSubSecret: %v", err)
	}

	s := New(st, nil, nil, discardLog(), time.Hour, 1, 0, websub.NewAllowLocal(), "https://backend.example")
	s.websubCycle(context.Background())

	if got := hub.last().Get("hub.secret"); got != "known-secret" {
		t.Fatalf("hub.secret = %q, a renewal must reuse the stored secret", got)
	}
}

func TestWebSubCycleRecordsHubErrors(t *testing.T) {
	st, _ := newStore(t)
	uid := newUser(t, st)
	f := dueFeed(t, st, uid, "https://x.example/feed")
	hub := newHubServer(t, http.StatusInternalServerError)
	if err := st.UpsertWebSub(f.ID, hub.srv.URL, "https://x.example/feed"); err != nil {
		t.Fatalf("UpsertWebSub: %v", err)
	}

	s := New(st, nil, nil, discardLog(), time.Hour, 1, 0, websub.NewAllowLocal(), "https://backend.example")
	s.websubCycle(context.Background())

	sub, err := st.GetWebSub(f.ID)
	if err != nil {
		t.Fatalf("GetWebSub: %v", err)
	}
	if sub.Status != "error" {
		t.Fatalf("status = %q, want error after a failed subscription", sub.Status)
	}
	if sub.Secret == "" {
		t.Fatal("the generated secret must be stored even when the hub fails")
	}
}

func TestWebSubCycleSkipsAnInvalidHub(t *testing.T) {
	st, _ := newStore(t)
	uid := newUser(t, st)
	f := dueFeed(t, st, uid, "https://x.example/feed")
	// Not an http(s) URL: the client refuses it before any connection.
	if err := st.UpsertWebSub(f.ID, "ftp://hub.example/", "https://x.example/feed"); err != nil {
		t.Fatalf("UpsertWebSub: %v", err)
	}

	s := New(st, nil, nil, discardLog(), time.Hour, 1, 0, websub.NewAllowLocal(), "https://backend.example")
	s.websubCycle(context.Background())

	sub, _ := st.GetWebSub(f.ID)
	if sub.Status != "error" {
		t.Fatalf("status = %q, want error for an unusable hub", sub.Status)
	}
}

func TestWebSubCycleStopsOnCanceledContext(t *testing.T) {
	st, _ := newStore(t)
	uid := newUser(t, st)
	f := dueFeed(t, st, uid, "https://x.example/feed")
	hub := newHubServer(t, http.StatusOK)
	if err := st.UpsertWebSub(f.ID, hub.srv.URL, "https://x.example/feed"); err != nil {
		t.Fatalf("UpsertWebSub: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(st, nil, nil, discardLog(), time.Hour, 1, 0, websub.NewAllowLocal(), "https://backend.example")
	s.websubCycle(ctx)

	if hub.calls() != 0 {
		t.Fatalf("hub calls = %d, a canceled context must not subscribe", hub.calls())
	}
}

func TestRunStartsTheWebSubLoop(t *testing.T) {
	st, _ := newStore(t)
	uid := newUser(t, st)
	f := dueFeed(t, st, uid, "https://x.example/feed")
	hub := newHubServer(t, http.StatusOK)
	if err := st.UpsertWebSub(f.ID, hub.srv.URL, "https://x.example/feed"); err != nil {
		t.Fatalf("UpsertWebSub: %v", err)
	}

	s := New(st, nil, nil, discardLog(), 10*time.Millisecond, 1, 0, websub.NewAllowLocal(), "https://backend.example")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	waitFor(t, 5*time.Second, func() bool { return hub.calls() >= 1 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want a normal shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancelling with the WebSub loop running")
	}
}

func TestCycleRecordsTheDetectedHub(t *testing.T) {
	st, _ := newStore(t)
	uid := newUser(t, st)
	f := dueFeed(t, st, uid, "https://x.example/feed")

	r := &resultRefresher{res: refresher.Result{FeedID: f.ID, Inserted: 1, Hub: "https://hub.example/"}}
	s := New(st, r, nil, discardLog(), time.Hour, 1, 0, nil, "")
	s.cycle(context.Background())

	sub, err := st.GetWebSub(f.ID)
	if err != nil {
		t.Fatalf("GetWebSub: %v", err)
	}
	if sub.Hub != "https://hub.example/" {
		t.Fatalf("hub = %q, want the one the feed advertised", sub.Hub)
	}
	if sub.Topic != f.URL {
		t.Fatalf("topic = %q, want the feed url %q", sub.Topic, f.URL)
	}
}

func TestCycleSkipsTheFaviconWithoutNovelty(t *testing.T) {
	st, _ := newStore(t)
	uid := newUser(t, st)
	f := dueFeed(t, st, uid, "https://x.example/feed")

	cache, err := favicon.NewCache(filepath.Join(t.TempDir(), "favicons"), discardLog())
	if err != nil {
		t.Fatalf("favicon.NewCache: %v", err)
	}
	r := &resultRefresher{res: refresher.Result{FeedID: f.ID}}
	s := New(st, r, cache, discardLog(), time.Hour, 1, 0, nil, "")
	s.cycle(context.Background())

	if cache.Has(favicon.Hash(f.URL)) {
		t.Fatal("a refresh without novelty must not fetch the favicon")
	}
}

func TestRandomSecret(t *testing.T) {
	a, b := randomSecret(), randomSecret()
	if a == b {
		t.Fatal("two secrets must differ")
	}
	if len(a) != 48 {
		t.Fatalf("len = %d, want 48 hex characters (24 bytes)", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("secret is not hex: %v", err)
	}
}
