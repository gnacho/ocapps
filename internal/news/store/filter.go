package store

import (
	"database/sql"
	"strings"
)

// GetFeedFilter returns the keyword filter of the feed (empty if it does not exist).
func (s *Store) GetFeedFilter(feedID int64) (*FeedFilter, error) {
	f := &FeedFilter{FeedID: feedID}
	err := s.db.QueryRow(
		`SELECT title_keywords, body_keywords, url_keywords FROM feed_filter WHERE feed_id = ?`,
		feedID).Scan(&f.TitleKeywords, &f.BodyKeywords, &f.URLKeywords)
	if err == sql.ErrNoRows {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// SaveFeedFilter inserts or updates the feed filter. If it has no keywords,
// it deletes it (News behaviour: empty filter = no filter).
func (s *Store) SaveFeedFilter(f FeedFilter) error {
	if !f.HasFilter() {
		_, err := s.db.Exec(`DELETE FROM feed_filter WHERE feed_id = ?`, f.FeedID)
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO feed_filter (feed_id, title_keywords, body_keywords, url_keywords)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(feed_id) DO UPDATE SET
		   title_keywords = excluded.title_keywords,
		   body_keywords  = excluded.body_keywords,
		   url_keywords   = excluded.url_keywords`,
		f.FeedID, f.TitleKeywords, f.BodyKeywords, f.URLKeywords)
	return err
}

// DeleteFeedFilter deletes the feed filter.
func (s *Store) DeleteFeedFilter(feedID int64) error {
	_, err := s.db.Exec(`DELETE FROM feed_filter WHERE feed_id = ?`, feedID)
	return err
}

// ReapplyFeedFilter re-evaluates the filter (keywords + regex rules) over the
// items already stored for the feed: it sets filtered=1 for those that match and
// filtered=0 for those that do not. With nothing configured, it unfreezes all
// the items of the feed. It returns the number of items flagged as filtered.
func (s *Store) ReapplyFeedFilter(feedID int64, f FeedFilter) (int64, error) {
	userID, err := s.FeedUserID(feedID)
	if err != nil {
		return 0, err
	}
	fn, err := s.itemFilter(userID, feedID, &f)
	if err != nil {
		return 0, err
	}
	has, err := s.HasAnyFilter(userID, feedID)
	if err != nil {
		return 0, err
	}
	if !has {
		_, err := s.db.Exec(`UPDATE items SET filtered = 0 WHERE feed_id = ?`, feedID)
		if err != nil {
			return 0, err
		}
		return 0, nil
	}
	rows, err := s.db.Query(
		`SELECT id, url, title, body FROM items WHERE feed_id = ?`, feedID)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	type row struct {
		id int64
		ni NewItem
	}
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.ni.URL, &r.ni.Title, &r.ni.Body); err != nil {
			return 0, err
		}
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var marked int64
	for _, r := range pending {
		val := 0
		if fn(r.ni) {
			val = 1
		}
		if _, err := s.db.Exec(`UPDATE items SET filtered = ? WHERE id = ?`, val, r.id); err != nil {
			return 0, err
		}
		if val == 1 {
			marked++
		}
	}
	return marked, nil
}

// ReapplyGlobalRules re-evaluates the user global rules over all
// their items (after changing the global ones). It returns the number of flagged items.
func (s *Store) ReapplyGlobalRules(userID int64) (int64, error) {
	fn, err := s.itemFilter(userID, 0, &FeedFilter{})
	if err != nil {
		return 0, err
	}
	has, err := s.HasAnyFilter(userID, 0)
	if err != nil {
		return 0, err
	}
	if !has {
		_, err := s.db.Exec(`UPDATE items SET filtered = 0 WHERE user_id = ?`, userID)
		if err != nil {
			return 0, err
		}
		return 0, nil
	}
	rows, err := s.db.Query(
		`SELECT id, url, title, body FROM items WHERE user_id = ?`, userID)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	type row struct {
		id int64
		ni NewItem
	}
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.ni.URL, &r.ni.Title, &r.ni.Body); err != nil {
			return 0, err
		}
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var marked int64
	for _, r := range pending {
		val := 0
		if fn(r.ni) {
			val = 1
		}
		if _, err := s.db.Exec(`UPDATE items SET filtered = ? WHERE id = ?`, val, r.id); err != nil {
			return 0, err
		}
		if val == 1 {
			marked++
		}
	}
	return marked, nil
}

// ApplyFilterToItems flags the items that match the feed filter or the
// global/feed rules, in memory. It must be called BEFORE opening a
// transaction (the Store uses a single connection: querying the filter inside
// a tx would cause a deadlock).
func (s *Store) ApplyFilterToItems(feedID int64, items []NewItem) error {
	userID, err := s.FeedUserID(feedID)
	if err != nil {
		return err
	}
	f, err := s.GetFeedFilter(feedID)
	if err != nil {
		return err
	}
	has, err := s.HasAnyFilter(userID, feedID)
	if err != nil {
		return err
	}
	if !has {
		return nil
	}
	fn, err := s.itemFilter(userID, feedID, f)
	if err != nil {
		return err
	}
	for i := range items {
		items[i].Filtered = fn(items[i])
	}
	return nil
}

// containsAnyFold: true if text contains any keyword of the CSV list (fold).
func containsAnyFold(text, csv string) bool {
	tl := strings.ToLower(text)
	for _, kw := range strings.Split(csv, ",") {
		kw = strings.TrimSpace(kw)
		if kw != "" && strings.Contains(tl, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// plain strips the HTML tags from the body to match on plain text.
func plain(html string) string {
	var b strings.Builder
	inTag := false
	for _, r := range html {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return b.String()
}
