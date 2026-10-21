// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

package servetest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"belfastgophers.com/site/api/rest/health"
	"belfastgophers.com/site/api/rest/serve"
)

// run is a small program of the shape Start runs: it serves /ready through
// serve.Run, on the address its environment gives it.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	return runWith(nil)(ctx, args, getenv, stdout, stderr)
}

// runWith is run with its JSON handler's options, and a /listening route
// that logs another record with msg "listening".
func runWith(opts *slog.HandlerOptions) RunFunc {
	return func(ctx context.Context, _ []string, getenv func(string) string, stdout, _ io.Writer) error {
		if getenv("LOG_FORMAT") != "json" {
			return fmt.Errorf("LOG_FORMAT = %q, want Start to force json", getenv("LOG_FORMAT"))
		}
		return serveReady(ctx, getenv, slog.New(slog.NewJSONHandler(stdout, opts)))
	}
}

func serveReady(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
	readiness := health.NewReadiness(nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		_ = readiness.Handler(logger)(w, r)
	})
	mux.HandleFunc("GET /listening", func(http.ResponseWriter, *http.Request) {
		logger.Info("listening", "addr", "elsewhere")
	})
	return serve.Run(ctx, serve.Config{
		Addr:      net.JoinHostPort(getenv("HOST"), getenv("PORT")),
		Handler:   mux,
		Logger:    logger,
		Readiness: readiness,
	})
}

func TestStart(t *testing.T) {
	t.Parallel()
	svc := Start(t, run, nil)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, svc.URL("/ready"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /ready = %d, want 200", resp.StatusCode)
	}

	if err := svc.Stop(); err != nil {
		t.Errorf("Stop = %v, want a clean stop", err)
	}
	if err := svc.Stop(); err != nil {
		t.Errorf("a second Stop = %v, want the first's result", err)
	}
	if !strings.Contains(svc.Logs(), "shutdown complete") {
		t.Errorf("logs missing the shutdown, which Stop waits for:\n%s", svc.Logs())
	}
}

// get sends a GET to path on svc and returns the status.
func get(t *testing.T, svc *Service, path string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, svc.URL(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestStartMoreListening checks that a service logging "listening" more
// than once, as one serving a second port would, still stops.
func TestStartMoreListening(t *testing.T) {
	t.Parallel()
	svc := Start(t, run, nil)
	for range 3 {
		get(t, svc, "/listening")
	}
	if err := svc.Stop(); err != nil {
		t.Errorf("Stop = %v, want a clean stop", err)
	}
}

// TestStartMessageKey checks that a service whose handler names the message
// "message", as Cloud Logging's format does, is still found listening.
func TestStartMessageKey(t *testing.T) {
	t.Parallel()
	rename := &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.MessageKey {
			a.Key = "message"
		}
		return a
	}}
	svc := Start(t, runWith(rename), nil)
	if status := get(t, svc, "/ready"); status != http.StatusOK {
		t.Errorf("GET /ready = %d, want 200", status)
	}
}

// fatalTB records the cleanups registered, and turns a call to Fatalf into
// a panic of type fatal, so that a test can check how Start fails without
// failing itself.
type fatalTB struct {
	testing.TB
	cleanups []func()
}

type fatal string

func (f *fatalTB) Helper()                           {}
func (f *fatalTB) Cleanup(fn func())                 { f.cleanups = append(f.cleanups, fn) }
func (f *fatalTB) Fatalf(format string, args ...any) { panic(fatal(fmt.Sprintf(format, args...))) }

// startFails calls Start and returns the message it failed with.
func startFails(tb *fatalTB, run RunFunc) (msg string) {
	defer func() {
		cause := recover()
		f, ok := cause.(fatal)
		if !ok {
			panic(cause)
		}
		msg = string(f)
	}()
	Start(tb, run, nil)
	return ""
}

// TestStartRunFails checks that a program failing before it listens fails
// the test with its error, and that the cleanup does not wait for a run that
// has already returned.
func TestStartRunFails(t *testing.T) {
	t.Parallel()
	tb := &fatalTB{TB: t}
	failing := func(context.Context, []string, func(string) string, io.Writer, io.Writer) error {
		return errors.New("bad config")
	}
	if msg := startFails(tb, failing); !strings.Contains(msg, "bad config") {
		t.Errorf("Start failed with %q, want run's error", msg)
	}
	start := time.Now()
	for _, fn := range tb.cleanups {
		fn()
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("cleanup took %v, want it at once: run had already returned", took)
	}
}
