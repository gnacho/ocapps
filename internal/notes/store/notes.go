package store

import (
	"database/sql"
	"fmt"
	"time"
)

type Note struct {
	ID       int64  `json:"id"`
	Etag     string `json:"etag"`
	Readonly bool   `json:"readonly"`
	Modified int64  `json:"modified"`
	Title    string `json:"title"`
	Category string `json:"category"`
	Content  string `json:"content"`
	Favorite bool   `json:"favorite"`
}

type Store struct {
	db *sql.DB
}

type Settings map[string]string

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) CreateNote(user, title, content, category string, modified int64) (*Note, error) {
	tag := fmt.Sprintf("%x", modified)
	note := &Note{
		Title:    title,
		Content:  content,
		Category: category,
		Favorite: false,
		Modified: modified,
		Etag:     tag,
	}

	res, err := s.db.Exec(
		`INSERT INTO notes (user, title, content, category, favorite, modified, etag) VALUES (?,?,?,?,?,?,?)`,
		user, title, content, category, 0, modified, tag,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	note.ID = id

	return note, nil
}

func (s *Store) GetNote(user string, id int64) (*Note, error) {
	row := s.db.QueryRow(
		`SELECT id, etag, modified, title, category, content, favorite FROM notes WHERE id=? AND user=?`, id, user,
	)
	return scanNote(row)
}

func (s *Store) ListNotes(user string, category string, exclude []string, limit int, pruneBefore int64) ([]Note, error) {
	query := `SELECT id, etag, modified, title, category, content, favorite FROM notes WHERE user=?`
	args := []interface{}{user}

	if category != "" {
		query += " AND category=?"
		args = append(args, category)
	}

	query += ` ORDER BY favorite DESC, modified DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notes []Note
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		notes = append(notes, *n)
	}
	return notes, rows.Err()
}

func (s *Store) UpdateNote(user string, id int64, title *string, content *string, category *string, favorite *bool, modified int64) (*Note, error) {
	existing, err := s.GetNote(user, id)
	if err != nil {
		return nil, err
	}

	newEtag := fmt.Sprintf("%x", modified)

	if title != nil && *title != existing.Title {
		existing.Title = *title
	}
	if content != nil {
		existing.Content = *content
	}
	if category != nil {
		existing.Category = *category
	}
	if favorite != nil {
		favInt := 0
		if *favorite {
			favInt = 1
		}
		s.db.Exec(`UPDATE notes SET favorite=? WHERE id=? AND user=?`, favInt, id, user)
		existing.Favorite = *favorite
	}
	existing.Modified = modified
	existing.Etag = newEtag

	_, err = s.db.Exec(
		`UPDATE notes SET title=?, content=?, category=?, modified=?, etag=? WHERE id=? AND user=?`,
		existing.Title, existing.Content, existing.Category, existing.Modified, existing.Etag, id, user,
	)
	if err != nil {
		return nil, err
	}

	return existing, nil
}

func (s *Store) DeleteNote(user string, id int64) error {
	res, err := s.db.Exec(`DELETE FROM notes WHERE id=? AND user=?`, id, user)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) SetFavorite(user string, id int64, favorite bool) (*Note, error) {
	favInt := 0
	if favorite {
		favInt = 1
	}
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`UPDATE notes SET favorite=?, modified=?, etag=? WHERE id=? AND user=?`,
		favInt, now, fmt.Sprintf("%x", now), id, user,
	)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return s.GetNote(user, id)
}

func (s *Store) SearchNotes(user string, query string, maxResults int) ([]Note, error) {
	pattern := "%" + query + "%"
	rows, err := s.db.Query(
		`SELECT id, etag, modified, title, category, content, favorite FROM notes WHERE user=? AND (title LIKE ? OR content LIKE ?) ORDER BY modified DESC LIMIT ?`,
		user, pattern, pattern, maxResults,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notes []Note
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		notes = append(notes, *n)
	}
	return notes, rows.Err()
}

func (s *Store) GetSettings(user string) (Settings, error) {
	settings := make(Settings)
	rows, err := s.db.Query(`SELECT key, value FROM settings WHERE user=?`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		settings[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Seed the per-user defaults lazily on first read.
	if len(settings) == 0 {
		defaults := Settings{"notesPath": "Notes", "fileSuffix": ".md"}
		for key, value := range defaults {
			if _, err := s.db.Exec(`INSERT OR REPLACE INTO settings (user, key, value) VALUES (?,?,?)`, user, key, value); err != nil {
				return nil, err
			}
		}
		return defaults, nil
	}
	return settings, nil
}

func (s *Store) UpdateSettings(user string, newSettings Settings) (Settings, error) {
	current, err := s.GetSettings(user)
	if err != nil {
		return nil, err
	}

	for key, value := range newSettings {
		if value == "" {
			switch key {
			case "notesPath":
				value = "Notes"
			case "fileSuffix":
				value = ".md"
			default:
				value = ""
			}
		}
		current[key] = value
		_, err := s.db.Exec(`INSERT OR REPLACE INTO settings (user, key, value) VALUES (?,?,?)`, user, key, value)
		if err != nil {
			return nil, err
		}
	}

	return current, nil
}
