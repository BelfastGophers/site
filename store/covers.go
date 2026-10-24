package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CoverSource returns the address on Meetup of the cover saved for an
// event, or ErrNotFound when it has none.
func (s *Store) CoverSource(ctx context.Context, eventID int64) (string, error) {
	var source string
	err := s.db.QueryRowContext(ctx, `SELECT source FROM event_covers WHERE event_id = ?`, eventID).Scan(&source)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("cover source: %w", err)
	}
	return source, nil
}

// Cover returns an event's cover image, or ErrNotFound when it has none.
func (s *Store) Cover(ctx context.Context, eventID int64) ([]byte, error) {
	var image []byte
	err := s.db.QueryRowContext(ctx, `SELECT image FROM event_covers WHERE event_id = ?`, eventID).Scan(&image)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("cover: %w", err)
	}
	return image, nil
}

// SaveCover sets an event's cover, downloaded from source, in place of any
// it had.
func (s *Store) SaveCover(ctx context.Context, eventID int64, source string, image []byte) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO event_covers (event_id, source, image) VALUES (?, ?, ?)
		 ON CONFLICT (event_id) DO UPDATE SET source = excluded.source, image = excluded.image`,
		eventID, source, image); err != nil {
		return fmt.Errorf("save cover: %w", err)
	}
	return nil
}
