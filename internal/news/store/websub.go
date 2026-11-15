package store

import (
	"database/sql"
	"errors"
	"time"
)

// WebSubSub: WebSub subscription of a feed to its hub.
type WebSubSub struct {
	FeedID     int64
	Hub        string
	Topic      string
	Secret     string
	Callback   string
	LeaseUntil int64
	Status     string
}

// UpsertWebSub records (or updates) the hub and topic of a feed, keeping
// the existing secret/callback/lease (a re-subscription does not regenerate them).
func (s *Store) UpsertWebSub(feedID int64, hub, topic string) error {
	_, err := s.db.Exec(
		`INSERT INTO websub (feed_id, hub, topic) VALUES (?, ?, ?)
		 ON CONFLICT(feed_id) DO UPDATE SET hub = excluded.hub, topic = excluded.topic`,
		feedID, hub, topic)
	return err
}

// GetWebSub returns the subscription of a feed (ErrNotFound if there is none).
func (s *Store) GetWebSub(feedID int64) (*WebSubSub, error) {
	var w WebSubSub
	err := s.db.QueryRow(
		`SELECT feed_id, hub, topic, secret, callback, lease_until, status FROM websub WHERE feed_id = ?`,
		feedID).Scan(&w.FeedID, &w.Hub, &w.Topic, &w.Secret, &w.Callback, &w.LeaseUntil, &w.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// SaveWebSubSecret sets the secret and the callback of the subscription.
func (s *Store) SaveWebSubSecret(feedID int64, secret, callback string) error {
	_, err := s.db.Exec(`UPDATE websub SET secret = ?, callback = ? WHERE feed_id = ?`, secret, callback, feedID)
	return err
}

// SaveWebSubLease sets the lease_until of the subscription.
func (s *Store) SaveWebSubLease(feedID, leaseUntil int64) error {
	_, err := s.db.Exec(`UPDATE websub SET lease_until = ? WHERE feed_id = ?`, leaseUntil, feedID)
	return err
}

// SaveWebSubStatus sets the subscription status (pending|active|error).
func (s *Store) SaveWebSubStatus(feedID int64, status string) error {
	_, err := s.db.Exec(`UPDATE websub SET status = ? WHERE feed_id = ?`, status, feedID)
	return err
}

// WebSubDue returns subscriptions that need to be (re)subscribed: those that
// never had a lease or whose lease expires within the horizon.
func (s *Store) WebSubDue(now int64, horizon time.Duration) ([]WebSubSub, error) {
	cutoff := now + int64(horizon/time.Second)
	rows, err := s.db.Query(
		`SELECT feed_id, hub, topic, secret, callback, lease_until, status FROM websub
		 WHERE lease_until = 0 OR lease_until <= ? ORDER BY feed_id`, cutoff)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []WebSubSub{}
	for rows.Next() {
		var w WebSubSub
		if err := rows.Scan(&w.FeedID, &w.Hub, &w.Topic, &w.Secret, &w.Callback, &w.LeaseUntil, &w.Status); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteWebSub deletes the feed subscription.
func (s *Store) DeleteWebSub(feedID int64) error {
	_, err := s.db.Exec(`DELETE FROM websub WHERE feed_id = ?`, feedID)
	return err
}

// GetFeedByID returns a feed by id (without user scoping, internal use).
func (s *Store) GetFeedByID(feedID int64) (*Feed, error) {
	f, err := scanFeed(s.db.QueryRow(feedSelect+` WHERE f.id = ?`, feedID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}
