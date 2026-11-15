package store

import (
	"database/sql"
	"errors"

	"github.com/gnacho/ocapps/internal/news/rules"
)

// Rules: block/keep filter in "Field=regex" multiline text format.
type Rules struct {
	Block string `json:"block"`
	Keep  string `json:"keep"`
}

func (r Rules) HasRules() bool {
	return r.Block != "" || r.Keep != ""
}

// GetFeedRules returns the feed rules (empty if they do not exist).
func (s *Store) GetFeedRules(feedID int64) (*Rules, error) {
	r := &Rules{}
	err := s.db.QueryRow(`SELECT block, keep FROM feed_rules WHERE feed_id = ?`, feedID).
		Scan(&r.Block, &r.Keep)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// SaveFeedRules inserts or updates the feed rules. Empty = they are deleted.
func (s *Store) SaveFeedRules(feedID int64, r Rules) error {
	if !r.HasRules() {
		_, err := s.db.Exec(`DELETE FROM feed_rules WHERE feed_id = ?`, feedID)
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO feed_rules (feed_id, block, keep) VALUES (?, ?, ?)
		 ON CONFLICT(feed_id) DO UPDATE SET block = excluded.block, keep = excluded.keep`,
		feedID, r.Block, r.Keep)
	return err
}

func (s *Store) DeleteFeedRules(feedID int64) error {
	_, err := s.db.Exec(`DELETE FROM feed_rules WHERE feed_id = ?`, feedID)
	return err
}

// GetGlobalRules returns the user global rules (empty if there are none).
func (s *Store) GetGlobalRules(userID int64) (*Rules, error) {
	r := &Rules{}
	err := s.db.QueryRow(`SELECT block, keep FROM global_rules WHERE user_id = ?`, userID).
		Scan(&r.Block, &r.Keep)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// SaveGlobalRules stores or deletes the user global rules.
func (s *Store) SaveGlobalRules(userID int64, r Rules) error {
	if !r.HasRules() {
		_, err := s.db.Exec(`DELETE FROM global_rules WHERE user_id = ?`, userID)
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO global_rules (user_id, block, keep) VALUES (?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET block = excluded.block, keep = excluded.keep`,
		userID, r.Block, r.Keep)
	return err
}

// FeedUserID returns the owner of a feed.
func (s *Store) FeedUserID(feedID int64) (int64, error) {
	var uid int64
	err := s.db.QueryRow(`SELECT user_id FROM feeds WHERE id = ?`, feedID).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return uid, nil
}

// HasAnyFilter: true if the feed has keywords, rules or the user has global
// rules. It avoids re-evaluating items when nothing is configured.
func (s *Store) HasAnyFilter(userID, feedID int64) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT (SELECT COUNT(*) FROM feed_filter WHERE feed_id = ? AND
		           (title_keywords <> '' OR body_keywords <> '' OR url_keywords <> ''))
		      + (SELECT COUNT(*) FROM feed_rules WHERE feed_id = ? AND (block <> '' OR keep <> ''))
		      + (SELECT COUNT(*) FROM global_rules WHERE user_id = ? AND (block <> '' OR keep <> ''))`,
		feedID, feedID, userID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// itemFilter composes keywords (News 28.4.0) + global and feed regex
// rules. true = the item must be discarded (filtered). Rules with an invalid
// regex (impossible through the API, which validates) are ignored for robustness.
func (s *Store) itemFilter(userID, feedID int64, kw *FeedFilter) (func(NewItem) bool, error) {
	gr, err := s.GetGlobalRules(userID)
	if err != nil {
		return nil, err
	}
	fr, err := s.GetFeedRules(feedID)
	if err != nil {
		return nil, err
	}
	gc, gerr := rules.Parse(gr.Block, gr.Keep)
	fc, ferr := rules.Parse(fr.Block, fr.Keep)
	return func(it NewItem) bool {
		if kw.HasFilter() && kw.Matches(it) {
			return true
		}
		f := rules.Fields{Title: it.Title, URL: it.URL, Author: it.Author, Content: plain(it.Body), PubDate: it.PubDate}
		if gerr == nil && gc.Ignore(f) {
			return true
		}
		if ferr == nil && fc.Ignore(f) {
			return true
		}
		return false
	}, nil
}
