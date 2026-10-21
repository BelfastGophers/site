package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Entry is one person's entry in an event's raffle.
type Entry struct {
	Name  string
	Email string
}

// Enter enters a person in an event's raffle. It reports false if that email
// address had already entered, which leaves the first entry as it was.
func (s *Store) Enter(ctx context.Context, eventID int64, name, email string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO raffle_entries (event_id, name, email) VALUES (?, ?, ?) ON CONFLICT (event_id, email) DO NOTHING`,
		eventID, name, email)
	if err != nil {
		return false, fmt.Errorf("enter raffle for event %d: %w", eventID, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// EntryCount returns how many people have entered an event's raffle.
func (s *Store) EntryCount(ctx context.Context, eventID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM raffle_entries WHERE event_id = ?`, eventID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count raffle entries for event %d: %w", eventID, err)
	}
	return n, nil
}

// Draw picks a winner at random from the entries in an event's raffle that
// have not already won, and marks them as drawn. It returns ErrNotFound when
// everyone has won, or nobody entered.
func (s *Store) Draw(ctx context.Context, eventID int64, now time.Time) (winner Entry, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Entry{}, fmt.Errorf("draw raffle for event %d: %w", eventID, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()

	var id int64
	err = tx.QueryRowContext(ctx,
		`SELECT id, name, email FROM raffle_entries WHERE event_id = ? AND drawn_at IS NULL ORDER BY random() LIMIT 1`,
		eventID).Scan(&id, &winner.Name, &winner.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, fmt.Errorf("draw raffle for event %d: %w", eventID, err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE raffle_entries SET drawn_at = ? WHERE id = ?`, format(now), id); err != nil {
		return Entry{}, fmt.Errorf("mark raffle winner for event %d: %w", eventID, err)
	}
	if err = tx.Commit(); err != nil {
		return Entry{}, fmt.Errorf("draw raffle for event %d: %w", eventID, err)
	}
	return winner, nil
}

// Winners returns an event's raffle winners, the latest drawn first.
func (s *Store) Winners(ctx context.Context, eventID int64) ([]Entry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, email FROM raffle_entries WHERE event_id = ? AND drawn_at IS NOT NULL ORDER BY drawn_at DESC, id DESC`,
		eventID)
	if err != nil {
		return nil, fmt.Errorf("query raffle winners for event %d: %w", eventID, err)
	}
	defer func() { _ = rows.Close() }() // rows.Err has the error that matters
	var winners []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Name, &e.Email); err != nil {
			return nil, fmt.Errorf("scan raffle winner: %w", err)
		}
		winners = append(winners, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query raffle winners for event %d: %w", eventID, err)
	}
	return winners, nil
}
