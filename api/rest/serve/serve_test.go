// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"belfastgophers.com/site/api/rest/health"
)

// syncBuffer is a log destination safe to write from the server's goroutines
// while a test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
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

// waitForLog waits until logs contains want.
func waitForLog(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("logs never contained %q:\n%s", want, logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// await returns what ch delivers, failing the test if nothing arrives within
// 5s: a shutdown that hangs then fails fast, rather than at go test's
// timeout ten minutes later.
func await(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting after 5s")
		return nil
	}
}

// drainTest is a server with one request in flight, held until release is
// closed or the connection is cut.
type drainTest struct {
	srv     *httptest.Server
	conns   *connCounter
	logs    *syncBuffer
	logger  *slog.Logger
	release chan struct{}
	reply   chan error // the in-flight request's outcome
	ctx     context.Context
}

func newDrainTest(t *testing.T) *drainTest {
	t.Helper()
	d := &drainTest{
		conns:   &connCounter{},
		logs:    &syncBuffer{},
		release: make(chan struct{}),
		reply:   make(chan error, 1),
		ctx:     t.Context(),
	}
	d.logger = slog.New(slog.NewTextHandler(d.logs, nil))
	started := make(chan struct{})
	d.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-d.release:
			_, _ = io.WriteString(w, "done")
		case <-r.Context().Done():
		}
	}))
	d.srv.Config.ConnState = d.conns.track
	d.srv.Start()
	t.Cleanup(d.srv.Close)

	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, d.srv.URL, nil)
		if err != nil {
			d.reply <- err
			return
		}
		resp, err := d.srv.Client().Do(req)
		if err == nil {
			var body []byte
			body, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err == nil && string(body) != "done" {
				err = errors.New("incomplete response: " + string(body))
			}
		}
		d.reply <- err
	}()
	<-started
	return d
}

// shutdown runs shutdown on the test's server in the background.
func (d *drainTest) shutdown(drainFor, timeout time.Duration, force <-chan os.Signal) <-chan error {
	result := make(chan error, 1)
	go func() {
		result <- shutdown(d.ctx, d.srv.Config, d.conns, health.NewReadiness(nil), drainFor, timeout, force, d.logger)
	}()
	return result
}

// TestShutdownWaitsForRequests checks that a graceful shutdown lets the
// request in flight finish, reporting it while it waits.
func TestShutdownWaitsForRequests(t *testing.T) {
	t.Parallel()
	d := newDrainTest(t)
	result := d.shutdown(0, 10*time.Second, nil)

	waitForLog(t, d.logs, "draining: waiting for requests to finish")
	waitForLog(t, d.logs, "requests_in_flight=1")
	close(d.release)

	if err := await(t, d.reply); err != nil {
		t.Errorf("in-flight request failed: %v", err)
	}
	if err := await(t, result); err != nil {
		t.Errorf("shutdown = %v, want nil", err)
	}
	waitForLog(t, d.logs, "shutdown complete")
}

// TestShutdownForced checks that a second signal cuts the request in flight
// off at once, in either phase, and is reported as a forced shutdown.
func TestShutdownForced(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		drainFor time.Duration
	}{
		{"while reporting not ready", time.Hour},
		{"while waiting for requests", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := newDrainTest(t)
			force := make(chan os.Signal, 1)
			result := d.shutdown(tt.drainFor, time.Hour, force)

			waitForLog(t, d.logs, "draining:")
			force <- syscall.SIGTERM
			if err := await(t, result); !errors.Is(err, errForced) {
				t.Errorf("shutdown = %v, want errForced", err)
			}
			if err := await(t, d.reply); err == nil {
				t.Error("in-flight request completed, want it cut off")
			}
			waitForLog(t, d.logs, "shutting down now: cutting off requests in flight")
		})
	}
}

// TestShutdownTimeout checks that requests still running when the timeout
// passes are cut off rather than left open.
func TestShutdownTimeout(t *testing.T) {
	t.Parallel()
	d := newDrainTest(t)
	result := d.shutdown(0, 100*time.Millisecond, nil)

	if err := await(t, result); !errors.Is(err, errForced) {
		t.Errorf("shutdown = %v, want errForced", err)
	}
	if err := await(t, d.reply); err == nil {
		t.Error("in-flight request completed, want it cut off")
	}
	waitForLog(t, d.logs, "reason=timeout")
}

// TestShutdownForcedIdle checks that forcing a shutdown with no connection
// open is reported as what it is: nothing was cut off, so it is not an error.
func TestShutdownForcedIdle(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	logs := &syncBuffer{}
	force := make(chan os.Signal, 1)
	force <- syscall.SIGTERM

	err := shutdown(t.Context(), srv.Config, &connCounter{}, nil, time.Hour, time.Hour, force, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Errorf("shutdown = %v, want nil: no request was cut off", err)
	}
	waitForLog(t, logs, "nothing cut off")
}

