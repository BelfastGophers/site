// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

package health

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// probe sends one readiness probe and returns the response.
func probe(t *testing.T, rd *Readiness, logger *slog.Logger) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	if err := rd.Handler(logger)(rec, req); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestLive(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	if err := Live()(rec, req); err != nil || rec.Code != http.StatusOK {
		t.Errorf("got %d, %v; want 200", rec.Code, err)
	}
}

func TestReady(t *testing.T) {
	t.Parallel()
	failing := func(context.Context) error { return errors.New("connection refused") }
	passing := func(context.Context) error { return nil }

	tests := []struct {
		name   string
		checks map[string]Check
		drain  bool
		want   int
	}{
		{"no checks", nil, false, http.StatusOK},
		{"checks pass", map[string]Check{"db": passing}, false, http.StatusOK},
		{"a check fails", map[string]Check{"db": passing, "cache": failing}, false, http.StatusServiceUnavailable},
		{"draining", map[string]Check{"db": passing}, true, http.StatusServiceUnavailable},
		{"a check panics", map[string]Check{"db": func(context.Context) error { panic("nil *sql.DB") }}, false, http.StatusServiceUnavailable},
		{"a check ignores its deadline", map[string]Check{"db": func(context.Context) error { time.Sleep(time.Hour); return nil }}, false, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			readiness := NewReadiness(tt.checks)
			if tt.drain {
				readiness.Drain()
			}
			if rec := probe(t, readiness, slog.New(slog.DiscardHandler)); rec.Code != tt.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// TestReadyLogsTransitions checks that a failing check is logged when it
// starts failing and when it recovers, not on every probe in between.
func TestReadyLogsTransitions(t *testing.T) {
	t.Parallel()
	var down atomic.Bool
	readiness := NewReadiness(map[string]Check{"db": func(context.Context) error {
		if down.Load() {
			return errors.New("connection refused")
		}
		return nil
	}})
	readiness.reuseFor = 0 // each probe must see the check's new answer
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	probe(t, readiness, logger)
	down.Store(true)
	probe(t, readiness, logger)
	probe(t, readiness, logger)
	down.Store(false)
	probe(t, readiness, logger)

	got := logs.String()
	if n := strings.Count(got, "readiness check failed"); n != 1 {
		t.Errorf("failure logged %d times, want once:\n%s", n, got)
	}
	if n := strings.Count(got, "readiness check recovered"); n != 1 {
		t.Errorf("recovery logged %d times, want once:\n%s", n, got)
	}
}

// TestReadyProberGone checks that a prober hanging up mid-probe is not taken
// for a failing dependency: the checks run to their answer regardless.
func TestReadyProberGone(t *testing.T) {
	t.Parallel()
	readiness := NewReadiness(map[string]Check{"db": func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
			return nil
		}
	}})
	var logs bytes.Buffer
	handler := readiness.Handler(slog.New(slog.NewTextHandler(&logs, nil)))

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // the prober has already gone
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	if err := handler(rec, req); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || logs.Len() != 0 {
		t.Errorf("got %d, logged %q; want 200 and nothing logged", rec.Code, logs.String())
	}
}

// TestReadyRunsChecksOnce checks that probes arriving together, or just
// after one another, share one run of the checks: /ready is public, and
// must not pass every request on to the service's dependencies.
func TestReadyRunsChecksOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	release := make(chan struct{})
	readiness := NewReadiness(map[string]Check{"db": func(context.Context) error {
		calls.Add(1)
		<-release
		return nil
	}})
	logger := slog.New(slog.DiscardHandler)

	const probes = 5
	codes := make(chan int, probes)
	for range probes {
		go func() {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ready", nil)
			rec := httptest.NewRecorder()
			_ = readiness.Handler(logger)(rec, req)
			codes <- rec.Code
		}()
	}
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	close(release)
	for range probes {
		if code := <-codes; code != http.StatusOK {
			t.Errorf("status = %d, want 200", code)
		}
	}
	if rec := probe(t, readiness, logger); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 from the last run", rec.Code)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("check ran %d times for %d probes, want once", n, probes+1)
	}
}

// TestReadyHungCheck checks that a check that never returns is not started
// again by each probe, which would leak a goroutine every time.
func TestReadyHungCheck(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	readiness := NewReadiness(map[string]Check{"db": func(context.Context) error {
		calls.Add(1)
		<-hang
		return nil
	}})
	readiness.reuseFor = 0
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	for range 3 {
		if rec := probe(t, readiness, logger); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("check started %d times, want once while it hangs", n)
	}
}
