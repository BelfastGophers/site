package store

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// meetup is a Thursday evening meet-up's start; tests place events around it.
var meetup = time.Date(2026, 10, 29, 18, 30, 0, 0, time.UTC)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "site.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// createEvent adds a three-hour event, as SyncEvents would from Meetup.
func createEvent(t *testing.T, s *Store, title string, starts time.Time) int64 {
	t.Helper()
	res, err := s.db.ExecContext(t.Context(),
		`INSERT INTO events (meetup_id, title, url, starts_at, ends_at) VALUES (?, ?, ?, ?, ?)`,
		"meetup-"+title, title, "https://www.meetup.com/belfast-gophers/events/"+title, format(starts), format(starts.Add(3*time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOpenReopens(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "site.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	createEvent(t, s, "October", meetup)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	events, err := s.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Title != "October" {
		t.Errorf("events after reopening = %v, want the one created", events)
	}
}

func TestEventEndsAfterStart(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	backwards := []Event{{MeetupID: "1", Title: "Backwards", StartsAt: meetup, EndsAt: meetup}}
	if err := s.SyncEvents(t.Context(), backwards, meetup); err == nil {
		t.Error("synced an event that ends as it starts")
	}
}

func TestEvents(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	past := createEvent(t, s, "September", meetup.AddDate(0, -1, 0))
	now := createEvent(t, s, "October", meetup)
	next := createEvent(t, s, "November", meetup.AddDate(0, 1, 0))

	e, err := s.Event(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if e.Title != "October" || !e.StartsAt.Equal(meetup) || !e.EndsAt.Equal(meetup.Add(3*time.Hour)) {
		t.Errorf("Event = %+v, want October as created", e)
	}
	if _, err := s.Event(t.Context(), 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Event(999) error = %v, want ErrNotFound", err)
	}

	all, err := s.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(all); !slices.Equal(got, []int64{next, now, past}) {
		t.Errorf("Events = %v, want latest first", got)
	}

	tests := []struct {
		name string
		at   time.Time
		want int64
	}{
		{"before it starts", meetup.Add(-time.Minute), 0},
		{"as it starts", meetup, now},
		{"during", meetup.Add(2 * time.Hour), now},
		{"as it ends", meetup.Add(3 * time.Hour), 0},
	}
	for _, tt := range tests {
		e, err := s.CurrentEvent(t.Context(), tt.at)
		switch {
		case tt.want == 0 && !errors.Is(err, ErrNotFound):
			t.Errorf("CurrentEvent %s = %v, %v, want ErrNotFound", tt.name, e.ID, err)
		case tt.want != 0 && (err != nil || e.ID != tt.want):
			t.Errorf("CurrentEvent %s = %v, %v, want %d", tt.name, e.ID, err, tt.want)
		}
	}
}

func TestRaffle(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := t.Context()
	event := createEvent(t, s, "October", meetup)
	other := createEvent(t, s, "November", meetup.AddDate(0, 1, 0))

	for _, email := range []string{"a@example.com", "b@example.com"} {
		if entered, err := s.Enter(ctx, event, "Gopher", email); err != nil || !entered {
			t.Fatalf("Enter(%s) = %v, %v, want entered", email, entered, err)
		}
	}
	if entered, err := s.Enter(ctx, event, "Again", "a@example.com"); err != nil || entered {
		t.Errorf("second Enter = %v, %v, want not entered", entered, err)
	}
	if entered, err := s.Enter(ctx, other, "Gopher", "a@example.com"); err != nil || !entered {
		t.Errorf("Enter in another event = %v, %v, want entered", entered, err)
	}
	if n, err := s.EntryCount(ctx, event); err != nil || n != 2 {
		t.Errorf("EntryCount = %d, %v, want 2", n, err)
	}

	// Nobody is drawn twice, and the draw runs out.
	seen := map[string]bool{}
	for range 2 {
		w, err := s.Draw(ctx, event, meetup)
		if err != nil {
			t.Fatal(err)
		}
		if seen[w.Email] {
			t.Errorf("%s drawn twice", w.Email)
		}
		seen[w.Email] = true
	}
	if _, err := s.Draw(ctx, event, meetup); !errors.Is(err, ErrNotFound) {
		t.Errorf("Draw with everyone drawn: error = %v, want ErrNotFound", err)
	}
	winners, err := s.Winners(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	if len(winners) != 2 {
		t.Errorf("Winners = %v, want both", winners)
	}
	if w, err := s.Winners(ctx, other); err != nil || len(w) != 0 {
		t.Errorf("another event's Winners = %v, %v, want none", w, err)
	}
}

// TestPrune checks that an event's raffle entries are kept for a week after
// it ends, and no longer.
func TestPrune(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := t.Context()
	event := createEvent(t, s, "October", meetup)
	ended := meetup.Add(3 * time.Hour)
	if _, err := s.Enter(ctx, event, "Gopher", "a@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, []byte("old"), "organiser", ended); err != nil {
		t.Fatal(err)
	}

	entries, sessions, err := s.Prune(ctx, ended.Add(RaffleRetention))
	if err != nil {
		t.Fatal(err)
	}
	if entries != 0 {
		t.Errorf("pruned %d entries exactly a week after the event, want 0", entries)
	}
	if sessions != 1 {
		t.Errorf("pruned %d sessions, want the expired one", sessions)
	}

	if entries, _, err = s.Prune(ctx, ended.Add(RaffleRetention+time.Second)); err != nil || entries != 1 {
		t.Errorf("Prune after a week = %d, %v, want 1 entry pruned", entries, err)
	}
	if n, err := s.EntryCount(ctx, event); err != nil || n != 0 {
		t.Errorf("EntryCount after pruning = %d, %v, want 0", n, err)
	}
	// The event itself stays, for its place on talk proposals.
	if _, err := s.Event(ctx, event); err != nil {
		t.Errorf("event after pruning: %v", err)
	}
}

func TestProposals(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := t.Context()
	p := Proposal{
		Name: "Gopher", Email: "g@example.com", Title: "Generics", Description: "All of them",
		DurationMinutes: 30, Remote: true, SubmittedAt: meetup.AddDate(0, -2, 0),
	}
	if _, err := s.SubmitProposal(ctx, p); err != nil {
		t.Fatal(err)
	}
	later := Proposal{Name: "Other", Email: "o@example.com", Title: "Testing", Description: "Why", DurationMinutes: 15, SubmittedAt: meetup.AddDate(0, -1, 0)}
	if _, err := s.SubmitProposal(ctx, later); err != nil {
		t.Fatal(err)
	}

	got, err := s.Proposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Title != "Testing" || got[1].Title != "Generics" {
		t.Fatalf("Proposals = %+v, want Testing then Generics", got)
	}
	got[1].ID = 0
	if !got[1].SubmittedAt.Equal(p.SubmittedAt) {
		t.Errorf("submitted at %v, want %v", got[1].SubmittedAt, p.SubmittedAt)
	}
	got[1].SubmittedAt = p.SubmittedAt
	if got[1] != p {
		t.Errorf("proposal = %+v, want it as submitted, %+v", got[1], p)
	}
}

func TestSessions(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := t.Context()
	hash := []byte("hash")
	if err := s.CreateSession(ctx, hash, "gopher", meetup.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if login, err := s.Session(ctx, hash, meetup); err != nil || login != "gopher" {
		t.Errorf("Session = %q, %v, want gopher", login, err)
	}
	if _, err := s.Session(ctx, hash, meetup.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired Session error = %v, want ErrNotFound", err)
	}
	if _, err := s.Session(ctx, []byte("other"), meetup); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown Session error = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSession(ctx, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, hash, meetup); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted Session error = %v, want ErrNotFound", err)
	}
}

func ids(events []Event) []int64 {
	var out []int64
	for _, e := range events {
		out = append(out, e.ID)
	}
	return out
}

// TestSyncEvents checks that events follow Meetup's: added, updated in
// place, and deleted when cancelled before they start, but kept once they
// have started, when Meetup no longer lists them.
func TestSyncEvents(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := t.Context()
	oct := Event{MeetupID: "oct", Title: "October", URL: "https://meetup.example/oct", StartsAt: meetup, EndsAt: meetup.Add(3 * time.Hour)}
	nov := Event{MeetupID: "nov", Title: "November", URL: "https://meetup.example/nov", StartsAt: meetup.AddDate(0, 1, 0), EndsAt: meetup.AddDate(0, 1, 0).Add(3 * time.Hour)}
	before := meetup.Add(-24 * time.Hour)
	if err := s.SyncEvents(ctx, []Event{oct, nov}, before); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].MeetupID != "nov" || events[0].URL != nov.URL || !events[0].StartsAt.Equal(nov.StartsAt) {
		t.Fatalf("events = %+v, want November and October as synced", events)
	}
	octID := events[1].ID

	// Renamed on Meetup: updated, keeping its id and so its raffle.
	oct.Title = "October: generics"
	if err := s.SyncEvents(ctx, []Event{oct, nov}, before); err != nil {
		t.Fatal(err)
	}
	if e, err := s.Event(ctx, octID); err != nil || e.Title != "October: generics" {
		t.Errorf("after renaming: %+v, %v, want the new title on the same event", e, err)
	}

	// Once October has started Meetup stops listing it, and it is kept;
	// November, still to come, is cancelled.
	if err := s.SyncEvents(ctx, nil, meetup.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if events, err = s.Events(ctx); err != nil || len(events) != 1 || events[0].ID != octID {
		t.Errorf("after November was cancelled: %+v, %v, want October only", events, err)
	}
}

func TestNextEvent(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	if _, err := s.NextEvent(t.Context(), meetup); !errors.Is(err, ErrNotFound) {
		t.Errorf("with none scheduled: error = %v, want ErrNotFound", err)
	}
	oct := createEvent(t, s, "October", meetup)
	nov := createEvent(t, s, "November", meetup.AddDate(0, 1, 0))
	for at, want := range map[time.Time]int64{
		meetup.Add(-time.Hour):    oct, // before it
		meetup.Add(time.Hour):     oct, // under way
		meetup.Add(4 * time.Hour): nov,
	} {
		if e, err := s.NextEvent(t.Context(), at); err != nil || e.ID != want {
			t.Errorf("NextEvent at %v = %d, %v, want %d", at, e.ID, err, want)
		}
	}
}

// TestVideos checks videos are saved, updated by their YouTube id, kept once
// the feed no longer lists them, and returned newest first.
func TestVideos(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	ctx := t.Context()
	old := Video{YouTubeID: "old", Title: "Old talk", PublishedAt: meetup.AddDate(-1, 0, 0)}
	talk := Video{YouTubeID: "new", Title: "New talk", PublishedAt: meetup}
	if err := s.SaveVideos(ctx, []Video{old, talk}); err != nil {
		t.Fatal(err)
	}
	talk.Title = "New talk, retitled"
	if err := s.SaveVideos(ctx, []Video{talk}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Videos(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Title != "New talk, retitled" || got[1].YouTubeID != "old" || !got[1].PublishedAt.Equal(old.PublishedAt) {
		t.Errorf("videos = %+v, want the new talk retitled, then the old one kept", got)
	}
}