// TestShutdownForcedKeepAlive checks that closing only idle keep-alive
// connections is not reported as cutting requests off.
func TestShutdownForcedKeepAlive(t *testing.T) {
	t.Parallel()
	conns := &connCounter{}
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Config.ConnState = conns.track
	srv.Start()
	t.Cleanup(srv.Close)

	// A finished request leaves its connection open, idle, for the next.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for open, inFlight := conns.counts(); open != 1 || inFlight != 0; open, inFlight = conns.counts() {
		if time.Now().After(deadline) {
			t.Fatalf("connections = %d open, %d in flight; want one idle", open, inFlight)
		}
		time.Sleep(10 * time.Millisecond)
	}

	force := make(chan os.Signal, 1)
	force <- syscall.SIGTERM
	err = shutdown(t.Context(), srv.Config, conns, nil, time.Hour, time.Hour, force, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Errorf("shutdown = %v, want nil: an idle connection has nothing to cut off", err)
	}
}

// TestShutdownDrainsFirst checks the first phase from a load balancer's
// side: /ready reports draining, while the service still accepts new
// connections and serves requests on them.
func TestShutdownDrainsFirst(t *testing.T) {
	t.Parallel()
	conns := &connCounter{}
	readiness := health.NewReadiness(nil)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		_ = readiness.Handler(slog.New(slog.DiscardHandler))(w, r)
	})
	mux.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {})
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.ConnState = conns.track
	srv.Start()
	t.Cleanup(srv.Close)
	force := make(chan os.Signal, 1)
	result := make(chan error, 1)
	go func() {
		result <- shutdown(t.Context(), srv.Config, conns, readiness, time.Hour, time.Hour, force, slog.New(slog.DiscardHandler))
	}()

	// A new connection for each request, as a load balancer's probe makes.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	status := func(path string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s while draining: %v", path, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	deadline := time.Now().Add(5 * time.Second)
	for status("/ready") != http.StatusServiceUnavailable {
		if time.Now().After(deadline) {
			t.Fatal("/ready never reported draining")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := status("/health"); got != http.StatusOK {
		t.Errorf("GET /health while draining = %d, want 200: the service should still serve", got)
	}

	// A client that reuses connections is told to make a new one, so its
	// next request goes to an instance that is staying.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /health while draining: %v", err)
	}
	_ = resp.Body.Close()
	if !resp.Close {
		t.Error("response while draining kept its connection open, want Connection: close")
	}

	// The client has its answer a moment before the server marks the
	// connection closed; force only once nothing is in flight, so the
	// shutdown has nothing to cut off.
	for _, inFlight := conns.counts(); inFlight != 0; _, inFlight = conns.counts() {
		if time.Now().After(deadline) {
			t.Fatal("a request was still in flight after its response arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	force <- syscall.SIGTERM
	if err := await(t, result); err != nil {
		t.Errorf("shutdown = %v, want nil", err)
	}
}

// TestConnCounterHijacked checks that a hijacked connection leaves the
// count: it is the handler's now, and the shutdown no longer waits for it.
func TestConnCounterHijacked(t *testing.T) {
	t.Parallel()
	conns := &connCounter{}
	hijacked := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		t.Cleanup(func() { _ = conn.Close() })
		close(hijacked)
	}))
	srv.Config.ConnState = conns.track
	srv.Start()
	t.Cleanup(srv.Close)

	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		if err != nil {
			return
		}
		if resp, err := srv.Client().Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-hijacked
	if open, inFlight := conns.counts(); open != 0 || inFlight != 0 {
		t.Errorf("counts = %d open, %d in flight; want the hijacked connection gone", open, inFlight)
	}
}

// startServe runs run on a free port with signals the test controls, and
// returns its address once it is listening.
func startServe(t *testing.T, cfg Config, signals chan os.Signal) (addr string, logs *syncBuffer, result <-chan error) {
	t.Helper()
	logs = &syncBuffer{}
	cfg.Addr = "127.0.0.1:0"
	cfg.Logger = slog.New(slog.NewJSONHandler(logs, nil))
	if cfg.Handler == nil {
		cfg.Handler = http.NotFoundHandler()
	}
	done := make(chan error, 1)
	go func() { done <- run(t.Context(), cfg, signals) }()
	waitForLog(t, logs, `"msg":"listening"`)
	return loggedAddr(t, logs, "listening"), logs, done
}

// loggedAddr returns the addr of the first record logged with msg.
func loggedAddr(t *testing.T, logs *syncBuffer, msg string) string {
	t.Helper()
	for line := range strings.Lines(logs.String()) {
		var rec struct{ Msg, Addr string }
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == msg {
			return rec.Addr
		}
	}
	t.Fatalf("no %q record:\n%s", msg, logs)
	return ""
}

// TestRunStopsOnSignal checks Run's lifecycle end to end: it serves, a
// signal starts a graceful shutdown, and it returns cleanly.
func TestRunStopsOnSignal(t *testing.T) {
	t.Parallel()
	signals := make(chan os.Signal, 2)
	addr, logs, result := startServe(t, Config{}, signals)

	if status := statusOf(t, "http://"+addr+"/"); status != http.StatusNotFound {
		t.Errorf("status = %d, want the handler's 404", status)
	}
	signals <- syscall.SIGTERM
	if err := await(t, result); err != nil {
		t.Errorf("run = %v, want a clean stop", err)
	}
	waitForLog(t, logs, "shutdown complete")
}

