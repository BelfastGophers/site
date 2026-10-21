package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"belfastgophers.com/site/api/rest"
	"belfastgophers.com/site/store"
)

// fakeGitHub answers the calls signing in makes, for one user, who is a
// member of the organisation when member is set.
type fakeGitHub struct {
	member bool
	// refuse makes the token exchange answer with an error, as GitHub does
	// for an expired code.
	refuse bool
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/token":
		if r.PostFormValue("client_secret") != "secret" || r.PostFormValue("code") != "code" || f.refuse {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "bad_verification_code"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "token"})
	case "/user":
		if r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"login": "gopher"})
	case "/user/memberships/orgs/BelfastGophers":
		if !f.member {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"state": "active"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// newAuth returns an Auth signing in through gh, with a database of the
// test's own.
func newAuth(t *testing.T, gh *fakeGitHub) *Auth {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "site.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	return New(Config{
		ClientID: "client", ClientSecret: "secret", Org: "BelfastGophers",
		CallbackURL:  "https://site.example/auth/callback",
		AuthorizeURL: srv.URL + "/authorize", TokenURL: srv.URL + "/token", APIURL: srv.URL,
		Client: srv.Client(),
	}, st)
}

// serve sends req to h, with the cookies given.
func serve(t *testing.T, h rest.HandlerFunc, req *http.Request, cookies ...*http.Cookie) (*httptest.ResponseRecorder, error) {
	t.Helper()
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	return rec, h(rec, req)
}

// cookieNamed returns the cookie the response set with name, or nil.
func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// login starts signing in and returns the state cookie set.
func login(t *testing.T, a *Auth) *http.Cookie {
	t.Helper()
	rec, err := serve(t, a.Login(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/login", nil))
	if err != nil {
		t.Fatal(err)
	}
	state := cookieNamed(rec, stateCookie)
	if state == nil || state.Value == "" {
		t.Fatal("login set no state cookie")
	}
	if !state.Secure || !state.HttpOnly || state.SameSite != http.SameSiteLaxMode {
		t.Errorf("state cookie %+v: want Secure, HttpOnly and SameSite=Lax", state)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if rec.Code != http.StatusSeeOther || q.Get("state") != state.Value || q.Get("client_id") != "client" ||
		q.Get("redirect_uri") != "https://site.example/auth/callback" {
		t.Errorf("login redirected %d to %s, want GitHub with the state %s", rec.Code, loc, state.Value)
	}
	return state
}

// callback returns the browser from GitHub with state and code.
func callback(t *testing.T, a *Auth, state *http.Cookie, query string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	denied := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/callback?"+query, nil)
	return serve(t, a.Callback(denied), req, state)
}

// guarded returns the login RequireOrganiser lets through with the cookie,
// and the status it answered.
func guarded(t *testing.T, a *Auth, cookies ...*http.Cookie) (string, int) {
	t.Helper()
	var login string
	unauthenticated := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	h := a.RequireOrganiser(unauthenticated)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		login = LoginFrom(r.Context())
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/organisers", nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return login, rec.Code
}

// TestSignIn follows a member through signing in, using their session, and
// signing out.
func TestSignIn(t *testing.T) {
	t.Parallel()
	a := newAuth(t, &fakeGitHub{member: true})
	state := login(t, a)

	rec, err := callback(t, a, state, "code=code&state="+state.Value)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/organisers" {
		t.Errorf("callback answered %d to %q, want a redirect to /organisers", rec.Code, rec.Header().Get("Location"))
	}
	if c := cookieNamed(rec, stateCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("state cookie %+v: want it deleted", c)
	}
	session := cookieNamed(rec, sessionCookie)
	if session == nil || session.Value == "" || !session.Secure || !session.HttpOnly {
		t.Fatalf("session cookie %+v: want one, Secure and HttpOnly", session)
	}

	if login, code := guarded(t, a, session); code != http.StatusOK || login != "gopher" {
		t.Errorf("with the session: %d, login %q, want 200 as gopher", code, login)
	}
	if _, code := guarded(t, a); code != http.StatusUnauthorized {
		t.Errorf("without a session: %d, want 401", code)
	}
	if _, code := guarded(t, a, a.cookie(sessionCookie, "forged", 60)); code != http.StatusUnauthorized {
		t.Errorf("with a forged session: %d, want 401", code)
	}

	// The session lasts as long as it should.
	a.now = func() time.Time { return time.Now().Add(SessionLength + time.Minute) }
	if _, code := guarded(t, a, session); code != http.StatusUnauthorized {
		t.Errorf("with an expired session: %d, want 401", code)
	}
	a.now = time.Now

	rec, err = serve(t, a.Logout(), httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/auth/logout", nil), session)
	if err != nil {
		t.Fatal(err)
	}
	if c := cookieNamed(rec, sessionCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("session cookie after logout %+v: want it deleted", c)
	}
	if _, code := guarded(t, a, session); code != http.StatusUnauthorized {
		t.Errorf("after logout, the old session: %d, want 401", code)
	}
}

// TestSignInNotMember checks that a GitHub user outside the organisation is
// shown denied, and gets no session.
func TestSignInNotMember(t *testing.T) {
	t.Parallel()
	a := newAuth(t, &fakeGitHub{member: false})
	state := login(t, a)
	rec, err := callback(t, a, state, "code=code&state="+state.Value)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want the denied page's 403", rec.Code)
	}
	if c := cookieNamed(rec, sessionCookie); c != nil {
		t.Errorf("set session cookie %+v for a non-member", c)
	}
}

// TestCallbackRefused checks the ways a return from GitHub is refused
// without a session.
func TestCallbackRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		gh     fakeGitHub
		query  func(state string) string
		status int // the rest.Status code, or 0 for an error the site logs
	}{
		{"state from elsewhere", fakeGitHub{member: true}, func(string) string { return "code=code&state=other" }, http.StatusBadRequest},
		{"no state", fakeGitHub{member: true}, func(string) string { return "code=code" }, http.StatusBadRequest},
		{"code refused", fakeGitHub{member: true, refuse: true}, func(s string) string { return "code=code&state=" + s }, 0},
		{"no code", fakeGitHub{member: true}, func(s string) string { return "state=" + s }, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a := newAuth(t, &tt.gh)
			state := login(t, a)
			rec, err := callback(t, a, state, tt.query(state.Value))
			if err == nil {
				t.Fatalf("callback succeeded with %d", rec.Code)
			}
			var se rest.StatusError
			got := 0
			if errors.As(err, &se) {
				got = se.HTTPStatus()
			}
			if got != tt.status {
				t.Errorf("error %v has status %d, want %d", err, got, tt.status)
			}
			if c := cookieNamed(rec, sessionCookie); c != nil {
				t.Errorf("set session cookie %+v", c)
			}
		})
	}
}

// TestCallbackDeclined checks that an organiser who declines at GitHub is
// sent home.
func TestCallbackDeclined(t *testing.T) {
	t.Parallel()
	a := newAuth(t, &fakeGitHub{member: true})
	state := login(t, a)
	rec, err := callback(t, a, state, "error=access_denied&state="+state.Value)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Errorf("declined: %d to %q, want a redirect home", rec.Code, rec.Header().Get("Location"))
	}
}
