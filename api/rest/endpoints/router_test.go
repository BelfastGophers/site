// Scaffolded by httpgen. This file is yours: edit it to suit the service.

package endpoints

import (
	"encoding/xml"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"belfastgophers.com/site/api/rest/middleware"
	"belfastgophers.com/site/auth"
	"belfastgophers.com/site/store"
	"belfastgophers.com/site/web"
)

// testDeps is the site's dependencies, with a database of the test's own.
func testDeps(t *testing.T) Deps {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "site.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return Deps{
		Logger: slog.New(slog.DiscardHandler),
		Site:   web.New(st),
		Auth:   auth.New(auth.Config{ClientID: "client", Org: "BelfastGophers"}, st),
	}
}

// newTestServer builds the service as main does, middleware and all. Set the
// dependencies a test uses; the rest get their defaults.
func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	return NewServer(testDeps(t))
}

// get sends a GET through the whole server.
func get(t *testing.T, srv http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestHealth requests the health checks through the whole server, so it
// also proves the middleware stack is in place.
func TestHealth(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	for _, path := range []string{"/health", "/ready"} {
		rec := get(t, srv, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", path, rec.Code, http.StatusOK)
		}
		if rec.Header().Get(middleware.RequestIDHeader) == "" {
			t.Errorf("GET %s has no %s header: the request did not pass through the stack", path, middleware.RequestIDHeader)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s has no security headers: SecurityHeaders is not in the stack", path)
		}
	}
}

// TestPages checks that each kind of page is reached through the whole
// server: static content, its old addresses, pages from the database, and
// the organisers' pages, which refuse anyone without a session.
func TestPages(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	tests := []struct {
		path     string
		status   int
		contains string
	}{
		{"/", http.StatusOK, "Belfast-based community"},
		{"/styles/belfastgophers.css", http.StatusOK, "Game of Thrones"},
		{"/speak", http.StatusOK, "Talk title"},
		{"/swag", http.StatusOK, "Visit our Redbubble store"},
		{"/videos", http.StatusOK, "YouTube channel"},
		{"/sponsors", http.StatusOK, "info@belfastgophers.com"},
		{"/code-of-conduct", http.StatusOK, "Go Community Code of"},
		{"/privacy", http.StatusOK, "deleted automatically a week after"},
		{"/robots.txt", http.StatusOK, "Sitemap: https://belfastgophers.com/sitemap.xml"},
		{"/sitemap.xml", http.StatusOK, "<loc>https://belfastgophers.com/speak</loc>"},
		{"/llms.txt", http.StatusOK, "# Belfast Gophers"},
		{"/speak/", http.StatusMovedPermanently, ""},
		{"/talk-ideas/", http.StatusMovedPermanently, ""},
		{"/raffle", http.StatusOK, "No raffle tonight"},
		{"/raffle/", http.StatusMovedPermanently, ""},
		{"/organisers", http.StatusUnauthorized, "Sign in with GitHub"},
		{"/organisers/proposals", http.StatusUnauthorized, "Sign in with GitHub"},
		{"/no-such-page", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		rec := get(t, srv, tt.path)
		if rec.Code != tt.status {
			t.Errorf("GET %s status = %d, want %d", tt.path, rec.Code, tt.status)
		}
		if !strings.Contains(rec.Body.String(), tt.contains) {
			t.Errorf("GET %s body does not contain %q", tt.path, tt.contains)
		}
	}
}

// TestCrossOriginRefused checks that a form another site posts, with an
// organiser's cookies, is refused before it reaches the site.
func TestCrossOriginRefused(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/organisers/events/1/draw", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestContentSecurityPolicy checks pages, and the errors shown as pages, carry
// the site's content security policy.
func TestContentSecurityPolicy(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	for _, path := range []string{"/", "/videos", "/no-such-page"} {
		if got := get(t, srv, path).Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") {
			t.Errorf("%s Content-Security-Policy = %q, want the site's policy", path, got)
		}
	}
}

// TestSitemap checks every page the sitemap lists is there.
func TestSitemap(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	var sitemap struct {
		URLs []string `xml:"url>loc"`
	}
	if err := xml.Unmarshal(get(t, srv, "/sitemap.xml").Body.Bytes(), &sitemap); err != nil {
		t.Fatal(err)
	}
	if len(sitemap.URLs) == 0 {
		t.Fatal("the sitemap lists no pages")
	}
	for _, loc := range sitemap.URLs {
		path := strings.TrimPrefix(loc, "https://belfastgophers.com")
		if rec := get(t, srv, path); rec.Code != http.StatusOK {
			t.Errorf("sitemap lists %s, which answers %d", loc, rec.Code)
		}
	}
}
