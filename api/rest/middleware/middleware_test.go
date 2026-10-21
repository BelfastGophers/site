// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// capture serves one request through h and returns the context the handler
// saw.
func capture(t *testing.T, h func(http.Handler) http.Handler, req *http.Request) context.Context {
	t.Helper()
	var ctx context.Context
	h(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		ctx = r.Context()
	})).ServeHTTP(httptest.NewRecorder(), req)
	return ctx
}

func TestRequestID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, header string
		kept         bool
	}{
		{"valid kept", "caller-id_1.2", true},
		{"missing minted", "", false},
		{"space replaced", "has space", false},
		{"too long replaced", strings.Repeat("a", maxRequestIDLen+1), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			if tt.header != "" {
				req.Header.Set(RequestIDHeader, tt.header)
			}
			id := RequestIDFrom(capture(t, RequestID, req))
			if id == "" || (id == tt.header) != tt.kept {
				t.Errorf("id = %q from header %q, want kept = %v", id, tt.header, tt.kept)
			}
		})
	}
}

// TestLogHandler checks that a record logged with a request's context
// carries its request id and trace, one logged on an untraced path carries
// the caller's trace id alone, and one logged without carries neither.
func TestLogHandler(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(LogHandler(slog.NewJSONHandler(&buf, nil)))

	tr := newTracing(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	ctx := capture(t, func(h http.Handler) http.Handler { return RequestID(tr.middleware(h)) }, req)
	logger.InfoContext(ctx, "in request")
	logger.InfoContext(t.Context(), "outside request")
	logger.InfoContext(tr.serve(t, "/health", callerParent), "untraced")
	// A logger made by With must keep adding the ids: a wrapper that
	// returned the inner handler's WithAttrs would lose them.
	logger.With("k", "v").InfoContext(ctx, "with attrs")

	type record struct {
		Msg       string `json:"msg"`
		RequestID string `json:"request_id"`
		TraceID   string `json:"trace_id"`
		SpanID    string `json:"span_id"`
	}
	span := trace.SpanContextFromContext(ctx)
	traceID, spanID := span.TraceID().String(), span.SpanID().String()
	want := map[string]record{
		"in request":      {"in request", RequestIDFrom(ctx), traceID, spanID},
		"outside request": {Msg: "outside request"},
		// The caller's span is not this service's, so it is left out.
		"untraced":   {Msg: "untraced", TraceID: callerTrace},
		"with attrs": {"with attrs", RequestIDFrom(ctx), traceID, spanID},
	}
	seen := 0
	for line := range strings.Lines(buf.String()) {
		var got record
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatal(err)
		}
		if got != want[got.Msg] {
			t.Errorf("record = %+v, want %+v", got, want[got.Msg])
		}
		seen++
	}
	if seen != len(want) {
		t.Errorf("logged %d records, want %d:\n%s", seen, len(want), buf.String())
	}

	// Likewise WithGroup, where the ids land in the group.
	buf.Reset()
	logger.WithGroup("g").InfoContext(ctx, "in group")
	if want := `"g":{"request_id":"` + RequestIDFrom(ctx) + `"`; !strings.Contains(buf.String(), want) {
		t.Errorf("logged %s, want it to contain %s", buf.String(), want)
	}
}

// serveRecovered serves one request through Recover and returns the
// response, and what reached net/http if the panic was passed on.
func serveRecovered(t *testing.T, h http.HandlerFunc) (rec *httptest.ResponseRecorder, passedOn any) {
	t.Helper()
	rec = httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	func() {
		defer func() { passedOn = recover() }()
		Recover(slog.New(slog.DiscardHandler), h).ServeHTTP(rec, req)
	}()
	return rec, passedOn
}

// isAbort reports whether a recovered panic value is http.ErrAbortHandler,
// which net/http takes as an instruction to abort the connection.
func isAbort(passedOn any) bool {
	err, ok := passedOn.(error)
	return ok && errors.Is(err, http.ErrAbortHandler)
}

func TestRecover(t *testing.T) {
	t.Parallel()

	t.Run("before the response", func(t *testing.T) {
		t.Parallel()
		rec, passedOn := serveRecovered(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "1000")
			panic("boom")
		})
		if passedOn != nil {
			t.Fatalf("panic %v passed on, want it answered", passedOn)
		}
		if rec.Code != http.StatusInternalServerError || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("got %d %q, want a JSON 500", rec.Code, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("Content-Length") != "" {
			t.Error("the handler's Content-Length survived into the 500")
		}
	})

	t.Run("after the response started", func(t *testing.T) {
		t.Parallel()
		_, passedOn := serveRecovered(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "half a bo")
			panic("boom")
		})
		if !isAbort(passedOn) {
			t.Errorf("passed on %v, want http.ErrAbortHandler so the connection is aborted", passedOn)
		}
	})

	t.Run("after a flush", func(t *testing.T) {
		t.Parallel()
		_, passedOn := serveRecovered(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = http.NewResponseController(w).Flush()
			panic("boom")
		})
		if !isAbort(passedOn) {
			t.Errorf("passed on %v, want http.ErrAbortHandler: a flush starts the response", passedOn)
		}
	})

	t.Run("ErrAbortHandler", func(t *testing.T) {
		t.Parallel()
		_, passedOn := serveRecovered(t, func(http.ResponseWriter, *http.Request) {
			panic(http.ErrAbortHandler)
		})
		if !isAbort(passedOn) {
			t.Errorf("passed on %v, want http.ErrAbortHandler untouched", passedOn)
		}
	})
}

