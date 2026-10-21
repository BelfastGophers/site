// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

// Package health answers the two questions a platform asks of a service: is
// the process up, and should it be sent traffic.
package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"belfastgophers.com/site/api/rest"
	"belfastgophers.com/site/api/rest/errlog"
)

// Live reports that the process is up. It checks nothing else, so a failing
// dependency never gets a healthy process restarted. Point a liveness probe
// at it.
func Live() rest.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) error {
		return rest.Encode(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// Check reports whether one dependency can serve, returning an error if it
// cannot. (*sql.DB).PingContext is one.
type Check func(ctx context.Context) error

// checkTimeout bounds the whole probe: the checks run at once, and one that
// has not answered by then counts as failed, whether or not it honours its
// context. It is below Kubernetes' default probe timeout of one second, so a
// hung dependency makes the service unready rather than making the probe
// itself time out.
const checkTimeout = 800 * time.Millisecond

// reuseFor is how long the result of running the checks answers further
// probes. /ready is public, so without it every request would reach the
// service's dependencies; probes come seconds apart, so it hides nothing.
const reuseFor = time.Second

// Readiness reports whether the service should be sent traffic: every check
// passes, and it is not shutting down.
//
// However many probes arrive, the checks run at most once a second: probes
// that arrive while they run wait for that run, and those that arrive just
// after are given its result. A check that has not returned from its last run
// is not started again, and counts as failing.
type Readiness struct {
	checks   map[string]Check
	draining atomic.Bool
	reuseFor time.Duration // reuseFor, or 0 in tests that change a check's answer

	mu sync.Mutex
	// failed holds the checks that failed at the last run. Probes come
	// every few seconds, so a failure is logged when it starts and when it
	// clears rather than on every probe while it lasts.
	failed  map[string]bool
	running map[string]bool // checks whose last run has not returned
	current *round          // the run in progress, if any
	last    *round          // the last run to finish
}

// round is one run of the checks, which every probe arriving during it, or
// within reuseFor of its end, is answered from.
type round struct {
	done    chan struct{} // closed when failing is set
	failing []string
	ended   time.Time
}

// NewReadiness returns a Readiness that runs checks, keyed by the name a
// failure is reported under.
func NewReadiness(checks map[string]Check) *Readiness {
	return &Readiness{
		checks:   maps.Clone(checks),
		reuseFor: reuseFor,
		failed:   map[string]bool{},
		running:  map[string]bool{},
	}
}

// Drain marks the service as shutting down, so that load balancers stop
// sending it new requests while it finishes the ones it has.
func (rd *Readiness) Drain() { rd.draining.Store(true) }

// failing returns the names of the checks that fail, running them unless a
// run in progress or just finished can answer. Why they fail goes to the
// log, not the response: this endpoint is public.
func (rd *Readiness) failing(probe context.Context, logger *slog.Logger) []string {
	rd.mu.Lock()
	if last := rd.last; last != nil && time.Since(last.ended) < rd.reuseFor {
		rd.mu.Unlock()
		return last.failing
	}
	if cur := rd.current; cur != nil {
		rd.mu.Unlock()
		select {
		case <-cur.done:
			return cur.failing
		case <-probe.Done():
			return nil // the prober has gone, so nobody needs the answer
		}
	}
	cur := &round{done: make(chan struct{})}
	rd.current = cur
	rd.mu.Unlock()

	// Run for every probe waiting, not only this one: its prober hanging
	// up must not cut the checks short, which would read as a failure.
	ctx := context.WithoutCancel(probe)
	names := slices.Sorted(maps.Keys(rd.checks))
	errs := rd.runChecks(ctx, names)
	rd.record(ctx, logger, cur, names, errs)
	return cur.failing
}

// errStillRunning is the failure of a check whose last run has not returned.
var errStillRunning = errors.New("its last run has not returned")

// runChecks runs the named checks at once and returns their errors, in the
// same order. A check that has not answered within checkTimeout counts as
// failed, and one that panics fails rather than stopping the process.
func (rd *Readiness) runChecks(probe context.Context, names []string) []error {
	ctx, cancel := context.WithTimeout(probe, checkTimeout)
	defer cancel()

	type answer struct {
		i   int
		err error
	}
	// Buffered, so a check that answers after the probe gave up on it can
	// still finish. One that never answers is not started again, so a hung
	// dependency costs one goroutine per check rather than one per probe.
	answers := make(chan answer, len(names))
	errs := make([]error, len(names))
	started := 0
	for i, name := range names {
		rd.mu.Lock()
		hung := rd.running[name]
		rd.running[name] = true
		rd.mu.Unlock()
		if hung {
			errs[i] = errStillRunning
			continue
		}
		errs[i] = fmt.Errorf("no answer within %s", checkTimeout)
		started++
		go func() {
			err := errors.New("check exited without answering")
			// One defer, in this order: the service's own goroutine, so a
			// panic is recovered here; then the check is marked as returned,
			// before its answer is sent, so the next run cannot find it still
			// running once this one has its answer.
			defer func() {
				if cause := recover(); cause != nil {
					err = fmt.Errorf("check panicked: %v", cause)
				}
				rd.mu.Lock()
				rd.running[name] = false
				rd.mu.Unlock()
				answers <- answer{i, err}
			}()
			err = rd.checks[name](ctx)
		}()
	}
	for range started {
		select {
		case a := <-answers:
			errs[a.i] = a.err
		case <-ctx.Done():
			return errs
		}
	}
	return errs
}

// record remembers which checks fail, logs each one that started failing or
// recovered since the last run, and ends cur with the names of those failing.
func (rd *Readiness) record(ctx context.Context, logger *slog.Logger, cur *round, names []string, errs []error) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	var failing []string
	for i, name := range names {
		switch {
		case errs[i] != nil && !rd.failed[name]:
			logger.ErrorContext(ctx, "readiness check failed", "check", name, "err", errlog.Value(errs[i]))
		case errs[i] == nil && rd.failed[name]:
			logger.InfoContext(ctx, "readiness check recovered", "check", name)
		}
		rd.failed[name] = errs[i] != nil
		if errs[i] != nil {
			failing = append(failing, name)
		}
	}
	cur.failing, cur.ended = failing, time.Now()
	rd.current, rd.last = nil, cur
	close(cur.done)
}

// Handler answers 200 when the service should be sent traffic and 503 when
// it should not: while draining, or while any check fails. Point a readiness
// probe, or a load balancer's health check, at it.
func (rd *Readiness) Handler(logger *slog.Logger) rest.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if rd.draining.Load() {
			return rest.Encode(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		}
		if failing := rd.failing(r.Context(), logger); len(failing) > 0 {
			return rest.Encode(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "failing": failing})
		}
		return rest.Encode(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}
