// Package store keeps the site's data in SQLite: events and their raffles,
// talk proposals, and organisers' sessions.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed schema.sql
var schema string

// ErrNotFound is returned when there is no row to return.
var ErrNotFound = errors.New("not found")

// RaffleRetention is how long after an event ends its raffle entries are
// kept, so that a prize can still be sorted out with the winner.
const RaffleRetention = 7 * 24 * time.Hour

// Store is the site's database.
type Store struct {
	db *sql.DB
}

// Open opens the database at path, creating it and its tables if they do not
// exist.
func Open(ctx context.Context, path string) (*Store, error) {
	// Foreign keys are off in SQLite unless each connection turns them on.
	// The busy timeout makes a write wait for another rather than fail.
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection: SQLite takes one writer at a time anyway, and a site
	// this size never waits on it.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, errors.Join(fmt.Errorf("create tables in %s: %w", path, err), db.Close())
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database can be reached, for the readiness check.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Prune deletes the raffle entries of events that ended more than
// RaffleRetention before now, and expired sessions.
func (s *Store) Prune(ctx context.Context, now time.Time) (entries, sessions int64, err error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM raffle_entries WHERE event_id IN (SELECT id FROM events WHERE ends_at < ?)`,
		format(now.Add(-RaffleRetention)))
	if err != nil {
		return 0, 0, fmt.Errorf("prune raffle entries: %w", err)
	}
	if entries, err = res.RowsAffected(); err != nil {
		return 0, 0, err
	}
	res, err = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, format(now))
	if err != nil {
		return 0, 0, fmt.Errorf("prune sessions: %w", err)
	}
	if sessions, err = res.RowsAffected(); err != nil {
		return 0, 0, err
	}
	return entries, sessions, nil
}

// format writes t as the schema stores times.
func format(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// parse reads a time the schema stored.
func parse(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("stored time %q: %w", s, err)
	}
	return t, nil
}
