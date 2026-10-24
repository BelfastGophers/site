package meetup

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"belfastgophers.com/site/store"
)

// jpeg is enough of a JPEG for http.DetectContentType.
var jpeg = []byte("\xff\xd8\xff\xe0 a cover")

// fakeMeetup serves an event page whose og:image is cover, and the images
// in images, counting the images it serves.
type fakeMeetup struct {
	cover  string
	images map[string][]byte
	served atomic.Int32
}

func (m *fakeMeetup) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/event" {
		// As Meetup writes it.
		_, _ = w.Write([]byte(`<html><head><meta property="og:title" content="November"/>` +
			`<meta property="og:image" content="` + m.cover + `" data-next-head=""/></head><body></body></html>`))
		return
	}
	image, ok := m.images[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	m.served.Add(1)
	_, _ = w.Write(image)
}

// TestSyncCover checks the next event's cover is saved, downloaded again only
// when Meetup's changes, and that a page with none, or a cover that is not
// an image, saves nothing.
func TestSyncCover(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "site.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m := &fakeMeetup{images: map[string][]byte{"/1.jpeg": jpeg, "/2.jpeg": append(jpeg, '2'), "/page.jpeg": []byte("<html>")}}
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	// Nothing scheduled: nothing to do.
	if err := SyncCover(ctx, srv.Client(), st, now); err != nil {
		t.Fatalf("with no event: %v", err)
	}

	starts := now.AddDate(0, 0, 25)
	nov := store.Event{MeetupID: "nov", Title: "November", URL: srv.URL + "/event", StartsAt: starts, EndsAt: starts.Add(2 * time.Hour)}
	if err := st.SyncEvents(ctx, []store.Event{nov}, now); err != nil {
		t.Fatal(err)
	}
	next, err := st.NextEvent(ctx, now)
	if err != nil {
		t.Fatal(err)
	}

	// No og:image on the page: no cover, and no error.
	if err := SyncCover(ctx, srv.Client(), st, now); err != nil {
		t.Errorf("page with no cover: %v", err)
	}
	if _, err := st.Cover(ctx, next.ID); err == nil {
		t.Error("saved a cover from a page with none")
	}

	m.cover = srv.URL + "/page.jpeg"
	if err := SyncCover(ctx, srv.Client(), st, now); err == nil || !strings.Contains(err.Error(), "not an image") {
		t.Errorf("cover that is a page: error = %v, want not an image", err)
	}

	m.cover = srv.URL + "/1.jpeg"
	for range 2 {
		if err := SyncCover(ctx, srv.Client(), st, now); err != nil {
			t.Fatal(err)
		}
	}
	if image, err := st.Cover(ctx, next.ID); err != nil || !bytes.Equal(image, jpeg) {
		t.Errorf("cover = %q, %v, want the first", image, err)
	}
	if n := m.served.Load(); n != 2 {
		t.Errorf("images downloaded = %d, want 2: the page, and the cover once", n)
	}

	// Changed on Meetup.
	m.cover = srv.URL + "/2.jpeg"
	if err := SyncCover(ctx, srv.Client(), st, now); err != nil {
		t.Fatal(err)
	}
	if image, err := st.Cover(ctx, next.ID); err != nil || !bytes.Equal(image, m.images["/2.jpeg"]) {
		t.Errorf("cover = %q, %v, want the second", image, err)
	}
}

// TestCoverRefused checks an event page Meetup did not serve is an error.
func TestCoverRefused(t *testing.T) {
	t.Parallel()
	if _, err := Cover(t.Context(), http.DefaultClient, serveFeed(t, http.StatusNotFound, nil)); err == nil {
		t.Error("an event page Meetup did not serve gave no error")
	}
}
