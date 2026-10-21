// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

package rest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
)

// readBody is a handler that reads the whole body, as a decoder would, and
// returns the read's error wrapped, as a handler would.
func readBody() HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if _, err := io.ReadAll(r.Body); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		return Encode(w, http.StatusOK, map[string]string{})
	}
}

// post sends a body of size bytes to path on r.
func post(t *testing.T, r *Router, path string, size int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(strings.Repeat("x", size)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestMaxBody(t *testing.T) {
	t.Parallel()
	r := NewRouter(slog.New(slog.DiscardHandler), MaxBody(100))
	g := r.Group(Public)
	g.Handle(http.MethodPost, "/default", readBody())
	g.Handle(http.MethodPost, "/raised", readBody(), MaxBody(1000))
	g.Handle(http.MethodPost, "/lowered", readBody(), MaxBody(10))

	tests := []struct {
		path string
		size int
		want int
	}{
		{"/default", 100, http.StatusOK},
		{"/default", 101, http.StatusRequestEntityTooLarge},
		{"/raised", 1000, http.StatusOK},
		{"/raised", 1001, http.StatusRequestEntityTooLarge},
		{"/lowered", 10, http.StatusOK},
		{"/lowered", 11, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s %d bytes", tt.path, tt.size), func(t *testing.T) {
			t.Parallel()
			if rec := post(t, r, tt.path, tt.size); rec.Code != tt.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// TestDefaultMaxBody checks the limit a router applies when it is given none.
func TestDefaultMaxBody(t *testing.T) {
	t.Parallel()
	r := NewRouter(slog.New(slog.DiscardHandler))
	r.Group(Public).Handle(http.MethodPost, "/", readBody())

	if rec := post(t, r, "/", DefaultMaxBody); rec.Code != http.StatusOK {
		t.Errorf("body at the default limit: status = %d, want 200", rec.Code)
	}
	rec := post(t, r, "/", DefaultMaxBody+1)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("body over the default limit: status = %d, want 413", rec.Code)
	}
	var body struct{ Error string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !strings.Contains(body.Error, "exceeds") {
		t.Errorf("413 body = %q, want an error saying the limit was exceeded", rec.Body)
	}
}

func TestMaxBodyRejectsNonPositive(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("MaxBody(0) did not panic")
		}
	}()
	MaxBody(0)
}

// TestErrorResponses checks how a handler's error becomes a response: a
// StatusError keeps its status, and its message with a 4xx code, and
// anything else is a 500 that hides its message from the client.
func TestErrorResponses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		err     error
		status  int
		message string
	}{
		{"status error", Status(http.StatusNotFound, errors.New("no such thing")), http.StatusNotFound, "no such thing"},
		{"plain error", errors.New("database password is hunter2"), http.StatusInternalServerError, "Internal Server Error"},
		{"5xx status error hides its message", Status(http.StatusServiceUnavailable, errors.New("replica db-3 is down")), http.StatusServiceUnavailable, "Service Unavailable"},
		{"non-error status is a 500", Status(http.StatusOK, errors.New("not an error status")), http.StatusInternalServerError, "Internal Server Error"},
		{"4xx sends the status error's own message", fmt.Errorf("lookup in shard-7: %w", Status(http.StatusNotFound, errors.New("no such thing"))), http.StatusNotFound, "no such thing"},
		{"status with no error", Status(http.StatusConflict, nil), http.StatusConflict, "Conflict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := NewRouter(slog.New(slog.DiscardHandler))
			r.Group(Public).Handle(http.MethodGet, "/", func(http.ResponseWriter, *http.Request) error { return tt.err })
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			var body struct{ Error string }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q is not JSON: %v", rec.Body, err)
			}
			if rec.Code != tt.status || body.Error != tt.message {
				t.Errorf("got %d %q, want %d %q", rec.Code, body.Error, tt.status, tt.message)
			}
		})
	}
}

// TestWrap checks that an ordinary handler registered through Wrap is served,
// behind its group's middleware.
func TestWrap(t *testing.T) {
	t.Parallel()
	var wrapped bool
	r := NewRouter(slog.New(slog.DiscardHandler))
	g := r.Group(Public, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			wrapped = true
			next.ServeHTTP(w, req)
		})
	})
	g.Handle(http.MethodGet, "/plain", Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/plain", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot || !wrapped {
		t.Errorf("status = %d, middleware ran = %v; want 418 through the middleware", rec.Code, wrapped)
	}
}

