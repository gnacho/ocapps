// Package refresher: shared refresh logic of a feed between the
// API updater endpoint and the periodic scheduler. It sanitizes the bodies
// on ingest and computes the next interval (adaptive by novelty,
// exponential backoff on error, with jitter).
package refresher

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/gnacho/ocapps/internal/common/cred"
	"github.com/gnacho/ocapps/internal/news/feed"
	"github.com/gnacho/ocapps/internal/news/notify"
	"github.com/gnacho/ocapps/internal/news/store"
)

type Refresher struct {
	store    *store.Store
	fetcher  feed.Fetcher
	cred     *cred.Cipher
	log      *slog.Logger
	interval time.Duration // base interval between refreshes
	maxGap   time.Duration // cap of the adaptive interval
	notifier *notify.Notifier
}

func New(st *store.Store, f feed.Fetcher, c *cred.Cipher, log *slog.Logger, interval, maxGap time.Duration, n *notify.Notifier) *Refresher {
	return &Refresher{store: st, fetcher: f, cred: c, log: log, interval: interval, maxGap: maxGap, notifier: n}
}

// Result summarizes a refresh for the scheduler.
type Result struct {
	FeedID   int64
	Inserted int64
	Err      error
	Hub      string // WebSub hub detected in the feed (#44)
}

// Refresh re-downloads the feed, persists new items (sanitized) and
// schedules the next_update according to the result. A fetch failure is NOT an
// error of the method: it is recorded in the feed (update_error_count) and its backoff
// becomes part of the next_update.
func (r *Refresher) Refresh(ctx context.Context, f *store.Feed) Result {
	res := r.refresh(ctx, f)
	r.scheduleNext(f, res)
	return res
}

func (r *Refresher) refresh(ctx context.Context, f *store.Feed) Result {
	authPass, err := r.cred.Decrypt(f.AuthPassEnc)
	if err != nil {
		r.store.RecordFeedError(f.ID, "unreadable credentials (key rotated?)")
		r.log.Warn("decrypt credentials failed", "url", f.URL, "err", err)
		return Result{FeedID: f.ID, Err: err}
	}
	parsed, items, err := r.fetcher.Fetch(ctx, f.URL, f.AuthUser, authPass)
	if err != nil {
		r.store.RecordFeedError(f.ID, err.Error())
		r.log.Warn("fetch failed", "url", f.URL, "err", err)
		return Result{FeedID: f.ID, Err: err}
	}
	return r.ingest(ctx, f, parsed, items)
}

// Ingest processes a body received through WebSub (#44) without downloading the
// feed again: it parses, sanitizes and persists as a normal refresh.
func (r *Refresher) Ingest(ctx context.Context, feedID int64, body []byte) Result {
	f, err := r.store.GetFeedByID(feedID)
	if err != nil {
		r.log.Warn("websub: unknown feed", "feed", feedID, "err", err)
		return Result{FeedID: feedID, Err: err}
	}
	parsed, items, err := feed.Parse(body)
	if err != nil {
		r.log.Warn("websub: parse body failed", "feed", feedID, "err", err)
		return Result{FeedID: feedID, Err: err}
	}
	return r.ingest(ctx, f, parsed, items)
}

// ingest persists the parsed batch (shared between fetch and WebSub).
func (r *Refresher) ingest(ctx context.Context, f *store.Feed, parsed *store.Feed, items []store.NewItem) Result {
	title, link := f.Title, f.Link
	if parsed.Title != "" {
		title = parsed.Title
	}
	if parsed.Link != "" {
		link = parsed.Link
	}
	fullContent := feed.HasFullContent(items)
	feed.SanitizeItems(items)
	inserted, err := r.store.ReplaceFeedItems(f.ID, f.UserID, title, link, items, fullContent)
	if err != nil {
		return Result{FeedID: f.ID, Err: err}
	}
	// reflect it in the struct for the caller (favicon uses f.Link)
	f.Title, f.Link = title, link
	if inserted > 0 {
		r.log.Info("feed refreshed", "url", f.URL, "new", inserted)
		r.notifyNewItems(ctx, f, inserted, items)
	}
	return Result{FeedID: f.ID, Inserted: inserted, Hub: parsed.Hub}
}

// notifyNewItems: push notification (ntfy) when the feed brings new
// articles. The topic is the user one (user_settings ntfy_topic) or, if not, the
// global one configured on start. No topic -> no notification.
func (r *Refresher) notifyNewItems(ctx context.Context, f *store.Feed, inserted int64, items []store.NewItem) {
	if r.notifier == nil || inserted <= 0 {
		return
	}
	topic, err := r.store.GetUserSetting(f.UserID, "ntfy_topic")
	if err != nil || topic == "" {
		topic = r.notifier.Topic()
	}
	if topic == "" {
		return
	}
	noun := "article"
	if inserted > 1 {
		noun = "articles"
	}
	title := fmt.Sprintf("%d new %s in %s", inserted, noun, f.Title)
	msg := ""
	if len(items) > 0 {
		msg = items[0].Title
	}
	if err := r.notifier.Notify(ctx, topic, title, msg); err != nil {
		r.log.Warn("ntfy notification failed", "feed", f.ID, "err", err)
	}
}

// scheduleNext sets next_update:
//   - failure: exponential backoff according to update_error_count (interval*2^err, cap 24h)
//   - no novelty: interval that doubles with the RESULTING streak (interval*2^(streak+1), cap maxGap)
//   - with novelty: base interval
//
// All with ±20% jitter so as not to sync feeds with each other.
// The base interval can be a per-user override (user_settings[feed_interval_min]).
func (r *Refresher) scheduleNext(f *store.Feed, res Result) {
	interval := r.feedInterval(f)
	var gap time.Duration
	switch {
	case res.Err != nil:
		errCount := f.UpdateErrorCount + 1 // RecordFeedError already incremented it
		gap = interval << min(errCount, 5)
		if gap > 24*time.Hour {
			gap = 24 * time.Hour
		}
	case res.Inserted == 0:
		streak := f.NoNewStreak + 1 // streak after this refresh without novelty
		gap = interval << min(streak, 3)
		if gap > r.maxGap {
			gap = r.maxGap
		}
	default:
		gap = interval
	}
	gap = jitter(gap, 0.2)
	r.store.SetNextUpdate(f.ID, time.Now().Add(gap).Unix())
}

// feedInterval returns the base interval of the feed: the user override
// (minutes, 0 if it is not set) or the global one.
func (r *Refresher) feedInterval(f *store.Feed) time.Duration {
	if v, err := r.store.GetUserSetting(f.UserID, "feed_interval_min"); err == nil && v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return r.interval
}

func jitter(d time.Duration, frac float64) time.Duration {
	if d <= 0 {
		return d
	}
	delta := float64(d) * frac
	return time.Duration(float64(d) + (rand.Float64()*2-1)*delta)
}
