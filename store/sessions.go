package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CreateSession records a session for an organiser, by the hash of its
// token, until it expires.
func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, login string, expiresAt time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, login, expires_at) VALUES (?, ?, ?)`,
		tokenHash, login, format(expiresAt)); err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// Session returns the GitHub login of the session with the token hash, or
// ErrNotFound if there is none or it has expired.
func (s *Store) Session(ctx context.Context, tokenHash []byte, now time.Time) (string, error) {
	var login string
	err := s.db.QueryRowContext(ctx,
		`SELECT login FROM sessions WHERE token_hash = ? AND expires_at > ?`,
		tokenHash, format(now)).Scan(&login)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("look up session: %w", err)
	}
	return login, nil
}

// DeleteSession ends a session. Ending one that does not exist is not an
// error.
func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}