// TestEncodeFailureLeavesResponseUnstarted checks that a value Encode cannot
// encode does not commit a status, so the router can still answer 500
// rather than a 200 carrying an error body.
func TestEncodeFailureLeavesResponseUnstarted(t *testing.T) {
	t.Parallel()
	r := NewRouter(slog.New(slog.DiscardHandler))
	r.Group(Public).Handle(http.MethodGet, "/", func(w http.ResponseWriter, _ *http.Request) error {
		return Encode(w, http.StatusOK, map[string]float64{"x": math.NaN()})
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: %s", rec.Code, rec.Body)
	}
}

// TestErrorAfterResponseStarted checks that an error returned after the
// handler has begun its response is not written over it: the client keeps
// the status and body it was sent, with no error appended.
func TestErrorAfterResponseStarted(t *testing.T) {
	t.Parallel()
	r := NewRouter(slog.New(slog.DiscardHandler))
	r.Group(Public).Handle(http.MethodGet, "/", func(w http.ResponseWriter, _ *http.Request) error {
		w.WriteHeader(http.StatusAccepted)
		if _, err := io.WriteString(w, "partial"); err != nil {
			return err
		}
		return errors.New("failed half way")
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted || rec.Body.String() != "partial" {
		t.Errorf("got %d %q, want 202 \"partial\" untouched", rec.Code, rec.Body)
	}
}

// TestGroupName checks that a group may have a name of the service's own,
// and that its routes are listed under it, but not an empty one.
func TestGroupName(t *testing.T) {
	t.Parallel()
	r := NewRouter(slog.New(slog.DiscardHandler))
	r.Group("webhooks").Handle(http.MethodPost, "/webhooks/x", func(http.ResponseWriter, *http.Request) error { return nil })
	if got := r.Routes(); len(got) != 1 || got[0].Group != "webhooks" {
		t.Errorf("Routes() = %v, want the route in group webhooks", got)
	}

	defer func() {
		if recover() == nil {
			t.Error(`Group("") did not panic`)
		}
	}()
	r.Group("")
}

// TestGroupMiddlewareOrder checks that a group's middleware runs in the
// order given, first outermost, so authentication listed first runs before
// what follows it.
func TestGroupMiddlewareOrder(t *testing.T) {
	t.Parallel()
	var ran []string
	named := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ran = append(ran, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	r := NewRouter(slog.New(slog.DiscardHandler))
	r.Group(Public, named("first"), named("second")).Handle(http.MethodGet, "/", func(http.ResponseWriter, *http.Request) error {
		ran = append(ran, "handler")
		return nil
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if want := []string{"first", "second", "handler"}; !slices.Equal(ran, want) {
		t.Errorf("ran %v, want %v", ran, want)
	}
}

// TestBodyLimitCoversMiddleware checks that a group's middleware reads the
// body within the route's limit too, not only the handler.
func TestBodyLimitCoversMiddleware(t *testing.T) {
	t.Parallel()
	var readErr error
	reads := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, readErr = io.ReadAll(r.Body)
			next.ServeHTTP(w, r)
		})
	}
	r := NewRouter(slog.New(slog.DiscardHandler), MaxBody(16))
	r.Group(Public, reads).Handle(http.MethodPost, "/", func(http.ResponseWriter, *http.Request) error { return nil })
	post(t, r, "/", 100)
	var tooLarge *http.MaxBytesError
	if !errors.As(readErr, &tooLarge) {
		t.Errorf("middleware read %v, want the body limit's error", readErr)
	}
}

// TestRoutesOrdered checks that Routes lists routes by path, then method,
// whatever order they were registered in.
func TestRoutesOrdered(t *testing.T) {
	t.Parallel()
	r := NewRouter(slog.New(slog.DiscardHandler))
	g := r.Group(Public)
	noop := func(http.ResponseWriter, *http.Request) error { return nil }
	g.Handle(http.MethodGet, "/b", noop)
	g.Handle(http.MethodPost, "/a", noop)
	g.Handle(http.MethodGet, "/a", noop)
	var got []string
	for _, route := range r.Routes() {
		got = append(got, route.Method+" "+route.Path)
	}
	if want := []string{"GET /a", "POST /a", "GET /b"}; !slices.Equal(got, want) {
		t.Errorf("Routes = %v, want %v", got, want)
	}
}

// TestErrorAfterFlush checks that a flush starts the response as a write
// does: an error returned after it is not written into the stream.
func TestErrorAfterFlush(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		flush func(http.ResponseWriter)
	}{
		{"http.ResponseController", func(w http.ResponseWriter) { _ = http.NewResponseController(w).Flush() }},
		{"http.Flusher", func(w http.ResponseWriter) { w.(http.Flusher).Flush() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := NewRouter(slog.New(slog.DiscardHandler))
			r.Group(Public).Handle(http.MethodGet, "/", func(w http.ResponseWriter, _ *http.Request) error {
				w.Header().Set("Content-Type", "text/event-stream")
				tt.flush(w)
				return errors.New("stream failed")
			})
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
				t.Errorf("got %d %q, want the flushed 200 with nothing written after it", rec.Code, rec.Body)
			}
		})
	}
}

// failingBody is a request body whose read fails with err.
type failingBody struct{ err error }

func (b failingBody) Read([]byte) (int, error) { return 0, b.err }
func (failingBody) Close() error               { return nil }

// TestBodyReadErrors checks that a body that cannot be read is answered as
// the client's fault, not reported as the service's.
func TestBodyReadErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"client went away mid-body", io.ErrUnexpectedEOF, http.StatusBadRequest},
		{"body arrived too slowly", os.ErrDeadlineExceeded, http.StatusRequestTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := NewRouter(slog.New(slog.DiscardHandler))
			r.Group(Public).Handle(http.MethodPost, "/", readBody())
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", failingBody{tt.err})
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// outerHeader stands in for the middleware outside the router: it sets a
// header before the router runs, and hides net/http's own response writer
// behind a wrapper, as the middleware's recorders do.
func outerHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Outer", "kept")
		next.ServeHTTP(struct{ http.ResponseWriter }{w}, r)
	})
}

