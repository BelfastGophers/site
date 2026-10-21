// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.
//
// Handlers and the route list are yours; this file is the machinery they
// share, so that every service routes, responds and reports errors alike.

// Package rest registers the service's HTTP routes and turns handler errors
// into responses.
package rest

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"

	"belfastgophers.com/site/api/rest/errlog"
	"belfastgophers.com/site/api/rest/internal/writer"
)

// GroupName identifies a set of routes sharing middleware, such as
// "webhooks" for routes authenticated by signature. Every group but Public
// is protected: the policy test checks that its routes refuse a request
// without credentials, so a new group, or a mistyped name, is checked rather
// than left open.
//
// A name is only a label. A group called "admin" does not check that the
// caller is an admin: its middleware does.
type GroupName string

const (
	// Public routes are reachable without authentication. It is the only
	// group the policy test does not check.
	Public GroupName = "public"

	// Protected is the group for ordinary authenticated routes.
	Protected GroupName = "protected"
)

// Middleware wraps a handler with behaviour that runs around it.
type Middleware func(http.Handler) http.Handler

// DenyAll refuses every request with 401. A new service has it on the
// protected group until it has authentication, so protected routes refuse
// everyone instead of serving everyone. Replace it with real authentication.
func DenyAll(http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
		_ = Encode(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
	})
}

// HandlerFunc is a handler that can fail. Returning an error hands the
// response to the router's error handling, so handlers never write one.
type HandlerFunc func(http.ResponseWriter, *http.Request) error

// Wrap adapts an ordinary handler so that it can be registered on a group,
// for handlers written elsewhere: a metrics endpoint, a file server.
// Registering one through its group keeps the group's middleware.
func Wrap(h http.Handler) HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		h.ServeHTTP(w, r)
		return nil
	}
}

// Route is one registered endpoint.
type Route struct {
	Group  GroupName
	Method string
	Path   string
}

// DefaultMaxBody is the largest request body a route accepts unless the
// router or the route says otherwise: ample for JSON, and small enough that a
// client cannot exhaust memory with one request.
const DefaultMaxBody = 1 << 20 // 1 MiB

// Option configures a router, or one route registered on it.
type Option func(*options)

type options struct {
	maxBody int64
}

// MaxBody limits request bodies to n bytes. Passed to NewRouter it sets the
// default for every route; passed to Handle it overrides that for one route,
// up or down, so an upload route can accept more than the rest.
//
// A body over the limit fails the handler's read with *http.MaxBytesError,
// which the router answers with 413. MaxBody panics if n is not positive:
// that is a programming error, and it shows at startup.
func MaxBody(n int64) Option {
	if n <= 0 {
		panic(fmt.Sprintf("rest.MaxBody(%d): the limit must be positive", n))
	}
	return func(o *options) { o.maxBody = n }
}

func apply(o options, opts []Option) options {
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// Router registers every route on one ServeMux and remembers what it
// registered, so tests can assert on the API surface.
type Router struct {
	mux     *http.ServeMux
	logger  *slog.Logger
	routes  []Route
	options options
}

// NewRouter returns a router that logs failed requests to logger.
func NewRouter(logger *slog.Logger, opts ...Option) *Router {
	return &Router{
		mux:     http.NewServeMux(),
		logger:  logger,
		options: apply(options{maxBody: DefaultMaxBody}, opts),
	}
}

// ServeHTTP makes the router the service's handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

// Routes returns every registered route, ordered by path.
func (r *Router) Routes() []Route {
	out := slices.Clone(r.routes)
	slices.SortFunc(out, func(a, b Route) int {
		return cmp.Or(cmp.Compare(a.Path, b.Path), cmp.Compare(a.Method, b.Method))
	})
	return out
}

// Group returns a group whose middleware wraps every route registered
// through it. The chain is fixed here, first middleware outermost, so a
// route cannot be registered without it.
func (r *Router) Group(name GroupName, middleware ...Middleware) *Group {
	if name == "" {
		panic("rest: a group needs a name")
	}
	return &Group{router: r, name: name, chain: slices.Clone(middleware)}
}

// Group is a named set of routes sharing a middleware chain.
type Group struct {
	router *Router
	name   GroupName
	chain  []Middleware
}

// Handle registers h for method and path, wrapped in the group's middleware.
// opts override the router's for this route alone.
func (g *Group) Handle(method, path string, h HandlerFunc, opts ...Option) {
	handler := g.serve(h)
	for i := len(g.chain) - 1; i >= 0; i-- {
		handler = g.chain[i](handler)
	}
	// Outermost, so that the group's middleware reads within the limit too.
	handler = limitBody(handler, apply(g.router.options, opts).maxBody)
	g.router.mux.Handle(method+" "+path, handler)
	g.router.routes = append(g.router.routes, Route{Group: g.name, Method: method, Path: path})
}

// serve reports a handler's error through the router instead of leaving the
// handler to write it.
func (g *Group) serve(h HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := writer.Wrap(w)
		// The headers the middleware set, kept so that an error response
		// carries them but none the handler set for a body it never sent.
		before := w.Header().Clone()
		if err := h(sw, r); err != nil {
			if !sw.Started() {
				header := w.Header()
				clear(header)
				maps.Copy(header, before)
			}
			g.router.fail(err, sw, r)
		}
	})
}

