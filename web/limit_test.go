package web

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	t.Parallel()
	l := newLimiter(2, time.Minute)
	now := time.Date(2026, 10, 29, 19, 0, 0, 0, time.UTC)
	for range 2 {
		if wait := l.Wait("a", now); wait != 0 {
			t.Fatalf("Wait with tries left = %v, want 0", wait)
		}
		l.Take("a", now)
	}
	if wait := l.Wait("a", now); wait != time.Minute {
		t.Errorf("Wait with none left = %v, want a minute", wait)
	}
	if wait := l.Wait("b", now); wait != 0 {
		t.Errorf("another client's Wait = %v, want 0", wait)
	}
	if wait := l.Wait("a", now.Add(30*time.Second)); wait != 30*time.Second {
		t.Errorf("Wait half way = %v, want 30s", wait)
	}
	if wait := l.Wait("a", now.Add(time.Minute)); wait != 0 {
		t.Errorf("Wait a minute later = %v, want 0", wait)
	}

	// Tries come back no more than burst, and a client with all of them
	// back is forgotten.
	later := now.Add(time.Hour)
	l.Take("b", later)
	if _, ok := l.buckets["a"]; ok {
		t.Error("a, with every try back, is still kept")
	}
	l.Take("a", later)
	l.Take("a", later)
	if wait := l.Wait("a", later); wait == 0 {
		t.Error("tries came back beyond the burst")
	}
}

func TestClient(t *testing.T) {
	t.Parallel()
	tests := []struct{ remote, want string }{
		{"192.0.2.1:1234", "192.0.2.1"},
		{"[::ffff:192.0.2.1]:1234", "192.0.2.1"},
		{"[2001:db8:1:2:3:4:5:6]:443", "2001:db8:1:2::/64"},
		{"[2001:db8:1:2:ffff::1]:443", "2001:db8:1:2::/64"},
		{"not an address", "not an address"},
	}
	for _, tt := range tests {
		r := httptest.NewRequestWithContext(t.Context(), "POST", "/raffle", nil)
		r.RemoteAddr = tt.remote
		if got := client(r); got != tt.want {
			t.Errorf("client(%q) = %q, want %q", tt.remote, got, tt.want)
		}
	}
}