// TestLoggingRecordsAborted checks that a request whose connection was
// aborted still gets its request line, marked aborted, and that the panic
// still reaches net/http.
func TestLoggingRecordsAborted(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	h := Logging(slog.New(slog.NewJSONHandler(&buf, nil)), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	func() {
		defer func() {
			if !isAbort(recover()) {
				t.Error("the abort did not reach net/http")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	if !strings.Contains(buf.String(), `"msg":"request"`) || !strings.Contains(buf.String(), `"aborted":true`) {
		t.Errorf("no aborted request line: %s", buf.String())
	}
}

// TestLoggingFinalStatus checks that the logged status is the final one: a
// 1xx sent ahead of it, and a second WriteHeader that net/http ignores, are
// not recorded.
func TestLoggingFinalStatus(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	h := Logging(slog.New(slog.NewJSONHandler(&buf, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		w.WriteHeader(http.StatusCreated)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !strings.Contains(buf.String(), `"status":201`) {
		t.Errorf("request line does not carry the final status 201: %s", buf.String())
	}
}

// TestLoggingTruncates checks that the client's path and user agent are cut
// short in the request line, so that one request cannot write a huge one.
func TestLoggingTruncates(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	h := Logging(slog.New(slog.NewJSONHandler(&buf, nil)), http.NotFoundHandler())
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/"+strings.Repeat("p", 4*maxLoggedPath), nil)
	// "é" is two bytes, and the leading "a" puts the cut inside one.
	req.Header.Set("User-Agent", "a"+strings.Repeat("é", maxLoggedUserAgent))
	h.ServeHTTP(httptest.NewRecorder(), req)

	var rec struct {
		Path      string
		UserAgent string `json:"user_agent"`
	}
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(rec.Path, "…") || len(rec.Path) > maxLoggedPath+len("…") {
		t.Errorf("logged a %d-byte path, want it cut to %d and marked", len(rec.Path), maxLoggedPath)
	}
	if want := "a" + strings.Repeat("é", maxLoggedUserAgent/2-1) + "…"; rec.UserAgent != want {
		t.Errorf("logged user agent %q, want %q", rec.UserAgent, want)
	}
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate(short) = %q, want it unchanged", got)
	}
}

// TestRecoverKeepsOuterHeaders checks that a 500 keeps the headers the outer
// middleware set and drops those the handler set for the body it never sent.
func TestRecoverKeepsOuterHeaders(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	rec.Header().Set(RequestIDHeader, "outer")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	Recover(slog.New(slog.DiscardHandler), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Encoding", "gzip")
		panic("boom")
	})).ServeHTTP(rec, req)

	if rec.Header().Get(RequestIDHeader) != "outer" {
		t.Error("the outer middleware's header was dropped")
	}
	for _, h := range []string{"ETag", "Content-Encoding"} {
		if rec.Header().Get(h) != "" {
			t.Errorf("the handler's %s survived into the 500", h)
		}
	}
}

// TestRecorderFlusher checks that code asserting http.Flusher, rather than
// using http.ResponseController, can still flush through the middleware.
func TestRecorderFlusher(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	Logging(slog.New(slog.DiscardHandler), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("the response writer is not an http.Flusher")
		}
		f.Flush()
	})).ServeHTTP(rec, req)
	if !rec.Flushed {
		t.Error("the flush did not reach the client")
	}
}

// TestRecoverAfterFailedFlush checks that a flush that sent nothing, through
// a writer that cannot flush, leaves a panic answerable with a 500.
func TestRecoverAfterFailedFlush(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	h := Recover(slog.New(slog.DiscardHandler), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = http.NewResponseController(w).Flush()
		panic("boom")
	}))
	// The anonymous struct hides the recorder's Flush.
	h.ServeHTTP(struct{ http.ResponseWriter }{rec}, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: the failed flush sent nothing", rec.Code)
	}
}

// TestLoggingSwitchingProtocols checks that 101, unlike the other 1xx
// statuses, is logged as the final status it is.
func TestLoggingSwitchingProtocols(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	h := Logging(slog.New(slog.NewJSONHandler(&buf, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !strings.Contains(buf.String(), `"status":101`) {
		t.Errorf("request line does not carry 101: %s", buf.String())
	}
}

// TestSecurityHeaders checks that every response carries the headers,
// including an error the handler wrote itself.
func TestSecurityHeaders(t *testing.T) {
	t.Parallel()
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
