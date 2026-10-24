// Package meetup reads a Meetup group's upcoming events from its public
// iCalendar feed, such as
// https://www.meetup.com/belfast-gophers/events/ical/, which needs no API
// key.
package meetup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	ical "github.com/arran4/golang-ical"

	"belfastgophers.com/site/store"
)

// maxBody bounds what is read of anything fetched from Meetup.
const maxBody = 1 << 20

// Events fetches the group's upcoming events from its feed. Events
// cancelled on Meetup are left out.
func Events(ctx context.Context, client *http.Client, feedURL string) ([]store.Event, error) {
	feed, err := get(ctx, client, feedURL)
	if err != nil {
		return nil, fmt.Errorf("meetup feed: %w", err)
	}
	cal, err := ical.ParseCalendar(bytes.NewReader(trimTrailing(feed)))
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

// trimTrailing drops the calendar's own properties that Meetup puts after
// its events: RFC 5545 has them first, and golang-ical refuses them
// anywhere else. The site uses none of them. A feed with no component is
// left as it is, for the parser to reject.
func trimTrailing(feed []byte) []byte {
	cut, read := -1, 0
	for line := range bytes.Lines(feed) {
		read += len(line)
		name := bytes.TrimRight(line, "\r\n")
		if bytes.HasPrefix(name, []byte("END:")) && string(name) != "END:VCALENDAR" {
			cut = read
		}
	}
	if cut < 0 {
		return feed
	}
	return append(feed[:cut:cut], "END:VCALENDAR\r\n"...)
}

// get fetches url from Meetup.
func get(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "BelfastGophers-site/1.0 (+https://belfastgophers.com)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("meetup answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return body, nil
}

// value is an event's property, or "" when it has none.
func value(ve *ical.VEvent, p ical.ComponentProperty) string {
	if prop := ve.GetProperty(p); prop != nil {
		return prop.Value
	}
	return ""
}
