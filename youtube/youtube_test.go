package youtube

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func serveFeed(t *testing.T, status int, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestVideos(t *testing.T) {
	t.Parallel()
	feed, err := os.ReadFile("testdata/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	videos, err := Videos(t.Context(), http.DefaultClient, serveFeed(t, http.StatusOK, feed))
	if err != nil {
		t.Fatal(err)
	}
	if len(videos) != 2 {
		t.Fatalf("videos = %+v, want both", videos)
	}
	v := videos[0]
	if v.YouTubeID != "ObDCVxsB46U" || v.Title != "Casting Ent by Simon Hewitt (Tyndyll)" ||
		!v.PublishedAt.Equal(time.Date(2023, 5, 3, 16, 20, 55, 0, time.UTC)) {
		t.Errorf("video = %+v, want Casting Ent as the feed has it", v)
	}
}

func TestVideosRefused(t *testing.T) {
	t.Parallel()
	if _, err := Videos(t.Context(), http.DefaultClient, serveFeed(t, http.StatusNotFound, nil)); err == nil {
		t.Error("a feed YouTube did not serve gave no error")
	}
}
