// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

// Package servetest starts the whole program in a test: run in a goroutine,
// on a free port, waited on until it is ready, and stopped when the test
// ends. A test of the program then goes through every layer a real caller
// does.
package servetest

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// RunFunc is the shape of the program's run function: everything main
// hands it from the operating system, passed in so that a test can pass its
// own.
type RunFunc func(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error

// Service is the whole program, running in a test.
type Service struct {
	// Addr is the address it is listening on.
	Addr string
	logs func() string
	stop func() error
}

// URL returns the address of path on the service.
func (s *Service) URL(path string) string { return "http://" + s.Addr + path }

// Logs returns everything the service has logged so far.
func (s *Service) Logs() string { return s.logs() }

// Stop cancels the service's context and waits for run to return, with its
// last log lines read. It returns run's error. Stopping twice is safe: the
// test's cleanup stops it too.
func (s *Service) Stop() error { return s.stop() }

// Start runs the program as main does, with env as its environment, on a
// free port on localhost, and waits until /ready answers 200, as a load
// balancer would. Each test gets a program of its own, so tests can run in
// parallel. The shutdown delay is zero unless env sets SHUTDOWN_DELAY, so
// stopping is quick.
//
// Start relies on what main.go is scaffolded with: HOST, PORT and LOG_FORMAT
// read from the environment, logs on stdout, and a /ready route. It forces
// HOST, PORT and LOG_FORMAT=json, and reads the address from the record
// serve.Run logs, with msg or message "listening" and an addr.
func Start(t testing.TB, run RunFunc, env map[string]string) *Service {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	getenv := func(key string) string {
		switch key {
		case "HOST":
			return "127.0.0.1"
		case "PORT":
			return "0" // a free port, read back from the log
		case "LOG_FORMAT":
			return "json" // the address is read from it
		}
		if value, ok := env[key]; ok {
			return value
		}
		if key == "SHUTDOWN_DELAY" {
			return "0s" // no load balancer to wait for, unless the test sets one
		}
		return ""
	}

	logR, logW := io.Pipe()
	var stderr strings.Builder
	// finished is closed once run has returned, with its error in runErr.
	// Closed rather than sent on, because Start and stop both wait for it.
	var runErr error
	finished := make(chan struct{})
	go func() {
		runErr = run(ctx, []string{"svc"}, getenv, logW, &stderr)
		_ = logW.Close()
		close(finished)
	}()

	var mu sync.Mutex
	var logs strings.Builder
	addrs := make(chan string, 1)
	scanned := make(chan struct{}) // closed once every line run wrote is in logs
	go func() {
		defer close(scanned)
		scanner := bufio.NewScanner(logR)
		for scanner.Scan() {
			line := scanner.Text()
			mu.Lock()
			logs.WriteString(line + "\n")
			mu.Unlock()

			var rec struct{ Msg, Message, Addr string }
			if json.Unmarshal([]byte(line), &rec) == nil && cmp.Or(rec.Msg, rec.Message) == "listening" {
				// Only the first is wanted, and the scanner must not block
				// on a second: run would block writing its next line.
				select {
				case addrs <- rec.Addr:
				default:
				}
			}
		}
		// A scanner stops at a line too long for it. Keep draining, or run
		// would block writing its next log line.
		_, _ = io.Copy(io.Discard, logR)
	}()

	svc := &Service{
		logs: func() string {
			mu.Lock()
			defer mu.Unlock()
			return logs.String()
		},
		// Once, because a test stops it and so does the cleanup.
		stop: sync.OnceValue(func() error {
			cancel()
			select {
			case <-finished:
				// run has returned, but its last lines may still be on their
				// way through the pipe.
				<-scanned
				return runErr
			case <-time.After(15 * time.Second):
				return errors.New("run did not return within 15s of cancel")
			}
		}),
	}
	t.Cleanup(func() { _ = svc.stop() })

	select {
	case svc.Addr = <-addrs:
	case <-finished:
		<-scanned
		t.Fatalf("run returned before listening: %v\n%s%s", runErr, svc.logs(), stderr.String())
	case <-time.After(15 * time.Second):
		t.Fatalf("run did not start listening within 15s:\n%s", svc.logs())
	}
	if err := waitForReady(t.Context(), 5*time.Second, svc.URL("/ready")); err != nil {
		t.Fatalf("%v\n%s", err, svc.logs())
	}
	return svc
}

// waitForReady polls endpoint until it answers 200, as a load balancer
// would.
func waitForReady(ctx context.Context, timeout time.Duration, endpoint string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s not ready: %w", endpoint, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
