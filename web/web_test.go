package web

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"belfastgophers.com/site/api/rest"
	"belfastgophers.com/site/store"
)

// meetup is when the test's meet-up starts. The site's clock is an hour in,
// so its raffle is open.
var meetup = time.Date(2026, 10, 29, 18, 30, 0, 0, time.UTC)

// newSite returns the site with a database of the test's own, its clock at
// an hour into the meet-up, which is in the database, and the id of next
// month's.
func newSite(t *testing.T) (s *Site, current, next int64) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "site.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s = New(st)
	s.now = func() time.Time { return meetup.Add(time.Hour) }
	november := meetup.AddDate(0, 0, 28)
	events := []store.Event{
		{MeetupID: "oct", Title: "October", URL: "https://www.meetup.com/belfast-gophers/events/1/", StartsAt: meetup, EndsAt: meetup.Add(3 * time.Hour)},
		{MeetupID: "nov", Title: "November", URL: "https://www.meetup.com/belfast-gophers/events/2/", StartsAt: november, EndsAt: november.Add(3 * time.Hour)},
	}
	if err := st.SyncEvents(t.Context(), events, meetup.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	all, err := st.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return s, all[1].ID, all[0].ID
}

// do sends a request to h through Page, with form as its body when it is
// set, and returns the response.
func do(t *testing.T, h rest.HandlerFunc, method, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if form == nil {
		req = httptest.NewRequestWithContext(t.Context(), method, target, nil)
	} else {
		req = httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	// The router sets the path's wildcards; set {id} as it would.
	if parts := strings.Split(target, "/"); len(parts) > 3 && parts[2] == "events" {
		req.SetPathValue("id", parts[3])
	}
	rec := httptest.NewRecorder()
	if err := Page(h)(rec, req); err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	return rec
}

// TestPagesParse checks every page is there, and renders with no data, as
// the 401 and 403 pages do.
func TestPagesParse(t *testing.T) {
	t.Parallel()
	for _, page := range []string{"draw", "error", "home", "goodluck", "noraffle", "notmember", "organisers", "proposals", "proposed", "raffle", "signin", "speak", "sponsors", "swag", "code-of-conduct", "privacy", "videos"} {
		if _, ok := pages[page]; !ok {
			t.Errorf("no %s page", page)
		}
	}
	for _, h := range []http.Handler{SignIn(), NotMember()} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/organisers", nil))
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 401 or 403", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<title>Belfast Gophers - Organisers</title>") ||
			!strings.Contains(rec.Body.String(), `href="https://uradical.io"`) {
			t.Errorf("page is not in the layout, footer and all:\n%s", rec.Body)
		}
	}
}

// TestPage checks how a handler's error is shown: a 4xx with its message,
// anything else as a 500 that says nothing of why, and is logged.
func TestPage(t *testing.T) {
	// Not parallel: it captures the default logger.
	var logs bytes.Buffer
	defer slog.SetDefault(slog.Default())
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))

	tests := []struct {
		err     error
		status  int
		message string
		logged  bool
	}{
		{rest.Status(http.StatusBadRequest, errors.New("sign-in expired")), http.StatusBadRequest, "sign-in expired", false},
		{errors.New("database is locked"), http.StatusInternalServerError, "Something went wrong", true},
		{&http.MaxBytesError{Limit: 10}, http.StatusRequestEntityTooLarge, "more than the form takes", false},
	}
	for _, tt := range tests {
		logs.Reset()
		h := Page(func(http.ResponseWriter, *http.Request) error { return tt.err })
		rec := httptest.NewRecorder()
		if err := h(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)); err != nil {
			t.Fatal(err)
		}
		if rec.Code != tt.status || !strings.Contains(rec.Body.String(), tt.message) {
			t.Errorf("%v: %d %q, want %d with %q", tt.err, rec.Code, rec.Body, tt.status, tt.message)
		}
		if strings.Contains(rec.Body.String(), "database") {
			t.Errorf("%v: the page shows the error's text", tt.err)
		}
		if logged := strings.Contains(logs.String(), "level=ERROR"); logged != tt.logged {
			t.Errorf("%v: logged = %v, want %v:\n%s", tt.err, logged, tt.logged, logs.String())
		}
	}
}

func proposal() url.Values {
	return url.Values{
		"name": {"Gopher"}, "email": {"Gopher@Example.com"}, "title": {"Generics"},
		"description": {"All of them"}, "duration": {"30"}, "remote": {"false"},
	}
}

