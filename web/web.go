// Package web is the site people see: the static pages, the talk proposal
// form, the raffle, and the organisers' pages.
//
// Its handlers render HTML, so they are registered through Page, which shows
// an error as a page rather than the router's JSON.
package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"time"

	"uradical.io/go/binder"

	"belfastgophers.com/site/api/rest"
	"belfastgophers.com/site/api/rest/errlog"
	"belfastgophers.com/site/auth"
	"belfastgophers.com/site/store"
)

//go:embed static templates
var files embed.FS

// funcs are the templates' functions: local shows a time in the site's
// zone.
var funcs = template.FuncMap{"local": func(t time.Time) time.Time { return t.In(zone) }}

// pages holds each page's template, parsed with the layout it fills in. The
// files are embedded, so a template that does not parse is a bug the tests
// catch, not something that can happen in production.
var pages = parsePages()

func parsePages() map[string]*template.Template {
	names, err := fs.Glob(files, "templates/pages/*.html")
	if err != nil {
		panic(err)
	}
	pages := make(map[string]*template.Template, len(names)+1)
	for _, name := range names {
		page := path.Base(name[:len(name)-len(".html")])
		pages[page] = template.Must(template.New("").Funcs(funcs).ParseFS(files, "templates/layout.html", "templates/partials/*.html", name))
	}
	// The home page has a layout of its own.
	pages["home"] = template.Must(template.New("").Funcs(funcs).ParseFS(files, "templates/home.html", "templates/partials/*.html"))
	return pages
}

// static is the site's static content, served as it is, but for its
// directories, which are not listed.
var static = func() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return unlisted{sub}
}()

// unlisted is a file system in which a directory without an index.html does
// not exist, so that the file server answers 404 for it rather than listing
// what it holds. A directory with one, such as an old event's page, is
// served as that page.
type unlisted struct{ fs.FS }

func (u unlisted) Open(name string) (fs.File, error) {
	f, err := u.FS.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if !info.IsDir() {
		return f, nil
	}
	if _, err := fs.Stat(u.FS, path.Join(name, "index.html")); err != nil {
		return nil, errors.Join(&fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}, f.Close())
	}
	return f, nil
}

// Static serves the static pages and assets.
func Static() rest.HandlerFunc {
	return rest.Wrap(http.FileServerFS(static))
}

// Site is the site's dynamic pages, and what they need.
type Site struct {
	store *store.Store
	now   func() time.Time

	// proposals limits how many talk proposals each client sends.
	proposals *limiter
	// guesses limits how many wrong raffle codes each client tries. Only
	// wrong ones count: everyone at a meet-up shares its wifi, and so one
	// address, as they enter.
	guesses *limiter
}

// New returns the site, keeping its data in st.
func New(st *store.Store) *Site {
	return &Site{
		store: st,
		now:   time.Now,
		// Five, then one every twelve minutes: five an hour.
		proposals: newLimiter(5, 12*time.Minute),
		// Ten, then one a minute: plenty for typos in a room, and a few
		// hundred guesses in an evening at a million codes.
		guesses: newLimiter(10, time.Minute),
	}
}

// view is what every page is rendered with: the layout uses Title and Login,
// and the page itself Data.
type view struct {
	Title string
	// Path is the page's, for the navbar to mark it.
	Path string
	// Login is the signed-in organiser's GitHub login, on their pages.
	Login string
	Data  any
}

// render writes the page to w only once it has rendered completely, so a
// template error is a 500 rather than a 200 cut off part way.
func render(w http.ResponseWriter, r *http.Request, status int, page, title string, data any) error {
	tmpl, ok := pages[page]
	if !ok {
		return fmt.Errorf("render: no page %q", page)
	}
	var buf bytes.Buffer
	v := view{Title: title, Path: r.URL.Path, Login: auth.LoginFrom(r.Context()), Data: data}
	if err := tmpl.ExecuteTemplate(&buf, "layout", v); err != nil {
		return fmt.Errorf("render %s: %w", page, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, err := buf.WriteTo(w)
	return err
}

// show is a page as an http.Handler, for the auth package's 401 and 403.
func show(status int, page, title string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := render(w, r, status, page, title, nil); err != nil {
			slog.ErrorContext(r.Context(), "rendering page", "page", page, "err", errlog.Value(err))
			http.Error(w, http.StatusText(status), status)
		}
	})
}

// SignIn is the page anyone but an organiser gets on an organisers' page:
// a 401, with a link to sign in.
func SignIn() http.Handler { return show(http.StatusUnauthorized, "signin", "Organisers") }

// Info is a page with nothing to look up, such as the sponsors or the code
// of conduct, from its template in templates/pages.
func Info(page, title string) http.Handler { return show(http.StatusOK, page, title) }

// NotMember is the page a GitHub user outside the organisation gets when
// they sign in.
func NotMember() http.Handler { return show(http.StatusForbidden, "notmember", "Organisers") }

// Page shows a handler's error as a page. A rest.Status error with a 4xx
// code shows its own message, such as a sign-in that has expired; anything
// else is the site's fault, so it is logged and shown as a 500 that says
// nothing of why.
func Page(h rest.HandlerFunc) rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		err := h(w, r)
		if err == nil {
			return nil
		}
		status, message := http.StatusInternalServerError, "Something went wrong on our side. Please try again."
		var se rest.StatusError
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			status, message = http.StatusRequestEntityTooLarge, "That was more than the form takes."
		case errors.Is(err, binder.ErrMalformedBody), errors.As(err, new(binder.BindErrors)):
			// The site's forms cannot send these: the request was made
			// some other way.
			status, message = http.StatusBadRequest, "That form could not be read."
		case errors.As(err, &se) && se.HTTPStatus() >= 400 && se.HTTPStatus() < 500:
			status, message = se.HTTPStatus(), se.Error()
		default:
			slog.ErrorContext(r.Context(), "page failed", "method", r.Method, "path", r.URL.Path, "err", errlog.Value(err))
		}
		return render(w, r, status, "error", http.StatusText(status), message)
	}
}

// Redirect answers with a redirect to target, for an old address.
func Redirect(target string) rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		http.Redirect(w, r, target, http.StatusMovedPermanently)
		return nil
	}
}