// TestErrorResponseHeaders checks every way a request can fail before its
// response starts, against every source of headers: those the middleware
// set are kept, those the handler set for its own body are dropped, and the
// error itself arrives as readable JSON.
func TestErrorResponseHeaders(t *testing.T) {
	t.Parallel()
	setsHeaders := func(fail func(*http.Request) error) HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) error {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("ETag", `"v1"`)
			return fail(r)
		}
	}
	readAll := func(r *http.Request) error {
		_, err := io.ReadAll(r.Body)
		return fmt.Errorf("decode: %w", err)
	}
	tests := []struct {
		name string
		h    HandlerFunc
		body io.Reader
		want int
	}{
		{"handler fault", setsHeaders(func(*http.Request) error { return errors.New("boom") }), nil, http.StatusInternalServerError},
		{"status error", setsHeaders(func(*http.Request) error { return Status(http.StatusNotFound, errors.New("no such thing")) }), nil, http.StatusNotFound},
		{"body too large", setsHeaders(readAll), strings.NewReader(strings.Repeat("x", 101)), http.StatusRequestEntityTooLarge},
		{"body unreadable", setsHeaders(readAll), failingBody{io.ErrUnexpectedEOF}, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := NewRouter(slog.New(slog.DiscardHandler), MaxBody(100))
			r.Group(Public).Handle(http.MethodPost, "/", tt.h)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", tt.body)
			rec := httptest.NewRecorder()
			outerHeader(r).ServeHTTP(rec, req)

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
			if rec.Header().Get("X-Outer") != "kept" {
				t.Error("the middleware's header was dropped")
			}
			for _, h := range []string{"Content-Encoding", "ETag"} {
				if rec.Header().Get(h) != "" {
					t.Errorf("the handler's %s survived into the error response", h)
				}
			}
			var body struct{ Error string }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
				t.Errorf("error body %q is not readable JSON", rec.Body)
			}
		})
	}
}

