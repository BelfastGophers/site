package web

import (
	"errors"
	"net/http"
	"strconv"

	"uradical.io/go/binder"

	"belfastgophers.com/site/api/rest"
	"belfastgophers.com/site/store"
)

// errNoEvent is the 404 for an event that does not exist.
var errNoEvent = rest.Status(http.StatusNotFound, errors.New("there is no such event"))

// Organisers shows every event, latest first, each linking to its raffle.
func (s *Site) Organisers() rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		events, err := s.store.Events(r.Context())
		if err != nil {
			return err
		}
		return render(w, r, http.StatusOK, "organisers", "Organisers", events)
	}
}

// draw is an event's raffle, as organisers see it.
type draw struct {
	Event store.Event
	// Code is the raffle's code, for organisers to show at the meet-up.
	Code    string
	Entries int
	// Winners are the latest drawn first.
	Winners []store.Entry
}

// Left is how many entries have still to win.
func (d draw) Left() int { return d.Entries - len(d.Winners) }

// EventRaffle shows an event's raffle: the code to show the room, how many
// have entered, who has won, and the button to draw.
func (s *Site) EventRaffle() rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		var req eventPath
		if err := binder.Bind(r, &req); err != nil {
			return errNoEvent
		}
		event, err := s.store.Event(r.Context(), req.ID)
		if errors.Is(err, store.ErrNotFound) {
			return errNoEvent
		}
		if err != nil {
			return err
		}
		code, err := s.store.RaffleCode(r.Context(), req.ID)
		if err != nil {
			return err
		}
		entries, err := s.store.EntryCount(r.Context(), req.ID)
		if err != nil {
			return err
		}
		winners, err := s.store.Winners(r.Context(), req.ID)
		if err != nil {
			return err
		}
		d := draw{Event: event, Code: code, Entries: entries, Winners: winners}
		return render(w, r, http.StatusOK, "draw", event.Title, d)
	}
}

// Draw draws a winner from an event's raffle, someone who has not already
// won, and shows the raffle again.
func (s *Site) Draw() rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		var req eventPath
		if err := binder.Bind(r, &req); err != nil {
			return errNoEvent
		}
		// Nobody left to draw shows on the raffle's page.
		if _, err := s.store.Draw(r.Context(), req.ID, s.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		http.Redirect(w, r, "/organisers/events/"+strconv.FormatInt(req.ID, 10), http.StatusSeeOther)
		return nil
	}
}

// Videos shows every talk on the YouTube channel, newest first.
func (s *Site) Videos() rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		videos, err := s.store.Videos(r.Context())
		if err != nil {
			return err
		}
		return render(w, r, http.StatusOK, "videos", "Videos", videos)
	}
}

// Proposals shows every talk proposal.
func (s *Site) Proposals() rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		proposals, err := s.store.Proposals(r.Context())
		if err != nil {
			return err
		}
		return render(w, r, http.StatusOK, "proposals", "Talk proposals", proposals)
	}
}
