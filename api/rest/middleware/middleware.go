// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.
//
// The middleware every service shares. Which of it runs, and in what order,
// is the service's choice, made in the stack in server.go; this file is not.

// Package middleware holds the behaviour that runs around every request.
package middleware

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
	"uuid"

	"go.opentelemetry.io/otel/trace"

	"belfastgophers.com/site/api/rest"
	"belfastgophers.com/site/api/rest/internal/writer"
)

// Recover answers a panicking handler with a 500 and logs the panic with its
// stack. Without it, net/http would drop the connection and log the panic as
// one line of text, without the request's id or trace. Either way the server
// keeps running.
//
// Two panics are passed on to net/http, which aborts the connection:
//   - http.ErrAbortHandler, which handlers such as httputil.ReverseProxy
//     raise on purpose to abandon a response;
//   - a panic after the response has started. A 500 can no longer be sent,
//     and finishing would hand the client a truncated body that looks
//     complete. This one is logged first.
//
// The 500 keeps only the headers set before Recover ran, so middleware that
// sets response headers, such as CORS, must sit outside it.
func Recover(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := writer.Wrap(w)
		// Kept so a 500 has the outer middleware's headers, not the
		// handler's.
		before := w.Header().Clone()
		defer func() {
			if cause := recover(); cause != nil {
				answerPanic(logger, r, rec, before, cause)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// answerPanic logs a handler's panic and answers it with a 500, or passes it
// on to net/http as Recover describes. before is the response's headers from
// before the handler ran.
func answerPanic(logger *slog.Logger, r *http.Request, rec *writer.Writer, before http.Header, cause any) {
	if err, ok := cause.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(cause)
	}
	logger.LogAttrs(r.Context(), slog.LevelError, "panic",
		slog.Any("cause", cause),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("stack", string(debug.Stack())),
	)
	if rec.Started() {
		panic(http.ErrAbortHandler)
	}
	h := rec.Header()
	clear(h)
	maps.Copy(h, before)
	// The same body the router sends for a 500.
	_ = rest.Encode(rec, http.StatusInternalServerError,
		map[string]string{"error": http.StatusText(http.StatusInternalServerError)})
}

// RequestID gives each request an id, keeping the caller's if it sent a
// valid one, so a request can be followed across services. The id goes back
// in the response and onto the context, where LogHandler adds it to every
// record logged with that context.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = uuid.NewV7().String()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// RequestIDHeader carries the request id in and out of the service.
const RequestIDHeader = "X-Request-Id"

// maxRequestIDLen fits a UUID or any common trace id, and stops a caller
// filling the logs through the header.
const maxRequestIDLen = 64

// validRequestID reports whether a caller's id is safe to log and echo back:
// at most maxRequestIDLen characters, each a letter, digit, '-', '_' or '.',
// none of which ever needs escaping.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for _, c := range []byte(id) {
		if !isIDChar(c) {
			return false
		}
	}
	return true
}

func isIDChar(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.'
}

type requestIDKey struct{}

// RequestIDFrom returns the id RequestID put on the context, or "" when the
// request did not pass through it.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// LogHandler adds the request id and trace to every record logged with a
// request's context, so everything one request logs can be found together,
// not just its request line, and with the request's span in a tracing
// backend. Use the Context logging methods, or LogAttrs, to get them. Like
// any attribute a handler adds, they go into whatever group is open on the
// logger.
//
// span_id is this service's span. A request Trace made none for, such as a
// health probe, carries the caller's trace id alone.
func LogHandler(h slog.Handler) slog.Handler { return logHandler{h} }

type logHandler struct{ slog.Handler }

func (h logHandler) Handle(ctx context.Context, r slog.Record) error {
	id := RequestIDFrom(ctx)
	span := trace.SpanContextFromContext(ctx)
	if id == "" && !span.HasTraceID() {
		return h.Handler.Handle(ctx, r)
	}
	// Add to a clone: a record's attributes may be shared with its copies.
	r = r.Clone()
	if id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	if span.HasTraceID() {
		r.AddAttrs(slog.String("trace_id", span.TraceID().String()))
	}
	if span.HasSpanID() && !span.IsRemote() {
		r.AddAttrs(slog.String("span_id", span.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return logHandler{h.Handler.WithAttrs(attrs)}
}

func (h logHandler) WithGroup(name string) slog.Handler {
	return logHandler{h.Handler.WithGroup(name)}
}

// Logging logs one record per request. The query string and request headers
// are left out: either may carry a credential.
//
// Middleware between Logging and the router must pass the request on rather
// than a copy of it, or Logging cannot see the route pattern the router
// matched.
//
// A request aborted by a panic that Recover passed on is still logged,
// marked aborted; the panic then continues to net/http.
func Logging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := writer.Wrap(w)
		defer func() {
			cause := recover()
			logRequest(logger, r, rec, start, cause != nil)
			if cause != nil {
				panic(cause)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// logRequest writes the one record Logging makes for a request.
func logRequest(logger *slog.Logger, r *http.Request, rec *writer.Writer, start time.Time, aborted bool) {
	logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
		slog.String("method", r.Method),
		slog.String("path", truncate(r.URL.Path, maxLoggedPath)),
		// The router sets the pattern it matched on this request, so it
		// is known only now. It groups requests by route where the path,
		// with its ids, would not; it is empty when nothing matched.
		slog.String("pattern", r.Pattern),
		slog.Int("status", rec.Status()),
		slog.Int64("bytes", rec.Bytes()),
		slog.Duration("duration", time.Since(start)),
		// Behind a proxy this is the proxy's address. Take the client's
		// from the proxy's header only if the service trusts the proxy.
		slog.String("remote_addr", r.RemoteAddr),
		slog.String("user_agent", truncate(r.UserAgent(), maxLoggedUserAgent)),
		slog.Bool("aborted", aborted),
		// A hijacked connection is the handler's: status and bytes stop
		// at what went through the response writer.
		slog.Bool("hijacked", rec.Hijacked()),
	)
}

// The longest path and user agent the request record holds. Both come from
// the client, and every request is logged, so without a cap one request
// could write a line as long as the headers net/http accepts.
const (
	maxLoggedPath      = 1024
	maxLoggedUserAgent = 256
)

// truncate cuts s to at most n bytes, marking the cut, without splitting a
// character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// SecurityHeaders sets the headers that cost nothing on an API. A content
// security policy and HSTS depend on how the service is served, so they
// belong with the service or its proxy rather than here.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
