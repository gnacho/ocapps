package refresher

// Pipeline tests: decrypt and fetch failures, the WebSub ingest path, the ntfy
// notification and the adaptive scheduling caps.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/news/notify"
	"github.com/gnacho/ocapps/internal/news/store"
)

const rssFeed = `<?xml version="1.0"?><rss version="2.0"><channel>
<title>WebSub feed</title><link>https://websub.example</link><description>d</description>
<item><title>first</title><link>https://websub.example/1</link>
<guid isPermaLink="false">w-1</guid><description>body one</description></item>
</channel></rss>`

func TestRefreshUnreadableCredentials(t *testing.T) {
	st, uid, ff, r := newEnv(t)
	f, err := st.CreateFeed(uid, "https://a.example/f", nil, "T", "https://a.example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Not a ciphertext of this secret: the decrypt fails before any fetch.
	broken := "this-is-not-a-ciphertext"
	if err := st.SetFeedCredentials(uid, f.ID, "user", &broken); err != nil {
		t.Fatal(err)
	}
	// The caller holds the feed it loaded, so reload it after the write.
	if f, err = st.GetFeed(uid, f.ID); err != nil {
		t.Fatal(err)
	}

	res := r.Refresh(context.Background(), f)
	if res.Err == nil {
		t.Fatal("an unreadable credential must be an error")
	}
	if ff.fetchN != 0 {
		t.Fatalf("fetches = %d, the feed must not be downloaded without credentials", ff.fetchN)
	}
	got, err := st.GetFeed(uid, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpdateErrorCount != 1 || got.LastUpdateError == nil {
		t.Fatalf("the failure must be recorded on the feed: %+v", got)
	}
}

func TestIngestFromWebSub(t *testing.T) {
	st, uid, _, r := newEnv(t)
	f, err := st.CreateFeed(uid, "https://websub.example/feed", nil, "Old title", "https://old.example", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	res := r.Ingest(context.Background(), f.ID, []byte(rssFeed))
	if res.Err != nil {
		t.Fatalf("Ingest: %v", res.Err)
	}
	if res.Inserted != 1 {
		t.Fatalf("inserted = %d, want the only item of the body", res.Inserted)
	}

	items, err := st.ListItems(store.ItemFilter{UserID: uid, Type: 3, BatchSize: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Title != "first" {
		t.Fatalf("items = %+v, want the ingested one", items)
	}

	// The body is the source of truth for the feed metadata.
	got, err := st.GetFeed(uid, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "WebSub feed" || got.Link != "https://websub.example" {
		t.Fatalf("feed title/link = %q/%q, want the ones in the body", got.Title, got.Link)
	}

	// Same body twice: no duplicate.
	if res := r.Ingest(context.Background(), f.ID, []byte(rssFeed)); res.Inserted != 0 {
		t.Fatalf("second ingest inserted %d items, want 0", res.Inserted)
	}
}

func TestIngestUnknownFeed(t *testing.T) {
	_, _, _, r := newEnv(t)
	res := r.Ingest(context.Background(), 999999, []byte(rssFeed))
	if res.Err == nil {
		t.Fatal("an unknown feed must be an error")
	}
	if res.Inserted != 0 {
		t.Fatalf("inserted = %d, want 0", res.Inserted)
	}
}

func TestIngestInvalidBody(t *testing.T) {
	st, uid, _, r := newEnv(t)
	f, err := st.CreateFeed(uid, "https://websub.example/feed", nil, "T", "https://websub.example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	res := r.Ingest(context.Background(), f.ID, []byte("<html>not a feed</html>"))
	if res.Err == nil {
		t.Fatal("an unparsable body must be an error")
	}
}

// ntfyServer records the notifications it receives.
type ntfyServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	posts []map[string]string
	paths []string
	code  int
}

func newNtfyServer(t *testing.T, code int) *ntfyServer {
	t.Helper()
	n := &ntfyServer{code: code}
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		n.mu.Lock()
		n.posts = append(n.posts, body)
		n.paths = append(n.paths, r.URL.Path)
		n.mu.Unlock()
		w.WriteHeader(n.code)
	}))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *ntfyServer) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.posts)
}

func (n *ntfyServer) last() (map[string]string, string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.posts) == 0 {
		return nil, ""
	}
	return n.posts[len(n.posts)-1], n.paths[len(n.paths)-1]
}

