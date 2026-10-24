package store

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// TestCovers checks a cover is saved, replaced when it changes, and goes
// with its event.
func TestCovers(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := t.Context()
	nov := createEvent(t, s, "November", meetup)
	if _, err := s.CoverSource(ctx, nov); !errors.Is(err, ErrNotFound) {
		t.Errorf("source with no cover: error = %v, want ErrNotFound", err)
	}
	if _, err := s.Cover(ctx, nov); !errors.Is(err, ErrNotFound) {
		t.Errorf("no cover: error = %v, want ErrNotFound", err)
	}

	for _, c := range []struct{ source, image string }{
		{"https://meetup.example/1.jpeg", "first"},
		{"https://meetup.example/2.jpeg", "second"}, // changed on Meetup
	} {
		if err := s.SaveCover(ctx, nov, c.source, []byte(c.image)); err != nil {
			t.Fatal(err)
		}
		if source, err := s.CoverSource(ctx, nov); err != nil || source != c.source {
			t.Errorf("source = %q, %v, want %q", source, err, c.source)
		}
		if image, err := s.Cover(ctx, nov); err != nil || !bytes.Equal(image, []byte(c.image)) {
			t.Errorf("cover = %q, %v, want %q", image, err, c.image)
		}
	}

	// Cancelled on Meetup: the event goes, and its cover with it.
	if err := s.SyncEvents(ctx, nil, meetup.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM event_covers`).Scan(&left); err != nil || left != 0 {
		t.Errorf("covers after the event was cancelled = %d, %v, want 0", left, err)
	}
}
