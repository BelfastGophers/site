package web

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"

	"uradical.io/go/binder"

	"belfastgophers.com/site/api/rest"
	"belfastgophers.com/site/store"
)

// Field limits, so a form cannot fill the database.
const (
	maxName        = 100
	maxEmail       = 254
	maxTitle       = 200
	maxDescription = 5000
	maxDuration    = 180
)

// Home is the home page, with the next meet-up and a link to sign up for it
// on Meetup, or a note that none is scheduled.
func (s *Site) Home() rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		next, err := s.store.NextEvent(r.Context(), s.now())
		if errors.Is(err, store.ErrNotFound) {
			return render(w, r, http.StatusOK, "home", "", nil)
		}
		if err != nil {
			return err
		}
		return render(w, r, http.StatusOK, "home", "", next)
	}
}

// proposalRequest is the talk proposal form. Fields left empty bind as
// nothing, for Validate to report.
type proposalRequest struct {
	Name        string `body:"name"`
	Email       string `body:"email"`
	Title       string `body:"title"`
	Description string `body:"description"`
	Duration    int    `body:"duration,omitempty"`
	Remote      *bool  `body:"remote,omitempty"`
}

func (p proposalRequest) Validate(context.Context) error {
	invalid := fieldErrors{}
	invalid.text("name", p.Name, maxName)
	invalid.text("title", p.Title, maxTitle)
	invalid.text("description", p.Description, maxDescription)
	if !isEmail(p.Email) {
		invalid["email"] = "Please give an email address we can reach you at."
	}
	if p.Duration < 1 || p.Duration > maxDuration {
		invalid["duration"] = "Please give a rough length, in minutes, up to " + strconv.Itoa(maxDuration) + "."
	}
	if p.Remote == nil {
		invalid["remote"] = "Please say whether you would speak remotely."
	}
	return invalid.err()
}

// RemoteIs reports whether the form answered remote with v, to show it
// again.
func (p proposalRequest) RemoteIs(v bool) bool { return p.Remote != nil && *p.Remote == v }

// proposalPage is the talk proposal form, as filled in, with what is wrong
// with it.
type proposalPage struct {
	Form   proposalRequest
	Errors fieldErrors
}

// Speak shows the talk proposal form.
func (s *Site) Speak() rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		return render(w, r, http.StatusOK, "speak", "Speak", proposalPage{})
	}
}

// Propose saves a talk proposal, or shows the form again, with 422, saying
// what is wrong.
func (s *Site) Propose() rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		var req proposalRequest
		err := binder.Bind(r, &req)
		var invalid fieldErrors
		if errors.As(err, &invalid) {
			return render(w, r, http.StatusUnprocessableEntity, "speak", "Speak", proposalPage{Form: req, Errors: invalid})
		}
		if err != nil {
			return err
		}

		p := store.Proposal{
			Name:            strings.TrimSpace(req.Name),
			Email:           strings.ToLower(req.Email),
			Title:           strings.TrimSpace(req.Title),
			Description:     strings.TrimSpace(req.Description),
			DurationMinutes: req.Duration,
			Remote:          *req.Remote,
			SubmittedAt:     s.now(),
		}
		if _, err := s.store.SubmitProposal(r.Context(), p); err != nil {
			return err
		}
		return render(w, r, http.StatusOK, "proposed", "Thank you", p.Title)
	}
}

// isEmail reports whether s is an email address alone: mail.ParseAddress
// also takes "Name <address>".
func isEmail(s string) bool {
	addr, err := mail.ParseAddress(s)
	return err == nil && len(s) <= maxEmail && addr.Address == s
}

// raffleRequest is the raffle entry form.
type raffleRequest struct {
	Name  string `body:"name"`
	Email string `body:"email"`
}

func (e raffleRequest) Validate(context.Context) error {
	invalid := fieldErrors{}
	invalid.text("name", e.Name, maxName)
	if !isEmail(e.Email) {
		invalid["email"] = "Please give an email address, so we can reach you if you win."
	}
	return invalid.err()
}

// rafflePage is the raffle entry form, as filled in, with what is wrong with
// it.
type rafflePage struct {
	Form   raffleRequest
	Errors fieldErrors
}

// Raffle shows the entry form for the raffle of the event under way, or
// says there is none.
func (s *Site) Raffle() rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		_, err := s.store.CurrentEvent(r.Context(), s.now())
		if errors.Is(err, store.ErrNotFound) {
			return render(w, r, http.StatusOK, "noraffle", "Raffle", nil)
		}
		if err != nil {
			return err
		}
		return render(w, r, http.StatusOK, "raffle", "Raffle", rafflePage{})
	}
}

// EnterRaffle enters someone in the raffle of the event under way. Email
// addresses are compared in lower case, so one person has one entry.
func (s *Site) EnterRaffle() rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		event, err := s.store.CurrentEvent(r.Context(), s.now())
		if errors.Is(err, store.ErrNotFound) {
			return render(w, r, http.StatusConflict, "noraffle", "Raffle", nil)
		}
		if err != nil {
			return err
		}
		var req raffleRequest
		err = binder.Bind(r, &req)
		var invalid fieldErrors
		if errors.As(err, &invalid) {
			return render(w, r, http.StatusUnprocessableEntity, "raffle", "Raffle", rafflePage{Form: req, Errors: invalid})
		}
		if err != nil {
			return err
		}

		entered, err := s.store.Enter(r.Context(), event.ID, strings.TrimSpace(req.Name), strings.ToLower(req.Email))
		if err != nil {
			return err
		}
		return render(w, r, http.StatusOK, "goodluck", "Good luck", entered)
	}
}
