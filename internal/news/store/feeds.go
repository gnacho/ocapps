package store

import (
	"crypto/md5"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const feedCols = `f.id, f.url, f.title, f.favicon, f.added, f.next_update, f.folder_id,
	COALESCE(c.unread, 0), f.ordering, f.link, f.pinned, f.update_error_count,
	NULLIF(f.last_update_error, ''), f.no_new_streak, f.user_id, f.url_hash, f.full_content, f.retention_days, f.is_podcast,
	f.auth_user, f.auth_pass`

const feedSelect = `SELECT ` + feedCols + `
	FROM feeds f
	LEFT JOIN (SELECT feed_id, COUNT(*) AS unread FROM items WHERE unread = 1 GROUP BY feed_id) c
		ON c.feed_id = f.id`

func scanFeed(row interface{ Scan(...any) error }) (*Feed, error) {
	f := &Feed{}
	var lastErr *string
	var pinned, full, retention, podcast int
	err := row.Scan(&f.ID, &f.URL, &f.Title, &f.FaviconLink, &f.Added, &f.NextUpdateTime,
		&f.FolderID, &f.UnreadCount, &f.Ordering, &f.Link, &pinned,
		&f.UpdateErrorCount, &lastErr, &f.NoNewStreak, &f.UserID, &f.URLHash, &full, &retention, &podcast,
		&f.AuthUser, &f.AuthPassEnc)
	if err != nil {
		return nil, err
	}
	f.LastUpdateError = lastErr
	f.Pinned = pinned != 0
	f.FullContent = full != 0
	f.RetentionDays = retention
	f.IsPodcast = podcast != 0
	return f, nil
}

// ListFeeds returns all the user feeds with their unreadCount.
func (s *Store) ListFeeds(userID int64) ([]Feed, error) {
	rows, err := s.db.Query(feedSelect+` WHERE f.user_id = ? ORDER BY f.title`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Feed{}
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func (s *Store) GetFeed(userID, feedID int64) (*Feed, error) {
	f, err := scanFeed(s.db.QueryRow(feedSelect+` WHERE f.user_id = ? AND f.id = ?`, userID, feedID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// FeedExistsByURL checks whether the user already follows that URL.
func (s *Store) FeedExistsByURL(userID int64, url string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM feeds WHERE user_id = ? AND url = ?`, userID, url).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// CreateFeed inserts the feed (already validated and fetched) and its items.
// It returns ErrConflict if the URL already exists.
func (s *Store) CreateFeed(userID int64, url string, folderID *int64, title, link, favicon string, items []NewItem) (*Feed, error) {
	return s.createFeed(userID, url, folderID, title, link, favicon, items, false)
}

// CreateFeedFull like CreateFeed setting the full content detection.
func (s *Store) CreateFeedFull(userID int64, url string, folderID *int64, title, link, favicon string, items []NewItem, fullContent bool) (*Feed, error) {
	return s.createFeed(userID, url, folderID, title, link, favicon, items, fullContent)
}

func (s *Store) createFeed(userID int64, url string, folderID *int64, title, link, favicon string, items []NewItem, fullContent bool) (*Feed, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO feeds (user_id, folder_id, url, url_hash, link, title, favicon, added, next_update, full_content, is_podcast)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, folderID, url, md5Hex(url), link, title, favicon, now(), now()+600,
		boolInt(fullContent), boolInt(hasMediaEnclosure(items)))
	if err != nil {
		return nil, ErrConflict
	}
	feedID, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	// A newly created feed has no filter (feed_filter table empty for it),
	// so there is no need to apply keywords here.
	var newest int64
	for _, it := range items {
		r, err := tx.Exec(itemInsertSQL(), feedID, userID, it.GUID, it.GUIDHash, it.URL, it.Title,
			it.Author, it.PubDate, it.Body, it.EnclosureMime, it.EnclosureLink,
			it.MediaThumbnail, it.MediaDescription, it.Fingerprint, it.ClusterKey, now(), boolInt(it.Filtered))
		if err != nil {
			return nil, err
		}
		if id, _ := r.LastInsertId(); id > newest {
			newest = id
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetFeed(userID, feedID)
}

// ReplaceFeedItems: full refresh of a feed. It inserts new items
// (INSERT OR IGNORE by guid_hash) and returns how many were new,
// no_new_streak feeds the adaptive interval of the scheduler.
// fullContent: if this batch already brings full articles (sticky: MAX).
// Items that disappeared from the feed are NOT deleted (same as News).
func (s *Store) ReplaceFeedItems(feedID, userID int64, title, link string, items []NewItem, fullContent bool) (int64, error) {
	// Apply the filter BEFORE opening the tx: the Store uses a single connection and
	// querying the filter inside the tx would cause a deadlock (MaxOpenConns=1).
	if err := s.ApplyFilterToItems(feedID, items); err != nil {
		return 0, err
	}
	auto, err := s.autoReadForFeed(userID, feedID)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE feeds SET title = ?, link = ?, update_error_count = 0, last_update_error = '' WHERE id = ?`,
		title, link, feedID); err != nil {
		return 0, err
	}
	var inserted int64
	for _, it := range items {
		r, err := tx.Exec(itemInsertSQL(), feedID, userID, it.GUID, it.GUIDHash, it.URL, it.Title,
			it.Author, it.PubDate, it.Body, it.EnclosureMime, it.EnclosureLink,
			it.MediaThumbnail, it.MediaDescription, it.Fingerprint, it.ClusterKey, now(), boolInt(it.Filtered))
		if err != nil {
			return 0, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			inserted += n
			// auto-mark-read (#40): user rules that match the title
			if id, _ := r.LastInsertId(); id > 0 && autoReadMatches(auto, it.Title) {
				if _, err := tx.Exec(`UPDATE items SET unread = 0, last_modified = ? WHERE id = ?`, now(), id); err != nil {
					return 0, err
				}
			}
		}
	}
	streak := 0
	if inserted == 0 {
		streak = 1 // signal: add 1 to the existing streak (see UPDATE)
	}
	if _, err := tx.Exec(
		`UPDATE feeds SET no_new_streak = CASE WHEN ? = 1 THEN no_new_streak + 1 ELSE 0 END,
			full_content = MAX(full_content, ?), is_podcast = MAX(is_podcast, ?) WHERE id = ?`,
		streak, boolInt(fullContent), boolInt(hasMediaEnclosure(items)), feedID); err != nil {
		return 0, err
	}
	return inserted, tx.Commit()
}

// hasMediaEnclosure: true if any item of the batch brings an audio or
// video enclosure (it indicates that the feed is a podcast).
func hasMediaEnclosure(items []NewItem) bool {
	for _, it := range items {
		if it.EnclosureMime != nil {
			m := *it.EnclosureMime
			if strings.HasPrefix(m, "audio/") || strings.HasPrefix(m, "video/") {
				return true
			}
		}
	}
	return false
}

// autoReadMatches: true if any compiled auto-read pattern matches the
// item title (rules already filtered by feed in autoReadForFeed).
func autoReadMatches(rules []compiledAutoRead, title string) bool {
	for _, r := range rules {
		if r.pattern.MatchString(title) {
			return true
		}
	}
	return false
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) RecordFeedError(feedID int64, msg string) {
	s.db.Exec(`UPDATE feeds SET update_error_count = update_error_count + 1, last_update_error = ? WHERE id = ?`, msg, feedID)
}

// ListDueFeeds returns the feeds with next_update in the past (for the scheduler).
func (s *Store) ListDueFeeds(now int64, limit int) ([]Feed, error) {
	q := feedSelect + ` WHERE f.next_update <= ? ORDER BY f.next_update`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.Query(q, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Feed{}
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

// SetNextUpdate sets the next refresh time of the feed.
func (s *Store) SetNextUpdate(feedID, nextUpdate int64) error {
	_, err := s.db.Exec(`UPDATE feeds SET next_update = ? WHERE id = ?`, nextUpdate, feedID)
	return err
}

// FeedRow: minimal feed for the refresher (without aggregated counters).
type FeedRow struct {
	ID     int64
	UserID int64
	URL    string
	Link   string
	Title  string
}

// FeedByURLHash looks up a feed by the md5 hash of its URL (/favicon endpoint).
func (s *Store) FeedByURLHash(hash string) (*FeedRow, error) {
	row := &FeedRow{}
	err := s.db.QueryRow(`SELECT id, user_id, url, link, title FROM feeds WHERE url_hash = ? LIMIT 1`, hash).
		Scan(&row.ID, &row.UserID, &row.URL, &row.Link, &row.Title)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return row, nil
}

// CreateFeedDeferred inserts a feed without an initial fetch (OPML import):
// provisional title = URL and next_update=0 so that the scheduler
// refreshes it in the next cycle.
func (s *Store) CreateFeedDeferred(userID int64, url string, folderID *int64) (*Feed, error) {
	res, err := s.db.Exec(
		`INSERT INTO feeds (user_id, folder_id, url, url_hash, link, title, favicon, added, next_update)
		 VALUES (?, ?, ?, ?, '', ?, '', ?, 0)`,
		userID, folderID, url, md5Hex(url), url, now())
	if err != nil {
		return nil, ErrConflict
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetFeed(userID, id)
}

// UpdaterFeed: row for the admin /feeds/all route of the spec.
type UpdaterFeed struct {
	ID     int64  `json:"id"`
	UserID string `json:"userId"`
}

func (s *Store) AllFeedsWithUser() ([]UpdaterFeed, error) {
	rows, err := s.db.Query(
		`SELECT f.id, u.username FROM feeds f JOIN users u ON u.id = f.user_id ORDER BY f.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UpdaterFeed{}
	for rows.Next() {
		var f UpdaterFeed
		if err := rows.Scan(&f.ID, &f.UserID); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) DeleteFeed(userID, feedID int64) error {
	res, err := s.db.Exec(`DELETE FROM feeds WHERE user_id = ? AND id = ?`, userID, feedID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MoveFeed moves the feed to another folder (folderID nil = root).
func (s *Store) MoveFeed(userID, feedID int64, folderID *int64) error {
	if folderID != nil {
		var one int
		err := s.db.QueryRow(`SELECT 1 FROM folders WHERE user_id = ? AND id = ?`, userID, *folderID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
	}
	res, err := s.db.Exec(`UPDATE feeds SET folder_id = ? WHERE user_id = ? AND id = ?`, folderID, userID, feedID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RenameFeed(userID, feedID int64, title string) error {
	res, err := s.db.Exec(`UPDATE feeds SET title = ? WHERE user_id = ? AND id = ?`, title, userID, feedID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetFeedCredentials sets the feed Basic auth. passEnc is the password
// ALREADY encrypted (internal/cred), passEnc = nil keeps the current one (edit that
// only changes the user). Empty user + passEnc pointing to "" removes the auth.
func (s *Store) SetFeedCredentials(userID, feedID int64, user string, passEnc *string) error {
	var res sql.Result
	var err error
	if passEnc == nil {
		res, err = s.db.Exec(`UPDATE feeds SET auth_user = ? WHERE user_id = ? AND id = ?`, user, userID, feedID)
	} else {
		res, err = s.db.Exec(`UPDATE feeds SET auth_user = ?, auth_pass = ? WHERE user_id = ? AND id = ?`, user, *passEnc, userID, feedID)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetFeedScraperSelector returns the CSS extraction selector of the feed
// ("" = use generic readability).
func (s *Store) GetFeedScraperSelector(feedID int64) (string, error) {
	var sel string
	err := s.db.QueryRow(`SELECT COALESCE(scraper_selector, '') FROM feeds WHERE id = ?`, feedID).Scan(&sel)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return sel, nil
}

// SetFeedScraperSelector sets the CSS selector of the feed ("" = remove).
func (s *Store) SetFeedScraperSelector(userID, feedID int64, selector string) error {
	res, err := s.db.Exec(`UPDATE feeds SET scraper_selector = ? WHERE user_id = ? AND id = ?`,
		selector, userID, feedID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func itemInsertSQL() string {
	return `INSERT OR IGNORE INTO items
		(feed_id, user_id, guid, guid_hash, url, title, author, pub_date, body,
		 enclosure_mime, enclosure_link, media_thumbnail, media_description, fingerprint, cluster_key, last_modified, filtered)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return fmt.Sprintf("%x", sum)
}
