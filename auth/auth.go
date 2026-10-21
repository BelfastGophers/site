// Package auth signs organisers in with GitHub. Anyone in the GitHub
// organisation is an organiser: the site has no accounts of its own, and
// keeps nothing of GitHub's, not even the access token, once sign-in is
// done.
//
// Signing in follows GitHub's web application flow: Login sends the browser
// to GitHub with a random state, Callback checks the state, swaps the code
// GitHub returns for a token, uses the token to check membership, and starts
// a session. The session is a random token in a cookie; the database holds
// only its hash.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"uradical.io/go/binder"

	"belfastgophers.com/site/api/rest"
	"belfastgophers.com/site/api/rest/errlog"
	"belfastgophers.com/site/store"
)

const (
	// The cookies' names, which the request types' cookie tags repeat.
	sessionCookie = "session"
	stateCookie   = "oauth_state"

	// SessionLength is how long an organiser stays signed in.
	SessionLength = 12 * time.Hour
	// stateLength is how long a sign-in at GitHub may take.
	stateLength = 10 * time.Minute
)

// Config is how the site signs in with GitHub.
type Config struct {
	// ClientID and ClientSecret are the GitHub App's.
	ClientID, ClientSecret string

	// CallbackURL is where GitHub returns the browser, the site's
	// /auth/callback, which must match the app's callback URL.
	CallbackURL string

	// Org is the GitHub organisation whose members are organisers.
	Org string

	// Client makes the calls to GitHub.
	Client *http.Client

	// GitHub's addresses. Empty, they are github.com's; tests set them to a
	// fake.
	AuthorizeURL, TokenURL, APIURL string
}

// Auth signs organisers in and out, and guards their pages.
type Auth struct {
	cfg   Config
	store *store.Store
	now   func() time.Time
}

// New returns an Auth that keeps sessions in st.
func New(cfg Config, st *store.Store) *Auth {
	if cfg.AuthorizeURL == "" {
		cfg.AuthorizeURL = "https://github.com/login/oauth/authorize"
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = "https://github.com/login/oauth/access_token"
	}
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.github.com"
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Auth{cfg: cfg, store: st, now: time.Now}
}

// Login sends the browser to GitHub to sign in.
func (a *Auth) Login() rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		state := rand.Text()
		http.SetCookie(w, a.cookie(stateCookie, state, int(stateLength.Seconds())))
		q := url.Values{
			"client_id":    {a.cfg.ClientID},
			"redirect_uri": {a.cfg.CallbackURL},
			"scope":        {"read:org"},
			"state":        {state},
		}
		http.Redirect(w, r, a.cfg.AuthorizeURL+"?"+q.Encode(), http.StatusSeeOther)
		return nil
	}
}

// callbackRequest is GitHub returning the browser to the site.
type callbackRequest struct {
	State string `query:"state"`
	Code  string `query:"code"`
	// Error is set when the organiser declined at GitHub.
	Error string `query:"error"`
	// LoginState is the state Login sent, from its cookie, stateCookie.
	LoginState string `cookie:"oauth_state"`
}

// Callback finishes signing in when GitHub returns the browser. A GitHub
// user outside the organisation is shown denied, with 403.
func (a *Auth) Callback(denied http.Handler) rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		var req callbackRequest
		if err := binder.Bind(r, &req); err != nil {
			return err
		}
		http.SetCookie(w, a.cookie(stateCookie, "", -1))
		if req.LoginState == "" || subtle.ConstantTimeCompare([]byte(req.LoginState), []byte(req.State)) != 1 {
			return rest.Status(http.StatusBadRequest, errors.New("sign-in expired or was started elsewhere: please sign in again"))
		}
		if req.Error != "" {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return nil
		}

		token, err := a.exchange(r.Context(), req.Code)
		if err != nil {
			return err
		}
		login, member, err := a.membership(r.Context(), token)
		if err != nil {
			return err
		}
		if !member {
			slog.InfoContext(r.Context(), "sign-in refused: not in the organisation", "login", login)
			denied.ServeHTTP(w, r)
			return nil
		}

		session := rand.Text()
		if err := a.store.CreateSession(r.Context(), hash(session), login, a.now().Add(SessionLength)); err != nil {
			return err
		}
		http.SetCookie(w, a.cookie(sessionCookie, session, int(SessionLength.Seconds())))
		slog.InfoContext(r.Context(), "organiser signed in", "login", login)
		http.Redirect(w, r, "/organisers", http.StatusSeeOther)
		return nil
	}
}

// sessionRequest is a request from an organiser's browser.
type sessionRequest struct {
	// Session is the session's token, from its cookie, sessionCookie.
	Session string `cookie:"session"`
}

// Logout ends the organiser's session, if there is one.
func (a *Auth) Logout() rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		var req sessionRequest
		if err := binder.Bind(r, &req); err != nil {
			return err
		}
		if req.Session != "" {
			if err := a.store.DeleteSession(r.Context(), hash(req.Session)); err != nil {
				return err
			}
		}
		http.SetCookie(w, a.cookie(sessionCookie, "", -1))
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return nil
	}
}

// RequireOrganiser lets a request through only with an organiser's session,
// and puts their GitHub login on its context. Anyone else is shown
// unauthenticated, which must answer 401.
func (a *Auth) RequireOrganiser(unauthenticated http.Handler) rest.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req sessionRequest
			if err := binder.Bind(r, &req); err != nil || req.Session == "" {
				unauthenticated.ServeHTTP(w, r)
				return
			}
			login, err := a.store.Session(r.Context(), hash(req.Session), a.now())
			if errors.Is(err, store.ErrNotFound) {
				unauthenticated.ServeHTTP(w, r)
				return
			}
			if err != nil {
				slog.ErrorContext(r.Context(), "looking up session", "err", errlog.Value(err))
				http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), loginKey{}, login)))
		})
	}
}

type loginKey struct{}

// LoginFrom returns the GitHub login of the organiser RequireOrganiser let
// through, or "" outside their pages.
func LoginFrom(ctx context.Context) string {
	login, _ := ctx.Value(loginKey{}).(string)
	return login
}

// cookie returns one of the site's cookies: sent only over HTTPS, never to
// scripts, and not on requests from other sites except following a link, so
// that GitHub's redirect back still carries the state. Browsers count
// http://localhost, and any name under it, as secure, so the site still
// signs in locally. maxAge below zero deletes it.
func (a *Auth) cookie(name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
}

// hash is what the database keeps of a session token.
func hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}
