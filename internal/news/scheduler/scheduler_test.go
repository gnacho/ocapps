package scheduler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gnacho/ocapps/internal/common/cred"
	"github.com/gnacho/ocapps/internal/news/auth"
	"github.com/gnacho/ocapps/internal/news/favicon"
	"github.com/gnacho/ocapps/internal/news/feed"
	"github.com/gnacho/ocapps/internal/news/refresher"
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

const rssBody = `<?xml version="1.0"?><rss version="2.0"><channel>
<title>Feed Test</title><link>https://x.example</link><description>d</description>ITEMSPLACEHOLDER
</channel></rss>`

func itemXML(guid string) string {
	return `<item><title>` + guid + `</title><link>https://x.example/` + guid +
		`</link><guid isPermaLink="false">` + guid + `</guid><description>body ` + guid + `</description></item>`
}

// feedServer serves an RSS whose items grow when the add button is pressed.
func feedServer(t *testing.T) (*httptest.Server, *int32, *int32) {
	t.Helper()
	var hits, extra int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		items := itemXML("base-1") + itemXML("base-2")
		for i := int32(0); i < atomic.LoadInt32(&extra); i++ {
			items += itemXML("extra-" + string(rune('a'+i)))
		}
		w.Write([]byte(strings.Replace(rssBody, "ITEMSPLACEHOLDER", items, 1)))
	}))
	t.Cleanup(ts.Close)
	return ts, &hits, &extra
}

// TestSchedulerPicksUpNewItems: feed subscribed with 2 items, the scheduler
// (100ms tick) discovers a 3rd one added to the feed without restarting the backend.
func TestSchedulerPicksUpNewItems(t *testing.T) {
	ts, hits, extra := feedServer(t)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	hash, _ := auth.HashPassword("x")
	uid, _ := st.CreateUser("u", hash, "u", "user")

	// subscription with an initial fetch through the store API (2 items)
	f, items, err := feed.NewHTTPFetcherAllowLocal(5*time.Second).Fetch(context.Background(), ts.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	created, err := st.CreateFeed(uid, ts.URL, nil, f.Title, f.Link, "", items)
	if err != nil {
		t.Fatal(err)
	}
	// CreateFeed schedules at +600s (already fetched on subscription): force
	// expiry so that the scheduler cycle picks it up NOW.
	st.SetNextUpdate(created.ID, time.Now().Unix()-1)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	creds := testCipher(t)
	refresh := refresher.New(st, feed.NewHTTPFetcherAllowLocal(5*time.Second), creds, log, 200*time.Millisecond, time.Hour, nil)
	fc, _ := favicon.NewCache(filepath.Join(dir, "fav"), log)
	s := New(st, refresh, fc, log, 100*time.Millisecond, 2, 0, nil, "")

	ctx, cancel := context.WithCancel(context.Background())
	var done = make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() {
		cancel()
		<-done
	}()

	// the scheduler refreshes in the first cycle, we wait for >=2 hits
	waitFor(t, 5*time.Second, func() bool { return atomic.LoadInt32(hits) >= 2 })

	// a new item shows up in the source feed
	atomic.AddInt32(extra, 1)

	waitFor(t, 8*time.Second, func() bool {
		newest, _ := st.NewestItemID(uid)
		return newest >= 3
	})
	n, _ := st.CountItems(store.ItemFilter{UserID: uid, Type: 3, BatchSize: -1})
	if n != 3 {
		t.Fatalf("items after discovery: %d (expected 3)", n)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not reached in " + timeout.String())
}
