// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

// Package serve runs the service's HTTP server: listening, an optional
// profiling server, and the graceful shutdown a platform expects, where a
// second signal stops it at once.
package serve

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"belfastgophers.com/site/api/rest/errlog"
)

// Drainer is told when shutdown begins, so that it can report the service
// as draining: *health.Readiness is one.
type Drainer interface{ Drain() }

// Config is what Run needs to serve.
type Config struct {
	// Addr is the address to listen on, host:port. Port 0 picks a free one,
	// which Run logs.
	Addr string

	// Handler serves every request: the whole service, middleware and all.
	Handler http.Handler

	// Logger receives the server's lifecycle and its own errors. Unset, it
	// is slog.Default().
	Logger *slog.Logger

	// Readiness, if set, is drained when shutdown begins.
	Readiness Drainer

	// DrainFor is how long the service reports draining, while still
	// serving, before it stops accepting connections. Set it to at least the
	// time a load balancer takes to notice a failed health check.
	DrainFor time.Duration

	// PprofAddr, if set, serves runtime profiles on a listener of its own.
	// It must be a loopback address, such as localhost:6060.
	PprofAddr string

	// TLSConfig, if set, serves HTTPS on Addr, with the certificates it
	// gives: one from autocert, say. Unset, Addr serves plain HTTP, for a
	// service behind a proxy or load balancer that ends TLS.
	TLSConfig *tls.Config
}

// Run serves until it is told to stop, then shuts down gracefully, and
// returns once it has. SIGTERM or an interrupt stops it, as does ctx, which
// is how tests stop it; a second signal cuts the shutdown short. It returns
// an error when it could not serve, or when stopping cut requests off.
func Run(ctx context.Context, cfg Config) error {
	// Signals arrive on one channel for the whole of Run: the first starts a
	// graceful shutdown, and a second forces it. With one channel there is
	// no moment between two listeners where a signal could be lost.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	return run(ctx, cfg, signals)
}

