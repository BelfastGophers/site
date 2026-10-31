package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
)

// Entry is one person's entry in an event's raffle.
type Entry struct {
	Name  string
	Email string
}

// Enter enters a person in an event's raffle. It reports false if that email
// address had already entered, or one that reaches the same inbox, such as
// a+raffle@example.com for a@example.com, which leaves the first entry as it
// was.
func (s *Store) Enter(ctx context.Context, eventID int64, name, email string) (entered bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("enter raffle for event %d: %w", eventID, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()

	emails, err := entrants(ctx, tx, eventID)
	if err != nil {
		return false, err
	}
	if slices.Contains(emails, Inbox(email)) {
		return false, tx.Rollback()
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO raffle_entries (event_id, name, email) VALUES (?, ?, ?)`,
		eventID, name, email); err != nil {
		return false, fmt.Errorf("enter raffle for event %d: %w", eventID, err)
	}
	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("enter raffle for event %d: %w", eventID, err)
	}
	return true, nil
}

// entrants returns the inbox of each of an event's raffle entries.
func entrants(ctx context.Context, tx *sql.Tx, eventID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT email FROM raffle_entries WHERE event_id = ?`, eventID)
	if err != nil {
		return nil, fmt.Errorf("query raffle entries for event %d: %w", eventID, err)
	}
	defer func() { _ = rows.Close() }() // rows.Err has the error that matters
	var inboxes []string
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, fmt.Errorf("scan raffle entry: %w", err)
		}
		inboxes = append(inboxes, Inbox(email))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query raffle entries for event %d: %w", eventID, err)
	}
	return inboxes, nil
}

// Inbox is the mailbox an email address delivers to, as far as the raffle
// can tell: in lower case, without a +tag, which most mail services ignore,
// and for Gmail without dots, which it ignores too. It is only for telling
// whether two entries are one person's; the address is kept as entered.
func Inbox(email string) string {
	email = strings.ToLower(email)
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return email
	}
	local, domain := email[:at], email[at+1:]
	local, _, _ = strings.Cut(local, "+")
	if domain == "gmail.com" || domain == "googlemail.com" {
		local, domain = strings.ReplaceAll(local, ".", ""), "gmail.com"
	}
	return local + "@" + domain
}

// RaffleCode returns an event's raffle code, six digits, making it the first
// time it is asked for.
func (s *Store) RaffleCode(ctx context.Context, eventID int64) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO raffle_codes (event_id, code) VALUES (?, ?) ON CONFLICT (event_id) DO NOTHING`,
		eventID, fmt.Sprintf("%06d", n)); err != nil {
		return "", fmt.Errorf("make raffle code for event %d: %w", eventID, err)
	}
	var code string
	if err := s.db.QueryRowContext(ctx, `SELECT code FROM raffle_codes WHERE event_id = ?`, eventID).Scan(&code); err != nil {
		return "", fmt.Errorf("raffle code for event %d: %w", eventID, err)
	}
	return code, nil
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
