// Package meetup reads a Meetup group's upcoming events from its public
// iCalendar feed, such as
// https://www.meetup.com/belfast-gophers/events/ical/, which needs no API
// key.
package meetup

import (
	"context"
	"fmt"
	"io"
	"net/http"

	ical "github.com/arran4/golang-ical"

	"belfastgophers.com/site/store"
)

// maxFeed bounds what is read of the feed.
const maxFeed = 1 << 20

// Events fetches the group's upcoming events from its feed. Events
// cancelled on Meetup are left out.
func Events(ctx context.Context, client *http.Client, feedURL string) ([]store.Event, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "BelfastGophers-site/1.0 (+https://belfastgophers.com)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch meetup feed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch meetup feed: meetup answered %s", resp.Status)
	}
	cal, err := ical.ParseCalendar(io.LimitReader(resp.Body, maxFeed))
	if err != nil {
		return nil, fmt.Errorf("parse meetup feed: %w", err)
	}

	var events []store.Event
	for _, ve := range cal.Events() {
		if value(ve, ical.ComponentPropertyStatus) == "CANCELLED" {
			continue
		}
		starts, err := ve.GetStartAt()
		if err != nil {
			return nil, fmt.Errorf("meetup event %s: %w", ve.Id(), err)
		}
		ends, err := ve.GetEndAt()
		if err != nil {
			return nil, fmt.Errorf("meetup event %s: %w", ve.Id(), err)
		}
		events = append(events, store.Event{
			MeetupID: ve.Id(),
			Title:    value(ve, ical.ComponentPropertySummary),
			URL:      value(ve, ical.ComponentPropertyUrl),
			StartsAt: starts,
			EndsAt:   ends,
		})
	}
	return events, nil
}

// value is an event's property, or "" when it has none.
func value(ve *ical.VEvent, p ical.ComponentProperty) string {
	if prop := ve.GetProperty(p); prop != nil {
		return prop.Value
	}
	return ""
}