// run is Run with the signals passed in, so that tests can send them.
func run(ctx context.Context, cfg Config, signals <-chan os.Signal) error {
	logger := cmp.Or(cfg.Logger, slog.Default())
	// Profiling is off unless asked for. Closed rather than shut down at
	// exit: a profile being taken has nothing worth waiting for.
	if cfg.PprofAddr != "" {
		profiles, err := startPprof(ctx, logger, cfg.PprofAddr)
		if err != nil {
			return err
		}
		defer func() { _ = profiles.Close() }()
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	var conns connCounter
	srv := &http.Server{
		Handler:   cfg.Handler,
		ConnState: conns.track,
		// Without this, net/http reports its own errors through the log
		// package, which would be the one thing not going through slog.
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		// A sixteenth of net/http's default, and still more than an API
		// needs. The default lets one request carry a megabyte of headers.
		MaxHeaderBytes: maxHeaderBytes,
		TLSConfig:      cfg.TLSConfig,
		// WriteTimeout is deliberately unset: it breaks streaming responses.
		// Put a deadline on a slow route with http.TimeoutHandler instead.
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", ln.Addr().String(), "tls", cfg.TLSConfig != nil)
		if err := serve(srv, ln); !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("serve: %w", err)
	case <-signals:
	case <-ctx.Done():
	}
	return shutdown(ctx, srv, &conns, cfg.Readiness, cfg.DrainFor, shutdownTimeout, signals, logger)
}

// serve serves srv on ln, over TLS when srv has a TLS config. The
// certificates come from that config, so ServeTLS is given no files.
func serve(srv *http.Server, ln net.Listener) error {
	if srv.TLSConfig != nil {
		return srv.ServeTLS(ln, "", "")
	}
	return srv.Serve(ln)
}

// maxHeaderBytes caps the size of a request's headers.
const maxHeaderBytes = 64 << 10

// shutdownTimeout is how long in-flight requests get to finish once the
// server stops accepting connections. Keep the drain delay plus this under
// the time a supervisor waits before killing the process: systemd's
// TimeoutStopSec, or Kubernetes' terminationGracePeriodSeconds.
const shutdownTimeout = 10 * time.Second

// progressEvery is how often draining reports what it is waiting for.
const progressEvery = time.Second

// connCounter counts the server's connections, and those with a request in
// flight, which net/http does not report. Give its track method to
// http.Server.ConnState.
type connCounter struct {
	mu    sync.Mutex
	state map[net.Conn]http.ConnState
}

func (c *connCounter) track(conn net.Conn, state http.ConnState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch state {
	case http.StateHijacked, http.StateClosed:
		// A hijacked connection is the handler's now, and the server no
		// longer waits for it.
		delete(c.state, conn)
	default:
		if c.state == nil {
			c.state = map[net.Conn]http.ConnState{}
		}
		c.state[conn] = state
	}
}

// counts returns how many connections are open, and how many of them have a
// request in flight. The rest are idle, kept alive for the client's next
// request, and closing them cuts nothing off.
func (c *connCounter) counts() (open, inFlight int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, state := range c.state {
		if state == http.StateActive {
			inFlight++
		}
	}
	return len(c.state), inFlight
}

// errForced reports a shutdown that cut requests off, so the process exits
// non-zero and the cut is visible to whatever stopped it.
var errForced = errors.New("shutdown cut requests off")

// shutdown stops srv gracefully. It reports draining, so load balancers stop
// sending requests, and keeps serving for drainFor while they notice. Each
// response then closes its connection, so a client that keeps connections
// open, as Go's does, reconnects for its next request, and the load
// balancer sends it to an instance that is staying. Then it
// stops accepting connections and waits up to timeout for in-flight
// requests, logging every progressEvery how many connections are still open.
//
// A signal on force stops it at once, closing every connection, as does
// running out of time. Either returns an error wrapping errForced, unless no
// request was in flight, when nothing was cut off.
func shutdown(ctx context.Context, srv *http.Server, conns *connCounter, readiness Drainer, drainFor, timeout time.Duration, force <-chan os.Signal, logger *slog.Logger) error {
	if readiness != nil {
		readiness.Drain()
	}
	// Idle connections close now; busy ones once their response is sent.
	// Without this, a pooled connection lasts until phase two closes it,
	// which can race with the client sending its next request on it.
	srv.SetKeepAlivesEnabled(false)
	logger.Info("shutting down: send SIGTERM or press Ctrl-C again to stop at once, cutting off requests in flight; under systemd, use systemctl kill",
		"drain_for", drainFor.String(), "timeout", timeout.String())

	ticker := time.NewTicker(progressEvery)
	defer ticker.Stop()

	// Phase one: still serving, while load balancers see /ready fail.
	drained := time.After(drainFor)
	for waiting := true; waiting; {
		select {
		case <-drained:
			waiting = false
		case <-ticker.C:
			open, inFlight := conns.counts()
			logger.Info("draining: reporting not ready, still serving",
				"requests_in_flight", inFlight, "open_connections", open)
		case sig := <-force:
			return closeNow(srv, conns, logger, sig.String())
		}
	}

	// Phase two: no new connections; wait for the open ones to finish.
	// Without ctx's cancellation, which has usually started the shutdown,
	// but with its values.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(ctx) }()
	deadline := time.Now().Add(timeout)
	for {
		select {
		case err := <-done:
			if err == nil {
				logger.Info("shutdown complete: every request finished")
				return nil
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("shutdown: %w", err)
			}
			return closeNow(srv, conns, logger, "timeout")
		case <-ticker.C:
			open, inFlight := conns.counts()
			logger.Info("draining: waiting for requests to finish",
				"requests_in_flight", inFlight, "open_connections", open,
				"remaining", time.Until(deadline).Round(time.Second).String())
		case sig := <-force:
			return closeNow(srv, conns, logger, sig.String())
		}
	}
}

// closeNow closes every connection, cutting off whatever is in flight.
func closeNow(srv *http.Server, conns *connCounter, logger *slog.Logger, why string) error {
	open, inFlight := conns.counts()
	if inFlight == 0 {
		logger.Info("shutting down now: no requests in flight, nothing cut off", "reason", why, "open_connections", open)
	} else {
		logger.Info("shutting down now: cutting off requests in flight", "reason", why, "requests_in_flight", inFlight)
	}
	if err := srv.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if inFlight == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s, %d requests cut off", errForced, why, inFlight)
}

// startPprof serves the runtime profiles on addr, on a server of its own.
// Profiles reveal the program's internals, so they never share the service's
// port, where a load balancer would expose them, and addr must be a loopback
// address: reach it through a port forward. The server has no write timeout,
// because a CPU profile takes as long as it is asked to.
func startPprof(ctx context.Context, logger *slog.Logger, addr string) (*http.Server, error) {
	if !loopback(addr) {
		return nil, fmt.Errorf("pprof address %q is not loopback: use localhost:port and a port forward", addr)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("pprof listen: %w", err)
	}

	// Registered by hand rather than by importing net/http/pprof for its
	// side effect, which would put the profiles on http.DefaultServeMux.
	// Cmdline is left out: it serves the command line, flags and all, and a
	// flag may hold a secret.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index) // also the named profiles: heap, goroutine and the rest
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{
		Handler:           mux,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("pprof listening", "addr", ln.Addr().String())
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("pprof server failed", "err", errlog.Value(err))
		}
	}()
	return srv, nil
}

// loopback reports whether addr, host:port, names only the local machine. An
// empty host means every interface, so it is not loopback.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