func TestPropose(t *testing.T) {
	t.Parallel()
	s, _, _ := newSite(t)
	rec := do(t, s.Propose(), http.MethodPost, "/speak", proposal())
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Generics") {
		t.Fatalf("status = %d, want the thank-you page:\n%s", rec.Code, rec.Body)
	}

	got, err := s.store.Proposals(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("saved %d proposals, want 1", len(got))
	}
	p := got[0]
	if p.Email != "gopher@example.com" || p.DurationMinutes != 30 || p.Remote || !p.SubmittedAt.Equal(s.now()) {
		t.Errorf("saved %+v, want the proposal as sent", p)
	}
}

func TestProposeInvalid(t *testing.T) {
	t.Parallel()
	s, _, _ := newSite(t)
	tests := []struct {
		name  string
		edit  func(url.Values)
		field string
	}{
		{"no name", func(f url.Values) { f.Del("name") }, "name"},
		{"long title", func(f url.Values) { f.Set("title", strings.Repeat("x", maxTitle+1)) }, "title"},
		{"bad email", func(f url.Values) { f.Set("email", "gopher") }, "email"},
		{"named email", func(f url.Values) { f.Set("email", "Gopher <g@example.com>") }, "email"},
		{"no duration", func(f url.Values) { f.Del("duration") }, "duration"},
		{"long duration", func(f url.Values) { f.Set("duration", "600") }, "duration"},
		{"no remote", func(f url.Values) { f.Del("remote") }, "remote"},
		{"blank title", func(f url.Values) { f.Set("title", "   ") }, "title"},
	}
	for _, tt := range tests {
		form := proposal()
		tt.edit(form)
		rec := do(t, s.Propose(), http.MethodPost, "/speak", form)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", tt.name, rec.Code)
		}
		if errs := fieldsInError(rec.Body.String()); !slices.Equal(errs, []string{tt.field}) {
			t.Errorf("%s: errors shown for %v, want %s", tt.name, errs, tt.field)
		}
	}

	// A value the form cannot send means the request was not from it.
	form := proposal()
	form.Set("remote", "maybe")
	if rec := do(t, s.Propose(), http.MethodPost, "/speak", form); rec.Code != http.StatusBadRequest {
		t.Errorf("remote=maybe: status = %d, want 400", rec.Code)
	}

	// The form comes back, filled in, with 422, and nothing is saved.
	form = proposal()
	form.Del("title")
	rec := do(t, s.Propose(), http.MethodPost, "/speak", form)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `value="Gopher@Example.com"`) {
		t.Errorf("status = %d, want 422 with the form filled in:\n%s", rec.Code, rec.Body)
	}
	if got, _ := s.store.Proposals(t.Context()); len(got) != 0 {
		t.Errorf("saved %d proposals from an invalid form", len(got))
	}
}

