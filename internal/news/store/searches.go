package store

import (
	"database/sql"
	"errors"
)

// SavedSearch: persisted user search (virtual feed).
type SavedSearch struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Query     string `json:"query"`
	CreatedAt int64  `json:"createdAt"`
}

// ListSavedSearches returns the user saved searches.
func (s *Store) ListSavedSearches(userID int64) ([]SavedSearch, error) {
	rows, err := s.db.Query(
		`SELECT id, name, query, created_at FROM saved_searches WHERE user_id = ? ORDER BY id`,
		userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []SavedSearch{}
	for rows.Next() {
		var ss SavedSearch
		if err := rows.Scan(&ss.ID, &ss.Name, &ss.Query, &ss.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

// GetSavedSearch returns a user saved search.
func (s *Store) GetSavedSearch(userID, searchID int64) (*SavedSearch, error) {
	var ss SavedSearch
	err := s.db.QueryRow(
		`SELECT id, name, query, created_at FROM saved_searches WHERE user_id = ? AND id = ?`,
		userID, searchID).Scan(&ss.ID, &ss.Name, &ss.Query, &ss.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ss, nil
}

// CreateSavedSearch stores a new user search.
func (s *Store) CreateSavedSearch(userID int64, name, query string) (*SavedSearch, error) {
	res, err := s.db.Exec(
		`INSERT INTO saved_searches (user_id, name, query) VALUES (?, ?, ?)`,
		userID, name, query)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return s.GetSavedSearch(userID, id)
}

// DeleteSavedSearch deletes the saved search (ErrNotFound if it does not exist).
func (s *Store) DeleteSavedSearch(userID, searchID int64) error {
	res, err := s.db.Exec(`DELETE FROM saved_searches WHERE user_id = ? AND id = ?`, userID, searchID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