// TestTooLargeClosesConnection checks that a 413 closes the connection, as
// net/http does for a body over its limit, even behind middleware that hides
// net/http's response writer. Otherwise the server reads on through the
// excess to keep the connection.
func TestTooLargeClosesConnection(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		h    HandlerFunc
	}{
		{"router answers", readBody()},
		{"handler answers itself", func(w http.ResponseWriter, r *http.Request) error {
			if _, err := io.ReadAll(r.Body); err != nil {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
			}
			return nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := NewRouter(slog.New(slog.DiscardHandler), MaxBody(100))
			r.Group(Public).Handle(http.MethodPost, "/", tt.h)
			srv := httptest.NewServer(outerHeader(r))
			t.Cleanup(srv.Close)

			// Under the 256 KiB net/http would read through on its own.
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL, strings.NewReader(strings.Repeat("x", 200<<10)))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Error(err)
			}
			if resp.StatusCode != http.StatusRequestEntityTooLarge || !resp.Close {
				t.Errorf("got %d, connection closed = %v; want 413 and the connection closed", resp.StatusCode, resp.Close)
			}
		})
	}
}

// TestErrorAfterNothingSent checks that an error still gets its response
// when what the handler did before it sent nothing final: a 1xx, or a flush
// through a writer that cannot flush, as http.TimeoutHandler's cannot.
func TestErrorAfterNothingSent(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		before func(http.ResponseWriter)
	}{
		{"103 Early Hints", func(w http.ResponseWriter) { w.WriteHeader(http.StatusEarlyHints) }},
		{"failed flush", func(w http.ResponseWriter) {
			if err := http.NewResponseController(w).Flush(); err == nil {
				panic("flushed through a writer that cannot flush")
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := NewRouter(slog.New(slog.DiscardHandler))
			r.Group(Public).Handle(http.MethodGet, "/", func(w http.ResponseWriter, _ *http.Request) error {
				tt.before(w)
				return Status(http.StatusConflict, errors.New("already exists"))
			})
			// A real server, since httptest.ResponseRecorder takes a 1xx for
			// the final status. outerHeader hides net/http's Flush.
			srv := httptest.NewServer(outerHeader(r))
			t.Cleanup(srv.Close)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Error(err)
			}
			if resp.StatusCode != http.StatusConflict {
				t.Errorf("status = %d, want the handler's 409", resp.StatusCode)
			}
		})
	}
}

// syncBuffer is a log destination the server's goroutines can write to
// while a test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestErrorAfterHijack checks that an error returned after the handler took
// the connection over, as a WebSocket upgrade does, is logged but not
// answered: the connection is no longer the router's to write to.
func TestErrorAfterHijack(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	r := NewRouter(slog.New(slog.NewTextHandler(logs, nil)))
	r.Group(Public).Handle(http.MethodGet, "/", func(w http.ResponseWriter, _ *http.Request) error {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return err
		}
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		_ = rw.Flush()
		_ = conn.Close()
		return errors.New("session ended badly")
	})
	served := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer close(served)
		r.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := srv.Client().Do(req); err == nil {
		_ = resp.Body.Close()
	}
	<-served
	if got := logs.String(); !strings.Contains(got, "after its response started") || strings.Contains(got, "writing error response") {
		t.Errorf("want the error logged as after the response started, and no response written:\n%s", got)
	}
}

// TestNoBodyLeftAlone checks that a request without a body keeps
// http.NoBody, so code that checks for it still knows there is nothing to
// read.
func TestNoBodyLeftAlone(t *testing.T) {
	t.Parallel()
	r := NewRouter(slog.New(slog.DiscardHandler))
	r.Group(Public).Handle(http.MethodGet, "/", func(w http.ResponseWriter, req *http.Request) error {
		if req.Body != http.NoBody {
			return errors.New("the body is no longer http.NoBody")
		}
		return Encode(w, http.StatusOK, map[string]string{})
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d: %s", rec.Code, rec.Body)
	}
}

// TestDenyAll checks the protected group's placeholder: every request is
// refused with a 401 that says what is wanted, before the handler runs.
func TestDenyAll(t *testing.T) {
	t.Parallel()
	r := NewRouter(slog.New(slog.DiscardHandler))
	r.Group(Protected, DenyAll).Handle(http.MethodGet, "/", func(http.ResponseWriter, *http.Request) error {
		t.Error("the handler ran behind DenyAll")
		return nil
	})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("got %d, WWW-Authenticate %q; want a 401 that names its scheme", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}
