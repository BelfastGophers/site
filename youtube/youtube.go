// Package youtube reads a channel's latest videos from its public feed, such
// as https://www.youtube.com/feeds/videos.xml?channel_id=..., which needs no
// API key. The feed lists only the latest fifteen.
package youtube

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"time"

	"belfastgophers.com/site/store"
)

// maxFeed bounds what is read of the feed.
const maxFeed = 1 << 20

// feed is the part of the channel's Atom feed the site uses.
type feed struct {
	Entries []struct {
		VideoID   string    `xml:"http://www.youtube.com/xml/schemas/2015 videoId"`
		Title     string    `xml:"title"`
		Published time.Time `xml:"published"`
	} `xml:"entry"`
}

// Videos fetches the channel's latest videos from its feed.
func Videos(ctx context.Context, client *http.Client, feedURL string) ([]store.Video, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch youtube feed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch youtube feed: youtube answered %s", resp.Status)
	}
	var f feed
	if err := xml.NewDecoder(io.LimitReader(resp.Body, maxFeed)).Decode(&f); err != nil {
		return nil, fmt.Errorf("parse youtube feed: %w", err)
	}
	videos := make([]store.Video, len(f.Entries))
	for i, e := range f.Entries {
		videos[i] = store.Video{YouTubeID: e.VideoID, Title: e.Title, PublishedAt: e.Published}
	}
	return videos, nil
}