// TestRunTLS checks that a TLS config serves HTTPS, with its certificate.
func TestRunTLS(t *testing.T) {
	t.Parallel()
	// httptest makes a certificate, and a client that trusts it, but its
	// server is not needed.
	certs := httptest.NewTLSServer(http.NotFoundHandler())
	certs.Close()
	signals := make(chan os.Signal, 2)
	addr, _, result := startServe(t, Config{TLSConfig: certs.TLS}, signals)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := certs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Error(err)
	}
	if resp.StatusCode != http.StatusNotFound || resp.TLS == nil {
		t.Errorf("status = %d, TLS = %v; want the handler's 404 over TLS", resp.StatusCode, resp.TLS != nil)
	}
	signals <- syscall.SIGTERM
	if err := await(t, result); err != nil {
		t.Errorf("run = %v, want a clean stop", err)
	}
}

func TestRunListenError(t *testing.T) {
	t.Parallel()
	err := run(t.Context(), Config{Addr: "256.0.0.1:0", Handler: http.NotFoundHandler(), Logger: slog.New(slog.DiscardHandler)}, nil)
	if err == nil || !strings.Contains(err.Error(), "listen:") {
		t.Errorf("run = %v, want a listen error", err)
	}
}

// statusOf sends a GET to url and returns the response's status.
func statusOf(t *testing.T, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Error(err)
	}
	return resp.StatusCode
}

// TestPprof checks that profiles are served when asked for, and only on a
// port of their own.
func TestPprof(t *testing.T) {
	t.Parallel()
	signals := make(chan os.Signal, 2)
	addr, logs, result := startServe(t, Config{PprofAddr: "127.0.0.1:0"}, signals)
	pprofAddr := loggedAddr(t, logs, "pprof listening")

	if status := statusOf(t, "http://"+pprofAddr+"/debug/pprof/goroutine?debug=1"); status != http.StatusOK {
		t.Errorf("goroutine profile status = %d, want 200", status)
	}
	// The command line may hold a secret passed as a flag.
	if status := statusOf(t, "http://"+pprofAddr+"/debug/pprof/cmdline"); status != http.StatusNotFound {
		t.Errorf("cmdline status = %d, want 404", status)
	}
	if status := statusOf(t, "http://"+addr+"/debug/pprof/"); status != http.StatusNotFound {
		t.Errorf("service port served /debug/pprof/ with %d, want 404", status)
	}
	signals <- syscall.SIGTERM
	if err := await(t, result); err != nil {
		t.Errorf("run = %v, want a clean stop", err)
	}
}

// TestPprofLoopbackOnly checks that profiles are refused an address other
// machines could reach.
func TestPprofLoopbackOnly(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{":0", "0.0.0.0:0", "[::]:0", "192.0.2.1:0", "example.com:0", "nonsense"} {
		err := run(t.Context(), Config{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler(), PprofAddr: addr}, nil)
		if err == nil || !strings.Contains(err.Error(), "not loopback") {
			t.Errorf("pprof on %q: run = %v, want it refused", addr, err)
		}
	}
	for _, addr := range []string{"localhost:6060", "127.0.0.1:6060", "[::1]:6060"} {
		if !loopback(addr) {
			t.Errorf("loopback(%q) = false, want true", addr)
		}
	}
}

// TestRunHeaderLimit checks that a request with headers far larger than an
// API needs is refused before it reaches the handler or its logs.
func TestRunHeaderLimit(t *testing.T) {
	t.Parallel()
	signals := make(chan os.Signal, 2)
	addr, _, result := startServe(t, Config{}, signals)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", strings.Repeat("a", 2*maxHeaderBytes))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("status = %d, want 431", resp.StatusCode)
	}
	signals <- syscall.SIGTERM
	if err := await(t, result); err != nil {
		t.Errorf("run = %v, want a clean stop", err)
	}
}

// TestRunForcedBySecondSignal sends the process real signals, as systemd or
// an operator would: the first starts the drain, and the second cuts the
// request in flight off at once. It is not parallel, because the signals go
// to the whole test binary.
func TestRunForcedBySecondSignal(t *testing.T) {
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- Run(t.Context(), Config{
			Addr: "127.0.0.1:0",
			Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
			}),
			Logger:   slog.New(slog.NewJSONHandler(logs, nil)),
			DrainFor: time.Hour,
		})
	}()
	// Run is listening for signals before it logs its address.
	waitForLog(t, logs, `"msg":"listening"`)
	addr := loggedAddr(t, logs, "listening")
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/", nil)
		if err != nil {
			return
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started

	signal := func() {
		t.Helper()
		if err := self.Signal(syscall.SIGTERM); err != nil {
			t.Skipf("cannot signal this process: %v", err)
		}
	}
	signal()
	waitForLog(t, logs, "shutting down:")
	signal()
	if err := await(t, result); !errors.Is(err, errForced) {
		t.Errorf("Run = %v, want errForced after a second signal", err)
	}
}
