package store

import (
	"database/sql"
	"errors"
	"fmt"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("conflict")

func (s *Store) CreateUser(username, passwordHash, displayName, role string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO users (username, password_hash, display_name, role, language, created_at) VALUES (?, ?, ?, ?, 'auto', ?)`,
		username, passwordHash, displayName, role, now())
	if err != nil {
		return 0, fmt.Errorf("create user: %w", err)
	}
	return res.LastInsertId()
}

// CreateUserWithOCID creates a shadow user with canonical OpenCloud identity.
func (s *Store) CreateUserWithOCID(username, ocID, passwordHash, displayName, role string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO users (username, password_hash, display_name, role, language, oc_id, created_at) VALUES (?, ?, ?, ?, 'auto', ?, ?)`,
		username, passwordHash, displayName, role, ocID, now())
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrConflict
		}
		return 0, fmt.Errorf("create user: %w", err)
	}
	return res.LastInsertId()
}

// isUniqueViolation detects a UNIQUE constraint error (code 2067) from
// SQLite, e.g. a shadow user with an oc_id that already exists due to a race between
// two requests of the same OpenCloud user.
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
	}
	return false
}

// SetUserOCID links the oc_id to an existing user (e.g. a Basic shadow
// created before the first Bearer login).
func (s *Store) SetUserOCID(userID int64, ocID string) error {
	_, err := s.db.Exec(`UPDATE users SET oc_id = ? WHERE id = ?`, ocID, userID)
	return err
}

func (s *Store) GetUserByOCID(ocID string) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(
		`SELECT id, username, password_hash, display_name, role, language, oc_id, created_at, last_login_at
		 FROM users WHERE oc_id = ?`, ocID,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DisplayName, &u.Role, &u.Language, &u.OCID, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// SetUserLanguage sets the preferred language of the user (auto|es|en).
func (s *Store) SetUserLanguage(userID int64, language string) error {
	_, err := s.db.Exec(`UPDATE users SET language = ? WHERE id = ?`, language, userID)
	return err
}

// BootstrapUser creates the first admin user if the table is empty.
// It returns true if it was created.
func (s *Store) BootstrapUser(username, passwordHash string) (bool, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return false, err
	}
	if count > 0 {
		return false, nil
	}
	_, err := s.CreateUser(username, passwordHash, username, "admin")
	return err == nil, err
}

func (s *Store) GetUserByUsername(username string) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(
		`SELECT id, username, password_hash, display_name, role, language, oc_id, created_at, last_login_at
		 FROM users WHERE username = ?`, username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DisplayName, &u.Role, &u.Language, &u.OCID, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// BootstrapCount returns the number of users (to validate booting without credentials).
func (s *Store) BootstrapCount(count *int) error {
	return s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(count)
}

// TouchLogin updates last_login_at with throttle: only if the last mark
// is older than 60s (it avoids one write per request on every API GET).
func (s *Store) TouchLogin(userID int64) {
	s.db.Exec(`UPDATE users SET last_login_at = ? WHERE id = ? AND last_login_at < ? - 60`,
		now(), userID, now())
}

// CountUsers returns the total number of users.
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountAdmins returns the number of users with the admin role.
func (s *Store) CountAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&n)
	return n, err
}

// ListUsers returns all the users (without hashes) for admin.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(
		`SELECT id, username, '', display_name, role, language, oc_id, created_at, last_login_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u := User{}
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DisplayName, &u.Role, &u.Language, &u.OCID, &u.CreatedAt, &u.LastLoginAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) GetUserByID(id int64) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(
		`SELECT id, username, password_hash, display_name, role, language, oc_id, created_at, last_login_at
		 FROM users WHERE id = ?`, id,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DisplayName, &u.Role, &u.Language, &u.OCID, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// UpdateProfile updates display_name and language of the user themselves.
func (s *Store) UpdateProfile(userID int64, displayName, language string) error {
	_, err := s.db.Exec(`UPDATE users SET display_name = ?, language = ? WHERE id = ?`,
		displayName, language, userID)
	return err
}

// UpdatePassword replaces the password hash after validating the current one.
func (s *Store) UpdatePassword(userID int64, passwordHash string) error {
	_, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, passwordHash, userID)
	return err
}

// UpdateUserFields (admin): role and language of any user.
func (s *Store) UpdateUserFields(userID int64, role, language *string) error {
	if role != nil {
		if _, err := s.db.Exec(`UPDATE users SET role = ? WHERE id = ?`, *role, userID); err != nil {
			return err
		}
	}
	if language != nil {
		if _, err := s.db.Exec(`UPDATE users SET language = ? WHERE id = ?`, *language, userID); err != nil {
			return err
		}
	}
	return nil
}

// DeleteUser deletes a user, the ON DELETE CASCADE FKs clean their data.
// Guard: do not delete the last admin (it returns ErrConflict).
func (s *Store) DeleteUser(userID int64) error {
	var target User
	err := s.db.QueryRow(`SELECT role FROM users WHERE id = ?`, userID).Scan(&target.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if target.Role == "admin" {
		var admins int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&admins); err != nil {
			return err
		}
		if admins <= 1 {
			return ErrConflict
		}
	}
	_, err = s.db.Exec(`DELETE FROM users WHERE id = ?`, userID)
	return err
}
