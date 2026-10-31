package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRedirects checks where each request ends up: plain HTTP on the domain
// or its www goes to HTTPS on the domain in one step, as does HTTPS on www,
// while HTTPS on the domain is the site, told to stay on HTTPS. Let's
// Encrypt's checks are answered, not redirected.
func TestRedirects(t *testing.T) {
	t.Parallel()
	site := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	plain := certificates("belfastgophers.com", t.TempDir()).HTTPHandler(toHTTPS("belfastgophers.com"))
	https := secure("belfastgophers.com", site)
	tests := []struct {
		name     string
		h        http.Handler
		url      string
		status   int
		location string
		hsts     bool
	}{
		{"http", plain, "http://belfastgophers.com/speak?x=1", http.StatusMovedPermanently, "https://belfastgophers.com/speak?x=1", false},
		{"http www", plain, "http://www.belfastgophers.com/videos", http.StatusMovedPermanently, "https://belfastgophers.com/videos", false},
		{"acme", plain, "http://belfastgophers.com/.well-known/acme-challenge/token", http.StatusNotFound, "", false},
		{"https www", https, "https://www.belfastgophers.com/swag", http.StatusMovedPermanently, "https://belfastgophers.com/swag", true},
		{"https", https, "https://belfastgophers.com/swag", http.StatusTeapot, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			tt.h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.url, nil))
			if rec.Code != tt.status || rec.Header().Get("Location") != tt.location {
				t.Errorf("GET %s = %d to %q, want %d to %q", tt.url, rec.Code, rec.Header().Get("Location"), tt.status, tt.location)
			}
			hsts := rec.Header().Get("Strict-Transport-Security")
			if got := hsts != ""; got != tt.hsts {
				t.Errorf("GET %s Strict-Transport-Security set = %t, want %t", tt.url, got, tt.hsts)
			}
			if tt.hsts && hsts != "max-age=31536000; includeSubDomains" {
				t.Errorf("GET %s Strict-Transport-Security = %q, want a year, with subdomains", tt.url, hsts)
			}
		})
	}
}