// notifierEnv builds a Refresher wired to an ntfy server and a feed.
func notifierEnv(t *testing.T, globalTopic string, code int) (*store.Store, *store.Feed, *Refresher, *ntfyServer) {
	t.Helper()
	st, uid, ff, _ := newEnv(t)
	f, err := st.CreateFeed(uid, "https://n.example/f", nil, "My feed", "https://n.example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newNtfyServer(t, code)
	r := New(st, ff, testCipher(t), log, time.Minute, 10*time.Minute,
		notify.NewAllowLocal(srv.srv.URL, globalTopic, log))
	return st, f, r, srv
}

func TestNotifyNewItemsUsesTheUserTopic(t *testing.T) {
	st, f, r, srv := notifierEnv(t, "global-topic", http.StatusOK)
	if err := st.SetUserSetting(f.UserID, "ntfy_topic", "user-topic"); err != nil {
		t.Fatal(err)
	}

	r.notifyNewItems(context.Background(), f, 1, []store.NewItem{{Title: "first item"}})

	if srv.count() != 1 {
		t.Fatalf("notifications = %d, want 1", srv.count())
	}
	post, path := srv.last()
	if path != "/user-topic" {
		t.Fatalf("path = %q, the user topic must win over the global one", path)
	}
	if post["topic"] != "user-topic" {
		t.Fatalf("topic = %q", post["topic"])
	}
	if post["title"] != "1 new article in My feed" {
		t.Fatalf("title = %q, want the singular noun", post["title"])
	}
	if post["message"] != "first item" {
		t.Fatalf("message = %q, want the first item title", post["message"])
	}
}

func TestNotifyNewItemsFallsBackToTheGlobalTopic(t *testing.T) {
	_, f, r, srv := notifierEnv(t, "global-topic", http.StatusOK)

	r.notifyNewItems(context.Background(), f, 2, []store.NewItem{{Title: "a"}, {Title: "b"}})

	if srv.count() != 1 {
		t.Fatalf("notifications = %d, want 1", srv.count())
	}
	post, path := srv.last()
	if path != "/global-topic" || post["topic"] != "global-topic" {
		t.Fatalf("path = %q, topic = %q, want the global topic", path, post["topic"])
	}
	if post["title"] != "2 new articles in My feed" {
		t.Fatalf("title = %q, want the plural noun", post["title"])
	}
}

func TestNotifyNewItemsWithoutTopicSendsNothing(t *testing.T) {
	_, f, r, srv := notifierEnv(t, "", http.StatusOK)

	r.notifyNewItems(context.Background(), f, 1, []store.NewItem{{Title: "a"}})

	if srv.count() != 0 {
		t.Fatalf("notifications = %d, want none without a topic", srv.count())
	}
}

func TestNotifyNewItemsWithoutNoveltySendsNothing(t *testing.T) {
	_, f, r, srv := notifierEnv(t, "global-topic", http.StatusOK)

	r.notifyNewItems(context.Background(), f, 0, nil)

	if srv.count() != 0 {
		t.Fatalf("notifications = %d, want none when nothing was inserted", srv.count())
	}
}

func TestNotifyNewItemsSurvivesANtfyError(t *testing.T) {
	_, f, r, srv := notifierEnv(t, "global-topic", http.StatusInternalServerError)

	// The failure is logged, never propagated: a broken ntfy cannot make a
	// refresh fail.
	r.notifyNewItems(context.Background(), f, 1, []store.NewItem{{Title: "a"}})

	if srv.count() != 1 {
		t.Fatalf("requests = %d, want the attempt", srv.count())
	}
}

func TestFeedIntervalUsesTheUserOverride(t *testing.T) {
	st, f, r, _ := notifierEnv(t, "", http.StatusOK)

	if got := r.feedInterval(f); got != time.Minute {
		t.Fatalf("interval = %v, want the global one", got)
	}
	if err := st.SetUserSetting(f.UserID, "feed_interval_min", "5"); err != nil {
		t.Fatal(err)
	}
	if got := r.feedInterval(f); got != 5*time.Minute {
		t.Fatalf("interval = %v, want the user override", got)
	}
	// An unusable override falls back to the global interval.
	for _, bad := range []string{"", "abc", "0", "-3"} {
		if err := st.SetUserSetting(f.UserID, "feed_interval_min", bad); err != nil {
			t.Fatal(err)
		}
		if got := r.feedInterval(f); got != time.Minute {
			t.Fatalf("interval with %q = %v, want the global one", bad, got)
		}
	}
}

func TestJitterIgnoresNonPositiveGaps(t *testing.T) {
	if got := jitter(0, 0.2); got != 0 {
		t.Fatalf("jitter(0) = %v, want 0", got)
	}
	if got := jitter(-time.Second, 0.2); got != -time.Second {
		t.Fatalf("jitter(-1s) = %v, want it untouched", got)
	}
	// A positive gap stays within the requested fraction.
	for i := 0; i < 50; i++ {
		got := jitter(time.Minute, 0.2)
		if got < 48*time.Second || got > 72*time.Second {
			t.Fatalf("jitter(1m) = %v, outside the ±20%% band", got)
		}
	}
}

func TestScheduleNextCapsTheErrorBackoff(t *testing.T) {
	st, f, r, _ := notifierEnv(t, "", http.StatusOK)
	r.interval = time.Hour
	r.jitterFn = func(d time.Duration, _ float64) time.Duration { return d }

	// 1h << 5 = 32h, over the daily cap.
	update := *f
	update.UpdateErrorCount = 10
	r.scheduleNext(&update, Result{FeedID: f.ID, Err: errFake})

	got, err := st.GetFeed(f.UserID, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertGap(t, got.NextUpdateTime, 24*time.Hour)
}

func TestScheduleNextCapsTheNoNoveltyGap(t *testing.T) {
	st, f, r, _ := notifierEnv(t, "", http.StatusOK)
	r.maxGap = 5 * time.Minute
	r.jitterFn = func(d time.Duration, _ float64) time.Duration { return d }

	// interval 1m << 3 (the streak shift is capped) is 8m, the maxGap cuts it.
	update := *f
	update.NoNewStreak = 5
	r.scheduleNext(&update, Result{FeedID: f.ID, Inserted: 0})

	got, err := st.GetFeed(f.UserID, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertGap(t, got.NextUpdateTime, 5*time.Minute)
}

func TestScheduleNextSurvivesAFailedUpdate(t *testing.T) {
	st, uid, _, r := newEnv(t)
	r.jitterFn = func(d time.Duration, _ float64) time.Duration { return d }

	// A feed that is not in the database: SetNextUpdate fails, the call only
	// logs it.
	missing := &store.Feed{ID: 424242, UserID: uid}
	r.scheduleNext(missing, Result{FeedID: missing.ID, Inserted: 3})

	if _, err := st.GetFeed(uid, missing.ID); err == nil {
		t.Fatal("the test needs a missing feed")
	}
}
