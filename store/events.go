package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Event is one meet-up, as Meetup lists it.
type Event struct {
	ID int64
	// MeetupID is Meetup's own id for the event, which SyncEvents matches
	// on.
	MeetupID string
	Title    string
	// URL is the event's page on Meetup, where people sign up.
	URL      string
	StartsAt time.Time
	EndsAt   time.Time
}

const eventColumns = `id, meetup_id, title, url, starts_at, ends_at`

// SyncEvents makes the events match Meetup's upcoming ones: each is added,
// or updated by its Meetup id, and an event yet to start that Meetup no
// longer lists, because it was cancelled, is deleted with its raffle. Events that have started are kept, since Meetup
// lists only those to come.
func (s *Store) SyncEvents(ctx context.Context, events []Event, now time.Time) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync events: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()

	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = e.MeetupID
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO events (meetup_id, title, url, starts_at, ends_at) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (meetup_id) DO UPDATE SET
			   title = excluded.title, url = excluded.url, starts_at = excluded.starts_at, ends_at = excluded.ends_at`,
			e.MeetupID, e.Title, e.URL, format(e.StartsAt), format(e.EndsAt)); err != nil {
			return fmt.Errorf("sync event %s: %w", e.MeetupID, err)
		}
	}
	listed, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx,
		`DELETE FROM events WHERE starts_at > ? AND meetup_id NOT IN (SELECT value FROM json_each(?))`,
		format(now), string(listed)); err != nil {
		return fmt.Errorf("delete cancelled events: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync events: %w", err)
	}
	return nil
}

// Event returns one event, or ErrNotFound.
func (s *Store) Event(ctx context.Context, id int64) (Event, error) {
	return s.event(ctx, `SELECT `+eventColumns+` FROM events WHERE id = ?`, id)
}

// Events returns every event, latest first.
func (s *Store) Events(ctx context.Context) ([]Event, error) {
	return s.events(ctx, `SELECT `+eventColumns+` FROM events ORDER BY starts_at DESC`)
}

// NextEvent returns the event under way at now, or else the next to start,
// or ErrNotFound when none is scheduled.
func (s *Store) NextEvent(ctx context.Context, now time.Time) (Event, error) {
	return s.event(ctx, `SELECT `+eventColumns+` FROM events WHERE ends_at > ? ORDER BY starts_at LIMIT 1`, format(now))
}

// CurrentEvent returns the event under way at now, whose raffle is open, or
// ErrNotFound. Of two that overlap, it is the one ending first.
func (s *Store) CurrentEvent(ctx context.Context, now time.Time) (Event, error) {
	return s.event(ctx,
		`SELECT `+eventColumns+` FROM events WHERE starts_at <= ? AND ends_at > ? ORDER BY ends_at LIMIT 1`,
		format(now), format(now))
}

func (s *Store) event(ctx context.Context, query string, args ...any) (Event, error) {
	events, err := s.events(ctx, query, args...)
	if err != nil {
		return Event{}, err
	}
	if len(events) == 0 {
		return Event{}, ErrNotFound
	}
	return events[0], nil
}

func (s *Store) events(ctx context.Context, query string, args ...any) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err has the error that matters
	var events []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	return events, nil
}

func scanEvent(rows *sql.Rows) (Event, error) {
	var e Event
	var starts, ends string
	if err := rows.Scan(&e.ID, &e.MeetupID, &e.Title, &e.URL, &starts, &ends); err != nil {
		return Event{}, fmt.Errorf("scan event: %w", err)
	}
	var errStart, errEnd error
	e.StartsAt, errStart = parse(starts)
	e.EndsAt, errEnd = parse(ends)
	if err := errors.Join(errStart, errEnd); err != nil {
		return Event{}, err
	}
	return e, nil
}
