package meetup

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// serveFeed serves body as the feed, with status.
func serveFeed(t *testing.T, status int, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/calendar")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestEvents(t *testing.T) {
	t.Parallel()
	feed, err := os.ReadFile("testdata/feed.ics")
	if err != nil {
		t.Fatal(err)
	}
	events, err := Events(t.Context(), http.DefaultClient, serveFeed(t, http.StatusOK, feed))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want November only: December is cancelled", events)
	}
	e := events[0]
	// Half six in Belfast in November is half six UTC.
	if e.MeetupID != "event_311000001@meetup.com" ||
		e.Title != "November Meetup: Go 1.27 and a long title that wraps across more than one line" ||
		e.URL != "https://www.meetup.com/belfast-gophers/events/311000001/" ||
		!e.StartsAt.Equal(time.Date(2026, 11, 26, 18, 30, 0, 0, time.UTC)) ||
		!e.EndsAt.Equal(time.Date(2026, 11, 26, 21, 0, 0, 0, time.UTC)) {
		t.Errorf("event = %+v, want November as the feed has it", e)
	}
}

func TestEventsRefused(t *testing.T) {
	t.Parallel()
	if _, err := Events(t.Context(), http.DefaultClient, serveFeed(t, http.StatusServiceUnavailable, nil)); err == nil {
		t.Error("a feed Meetup did not serve gave no error")
	}
}

// A page that is not a feed must fail, not read as no events: SyncEvents
// would delete every event to come.
func TestEventsNotAFeed(t *testing.T) {
	t.Parallel()
	if _, err := Events(t.Context(), http.DefaultClient, serveFeed(t, http.StatusOK, []byte("<html>Meetup is down</html>"))); err == nil {
		t.Error("a page that is not a feed gave no error")
	}
}