func TestRaffle(t *testing.T) {
	t.Parallel()
	s, current, _ := newSite(t)
	if body := do(t, s.Raffle(), http.MethodGet, "/raffle", nil).Body.String(); !strings.Contains(body, `action="/raffle"`) {
		t.Errorf("the raffle form is not shown during the meet-up:\n%s", body)
	}

	entry := url.Values{"name": {"Gopher"}, "email": {"g@example.com"}}
	if rec := do(t, s.EnterRaffle(), http.MethodPost, "/raffle", entry); rec.Code != http.StatusOK ||
		strings.Contains(rec.Body.String(), "already entered") {
		t.Errorf("entering: %d, want good luck:\n%s", rec.Code, rec.Body)
	}
	entry.Set("email", "G@EXAMPLE.COM")
	if rec := do(t, s.EnterRaffle(), http.MethodPost, "/raffle", entry); !strings.Contains(rec.Body.String(), "already entered") {
		t.Errorf("entering again in other case: want already entered:\n%s", rec.Body)
	}
	if rec := do(t, s.EnterRaffle(), http.MethodPost, "/raffle", url.Values{"name": {"Gopher"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("entering without an email: %d, want 422", rec.Code)
	}
	if n, err := s.store.EntryCount(t.Context(), current); err != nil || n != 1 {
		t.Errorf("entries = %d, %v, want 1", n, err)
	}

	// Once the meet-up is over, the raffle is closed.
	s.now = func() time.Time { return meetup.Add(4 * time.Hour) }
	if body := do(t, s.Raffle(), http.MethodGet, "/raffle", nil).Body.String(); !strings.Contains(body, "No raffle tonight") {
		t.Errorf("the raffle is open after the meet-up:\n%s", body)
	}
	if rec := do(t, s.EnterRaffle(), http.MethodPost, "/raffle", url.Values{"name": {"Late"}, "email": {"l@example.com"}}); rec.Code != http.StatusConflict {
		t.Errorf("entering after the meet-up: %d, want 409", rec.Code)
	}
}

func TestDraw(t *testing.T) {
	t.Parallel()
	s, current, _ := newSite(t)
	path := "/organisers/events/" + itoa(current)
	if body := do(t, s.EventRaffle(), http.MethodGet, path, nil).Body.String(); !strings.Contains(body, "0 entries") || strings.Contains(body, ">Draw<") {
		t.Errorf("with no entries: want 0 entries and no draw button:\n%s", body)
	}
	for _, email := range []string{"a@example.com", "b@example.com"} {
		if _, err := s.store.Enter(t.Context(), current, "Name "+email, email); err != nil {
			t.Fatal(err)
		}
	}

	if rec := do(t, s.Draw(), http.MethodPost, path+"/draw", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != path {
		t.Errorf("draw: %d to %q, want a redirect to the raffle", rec.Code, rec.Header().Get("Location"))
	}
	body := do(t, s.EventRaffle(), http.MethodGet, path, nil).Body.String()
	if !strings.Contains(body, `<div id="winner">Name `) || !strings.Contains(body, ">Draw<") {
		t.Errorf("after one draw: want the winner and the draw button:\n%s", body)
	}

	do(t, s.Draw(), http.MethodPost, path+"/draw", nil)
	// Drawing with nobody left is not an error.
	do(t, s.Draw(), http.MethodPost, path+"/draw", nil)
	body = do(t, s.EventRaffle(), http.MethodGet, path, nil).Body.String()
	if !strings.Contains(body, "Everyone has won") || strings.Contains(body, ">Draw<") {
		t.Errorf("after everyone has won: want no draw button:\n%s", body)
	}

	if rec := do(t, s.EventRaffle(), http.MethodGet, "/organisers/events/999", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown event: %d, want 404", rec.Code)
	}
	if rec := do(t, s.EventRaffle(), http.MethodGet, "/organisers/events/x", nil); rec.Code != http.StatusNotFound {
		t.Errorf("event id x: %d, want 404", rec.Code)
	}
}

func TestProposals(t *testing.T) {
	t.Parallel()
	s, _, _ := newSite(t)
	form := proposal()
	form.Set("description", "<script>alert(1)</script>")
	do(t, s.Propose(), http.MethodPost, "/speak", form)

	body := do(t, s.Proposals(), http.MethodGet, "/organisers/proposals", nil).Body.String()
	for _, want := range []string{"Generics", "gopher@example.com", "about 30 minutes", "in person", "&lt;script&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("proposals page missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<script>") {
		t.Error("a proposal's text is not escaped")
	}
}

// TestStatic checks the static content is served, and has no copy of the
// pages the site now renders.
func TestStatic(t *testing.T) {
	t.Parallel()
	h := Static()
	for path, want := range map[string]int{"/nov2020/": http.StatusOK, "/images/logo.webp": http.StatusOK, "/speak/": http.StatusNotFound, "/talk-ideas/": http.StatusNotFound} {
		rec := httptest.NewRecorder()
		if err := h(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)); err != nil {
			t.Fatal(err)
		}
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}

// fieldsInError returns the fields a page shows an error for.
func fieldsInError(page string) []string {
	var fields []string
	for _, m := range errorID.FindAllStringSubmatch(page, -1) {
		fields = append(fields, m[1])
	}
	return fields
}

var errorID = regexp.MustCompile(`id="([a-z]+)-error"`)

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// TestHome checks the home page shows the next meet-up, with its Meetup
// link, and says so when none is scheduled.
func TestHome(t *testing.T) {
	t.Parallel()
	s, current, _ := newSite(t)
	body := do(t, s.Home(), http.MethodGet, "/", nil).Body.String()
	for _, want := range []string{"October", "Thursday 29 October 2026", `href="https://www.meetup.com/belfast-gophers/events/1/"`, "Belfast-based community", `href="https://uradical.io"`, `alt="DailyPay"`, `href="/sponsors"`} {
		if !strings.Contains(body, want) {
			t.Errorf("home page during October does not contain %q", want)
		}
	}
	if strings.Contains(body, `class="cover"`) {
		t.Error("home page shows a cover October does not have")
	}

	// With its cover, which links to the event on Meetup and stands for the
	// text, which a screen reader reads from its alt.
	if err := s.store.SaveCover(t.Context(), current, "https://meetup.example/oct.jpeg", jpeg); err != nil {
		t.Fatal(err)
	}
	body = do(t, s.Home(), http.MethodGet, "/", nil).Body.String()
	want := `<a href="https://www.meetup.com/belfast-gophers/events/1/"><img class="cover" src="/events/` + itoa(current) +
		`/cover" alt="October, Thursday 29 October 2026: sign up on Meetup"></a>`
	if !strings.Contains(body, want) {
		t.Errorf("home page during October does not contain %q", want)
	}
	if strings.Contains(body, "event-title") {
		t.Error("home page shows the event's text as well as its cover")
	}

	s.now = func() time.Time { return meetup.AddDate(1, 0, 0) }
	if body := do(t, s.Home(), http.MethodGet, "/", nil).Body.String(); !strings.Contains(body, "No meet-up scheduled yet") {
		t.Errorf("home page with nothing scheduled does not say so:\n%s", body)
	}
}

// jpeg is enough of a JPEG for http.DetectContentType.
var jpeg = []byte("\xff\xd8\xff\xe0 a cover")

// TestCover checks an event's cover is served as an image, and is a 404
// for an event with none.
func TestCover(t *testing.T) {
	t.Parallel()
	s, current, next := newSite(t)
	if err := s.store.SaveCover(t.Context(), current, "https://meetup.example/oct.jpeg", jpeg); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		id     string
		status int
		kind   string
	}{
		{itoa(current), http.StatusOK, "image/jpeg"},
		{itoa(next), http.StatusNotFound, "text/html; charset=utf-8"},
		{"nonsense", http.StatusNotFound, "text/html; charset=utf-8"},
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/events/"+tt.id+"/cover", nil)
		req.SetPathValue("id", tt.id)
		rec := httptest.NewRecorder()
		if err := Page(s.Cover())(rec, req); err != nil {
			t.Fatal(err)
		}
		if rec.Code != tt.status || rec.Header().Get("Content-Type") != tt.kind {
			t.Errorf("cover of %s = %d %s, want %d %s", tt.id, rec.Code, rec.Header().Get("Content-Type"), tt.status, tt.kind)
		}
	}
}

// TestNav checks every rendered page has the navbar, with the page it is on
// marked.
func TestNav(t *testing.T) {
	t.Parallel()
	s, _, _ := newSite(t)
	for path, h := range map[string]rest.HandlerFunc{"/": s.Home(), "/speak": s.Speak(), "/swag": rest.Wrap(Info("swag", "Swag")), "/sponsors": rest.Wrap(Info("sponsors", "Sponsors")), "/videos": s.Videos()} {
		body := do(t, h, http.MethodGet, path, nil).Body.String()
		if !strings.Contains(body, `>Sponsors</a>`) {
			t.Errorf("%s has no navbar", path)
		}
		if want := `<a href="` + path + `" aria-current="page">`; !strings.Contains(body, want) {
			t.Errorf("%s does not mark itself in the navbar", path)
		}
	}
}

// TestVideos checks the videos page has a player for every video, newest
// first, with the title escaped.
func TestVideos(t *testing.T) {
	t.Parallel()
	s, _, _ := newSite(t)
	if body := do(t, s.Videos(), http.MethodGet, "/videos", nil).Body.String(); !strings.Contains(body, "on their way") {
		t.Errorf("with no videos: want a note that they are coming:\n%s", body)
	}
	videos := []store.Video{
		{YouTubeID: "old", Title: "Generics <in> Go", PublishedAt: meetup.AddDate(-1, 0, 0)},
		{YouTubeID: "new", Title: "Fuzzing", PublishedAt: meetup},
	}
	if err := s.store.SaveVideos(t.Context(), videos); err != nil {
		t.Fatal(err)
	}
	body := do(t, s.Videos(), http.MethodGet, "/videos", nil).Body.String()
	newer := strings.Index(body, "youtube-nocookie.com/embed/new")
	older := strings.Index(body, "youtube-nocookie.com/embed/old")
	if newer < 0 || older < 0 || newer > older {
		t.Errorf("want a player for each video, newest first:\n%s", body)
	}
	if !strings.Contains(body, "Generics &lt;in&gt; Go") {
		t.Error("a video's title is not escaped")
	}
}

// TestOrganisers checks the organisers' list gives each event's date, a
// link to its raffle, and its Meetup page.
func TestOrganisers(t *testing.T) {
	t.Parallel()
	s, current, _ := newSite(t)
	body := do(t, s.Organisers(), http.MethodGet, "/organisers", nil).Body.String()
	for _, want := range []string{"Thu 29 Oct 2026", `<a href="/organisers/events/` + itoa(current) + `">Raffle</a>`, `href="https://www.meetup.com/belfast-gophers/events/1/"`} {
		if !strings.Contains(body, want) {
			t.Errorf("organisers' page missing %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "18:30") {
		t.Error("organisers' page shows a time")
	}
}
