package web

import (
	"errors"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"belfastgophers.com/site/api/rest"
)

// limiter allows each client a number of tries, burst, which come back one
// every interval: a token bucket per client. The site keeps it in memory, so
// a restart forgets it, which costs nothing worth keeping.
//
// A handler asks Wait before the work and calls Take only for what counts
// against the client, such as a wrong raffle code, so that a form shown
// again to be corrected costs nothing.
type limiter struct {
	burst    float64
	interval time.Duration

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

// bucket is one client's tries left, as of when.
type bucket struct {
	tokens float64
	when   time.Time
}

func newLimiter(burst int, interval time.Duration) *limiter {
	return &limiter{burst: float64(burst), interval: interval, buckets: map[string]*bucket{}}
}

// Wait returns how long the client must wait for a try: zero if it has one
// now.
func (l *limiter) Wait(client string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	tokens := l.tokens(client, now)
	if tokens >= 1 {
		return 0
	}
	return time.Duration((1 - tokens) * float64(l.interval))
}

// Take spends one of the client's tries.
func (l *limiter) Take(client string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	b := &bucket{tokens: math.Max(l.tokens(client, now)-1, 0), when: now}
	l.buckets[client] = b
}

// tokens is how many tries the client has at now.
func (l *limiter) tokens(client string, now time.Time) float64 {
	b, ok := l.buckets[client]
	if !ok {
		return l.burst
	}
	return math.Min(b.tokens+float64(now.Sub(b.when))/float64(l.interval), l.burst)
}

// sweep forgets, at most once an interval, the clients whose tries have all
// come back, so the map holds only those who have used some lately.
func (l *limiter) sweep(now time.Time) {
	if now.Sub(l.swept) < l.interval {
		return
	}
	l.swept = now
	for client := range l.buckets {
		if l.tokens(client, now) >= l.burst {
			delete(l.buckets, client)
		}
	}
}

// client is who a request is limited as: its address, or for IPv6 the /64
// it is in, which is what one home or phone is given. The site serves the
// internet itself, so the address is the client's; behind a proxy it would
// be the proxy's, and every client would share one limit.
func client(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	if addr.Is6() {
		prefix, _ := addr.Prefix(64)
		return prefix.String()
	}
	return addr.String()
}

// tooMany is the 429 for a client out of tries, telling it when to come
// back.
func tooMany(w http.ResponseWriter, wait time.Duration, message string) error {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
	return rest.Status(http.StatusTooManyRequests, errors.New(message))
}