// limitBody caps the request body at n bytes and marks read failures as the
// client's, so the router answers them with 400, 408 or 413 instead of 500.
//
// It runs per route, not as middleware, because a body limit can only be
// lowered by an inner one, never raised: middleware would stop an upload
// route from accepting more than the default.
func limitBody(next http.Handler, n int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Leave http.NoBody alone: code compares against it.
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = bodyReader{ReadCloser: http.MaxBytesReader(w, r.Body, n), w: w}
		}
		next.ServeHTTP(w, r)
	})
}

// bodyReader turns the body's read errors, other than io.EOF, into
// bodyErrors.
type bodyReader struct {
	io.ReadCloser
	w http.ResponseWriter
}

func (b bodyReader) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == nil || errors.Is(err, io.EOF) {
		return n, err
	}
	// The rest of an oversized body is still on the connection. net/http
	// would close it, but only when it can see its own response writer,
	// and the middleware's wrapper hides that. Without this, the server
	// reads up to 256 KiB more to keep the connection alive.
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		b.w.Header().Set("Connection", "close")
	}
	return n, bodyError{err}
}

// bodyError is a failure to read the request body: the client went away,
// sent less than it declared, or sent too slowly. The cause, which may be an
// *http.MaxBytesError, stays reachable through Unwrap.
type bodyError struct{ err error }

func (e bodyError) Error() string { return "read request body: " + e.err.Error() }
func (e bodyError) Unwrap() error { return e.err }

// fail turns a handler error into a response.
//
// If the response has already started, the client has a status and part of
// a body, so the error is only logged: a second response would corrupt the
// first. If the client has gone away, nothing is logged, since that is not
// the service's fault.
func (r *Router) fail(err error, w *writer.Writer, req *http.Request) {
	if w.Started() {
		if req.Context().Err() == nil {
			r.logger.ErrorContext(req.Context(), "request failed after its response started",
				"method", req.Method, "path", req.URL.Path, "err", errlog.Value(err))
		}
		return
	}
	status, message := statusFor(err)
	if status == http.StatusRequestEntityTooLarge {
		// serve has just restored the headers from before the handler ran,
		// dropping the Connection: close the body reader set.
		w.Header().Set("Connection", "close")
	}
	if status >= http.StatusInternalServerError {
		r.logger.ErrorContext(req.Context(), "request failed",
			"method", req.Method, "path", req.URL.Path, "status", status, "err", errlog.Value(err))
		message = http.StatusText(status)
	}

	if err := Encode(w, status, map[string]string{"error": message}); err != nil {
		r.logger.ErrorContext(req.Context(), "writing error response", "err", errlog.Value(err))
	}
}

// statusFor chooses the status and message for an error:
//   - a body too large, or unreadable, is the client's fault however the
//     handler wrapped it: 413, 408 or 400;
//   - a StatusError with a 4xx or 5xx code gives its own status and its own
//     message, not the wrapping's, which may describe the service's
//     internals. fail sends only a 5xx's status text;
//   - anything else is the service's fault: 500.
func statusFor(err error) (int, string) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit)
	}
	var unread bodyError
	if errors.As(err, &unread) {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return http.StatusRequestTimeout, "request body took too long to arrive"
		}
		return http.StatusBadRequest, "request body could not be read"
	}
	var se StatusError
	if errors.As(err, &se) && se.HTTPStatus() >= 400 && se.HTTPStatus() <= 599 {
		return se.HTTPStatus(), se.Error()
	}
	return http.StatusInternalServerError, err.Error()
}

// StatusError is an error that names the status code to send.
type StatusError interface {
	error
	HTTPStatus() int
}

// Status wraps err so that the response uses code, a 4xx or 5xx status,
// instead of 500. Any other code is ignored, and the response is a 500.
//
// With a 4xx code, err's text is the message the client is sent, so it must
// not describe the service's internals; a nil err sends the status's own
// text. With a 5xx code the client is sent only the status's text, and err is
// logged.
func Status(code int, err error) error {
	if err == nil {
		err = errors.New(http.StatusText(code))
	}
	return statusError{code: code, err: err}
}

type statusError struct {
	code int
	err  error
}

func (e statusError) Error() string   { return e.err.Error() }
func (e statusError) Unwrap() error   { return e.err }
func (e statusError) HTTPStatus() int { return e.code }

// Encode writes v as JSON with status. Every JSON response goes through it, so
// status and content type are set in one place. It encodes before writing
// anything, so if v cannot be encoded the response is untouched and the
// router can still send an error.
func Encode(w http.ResponseWriter, status int, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(append(body, '\n')); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}
