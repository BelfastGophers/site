// Scaffolded by httpgen. This file is yours: edit it to suit the service.

package main

import (
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"belfastgophers.com/site/api/rest"
	"belfastgophers.com/site/api/rest/servetest"
)

// siteEnv is the environment the site needs to start, with a database of
// the test's own, and extra on top.
func siteEnv(t *testing.T, extra map[string]string) map[string]string {
	t.Helper()
	// Empty feeds for Meetup and YouTube, so the test calls neither.
	feeds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/youtube" {
			_, _ = io.WriteString(w, `<feed xmlns="http://www.w3.org/2005/Atom"></feed>`)
			return
		}
		_, _ = io.WriteString(w, "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n")
	}))
	t.Cleanup(feeds.Close)
	env := map[string]string{
		"MEETUP_FEED":          feeds.URL + "/meetup",
		"YOUTUBE_FEED":         feeds.URL + "/youtube",
		"DB":                   filepath.Join(t.TempDir(), "site.db"),
		"GITHUB_CLIENT_ID":     "client",
		"GITHUB_CLIENT_SECRET": "secret",
	}
	maps.Copy(env, extra)
	return env
}

// TestRun drives the whole program: configuration, middleware, routing and
// graceful shutdown, through the same layers a real caller goes through.
// Pass settings in the map, not with t.Setenv, so tests can run in parallel.
func TestRun(t *testing.T) {
	t.Parallel()
	svc := servetest.Start(t, run, siteEnv(t, nil))

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, svc.URL("/health"), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Error(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	// A static page, a page from the database, and an organisers' page,
	// refused without a session.
	for path, want := range map[string]int{"/": http.StatusOK, "/speak": http.StatusOK, "/organisers": http.StatusUnauthorized} {
		if got := status(t, svc.URL(path)); got != want {
			t.Errorf("GET %s status = %d, want %d", path, got, want)
		}
	}

	if err := svc.Stop(); err != nil {
		t.Fatalf("run returned %v, want nil after cancel", err)
	}

	logs := svc.Logs()
	// The request line carries the caller's trace, so this service's logs
	// can be found with the caller's, and the pattern the router matched.
	for _, want := range []string{`"trace_id":"` + traceID + `"`, `"pattern":"GET /health"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %s:\n%s", want, logs)
		}
	}
}

// TestRunDrains checks that run hands serve.Run the readiness /ready
// answers from and the shutdown delay: once stopping, the service reports
// draining for the delay instead of closing at once.
func TestRunDrains(t *testing.T) {
	t.Parallel()
	svc := servetest.Start(t, run, siteEnv(t, map[string]string{"SHUTDOWN_DELAY": "2s"}))
	stopped := make(chan error, 1)
	go func() { stopped <- svc.Stop() }()

	deadline := time.Now().Add(2 * time.Second)
	for status(t, svc.URL("/ready")) != http.StatusServiceUnavailable {
		if time.Now().After(deadline) {
			t.Fatal("/ready never reported draining while stopping")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := <-stopped; err != nil {
		t.Errorf("run returned %v, want nil", err)
	}
}

// status sends a GET to url and returns the response's status, or 0 if
// there was no response.
func status(t *testing.T, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestParseConfig checks where each setting comes from: its default, its
// environment variable, or its flag, which wins.
func TestParseConfig(t *testing.T) {
	t.Parallel()
	github := map[string]string{"GITHUB_CLIENT_ID": "client", "GITHUB_CLIENT_SECRET": "secret"}
	env := map[string]string{"PORT": "9000", "SHUTDOWN_DELAY": "1s", "LOG_LEVEL": "debug", "DB": "/var/lib/site/site.db"}
	maps.Copy(env, github)
	cfg, err := parseConfig([]string{"svc", "-log-level", "warn"}, func(key string) string { return env[key] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := config{
		port: "9000", logLevel: "warn", logFormat: "json", maxBody: rest.DefaultMaxBody, drainFor: time.Second,
		db: "/var/lib/site/site.db", baseURL: "http://localhost:8080", githubOrg: "BelfastGophers", meetupFeed: "https://www.meetup.com/belfast-gophers/events/ical/", youtubeFeed: "https://www.youtube.com/feeds/videos.xml?channel_id=UClH4m_4FPMOYYw7hGKHS1UA",
		githubClientID: "client", githubClientSecret: "secret",
	}
	// DeepEqual rather than ==, so the test still compiles once config
	// holds a list.
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("config = %+v, want %+v", cfg, want)
	}

	// With only GitHub set, every setting has its default. A drain long
	// enough for a load balancer to notice is the production default.
	cfg, err = parseConfig([]string{"svc"}, func(key string) string { return github[key] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want = config{
		port: "8080", logLevel: "info", logFormat: "json", maxBody: rest.DefaultMaxBody, drainFor: 5 * time.Second,
		db: "site.db", baseURL: "http://localhost:8080", githubOrg: "BelfastGophers", meetupFeed: "https://www.meetup.com/belfast-gophers/events/ical/", youtubeFeed: "https://www.youtube.com/feeds/videos.xml?channel_id=UClH4m_4FPMOYYw7hGKHS1UA",
		githubClientID: "client", githubClientSecret: "secret",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("defaults = %+v, want %+v", cfg, want)
	}

	// Organisers cannot sign in without the GitHub app, so the site does
	// not start. The secret comes only from the environment.
	for _, args := range [][]string{{"svc"}, {"svc", "-github-client-id", "client"}} {
		if _, err := parseConfig(args, func(string) string { return "" }, io.Discard); err == nil || !strings.Contains(err.Error(), "GITHUB_CLIENT_SECRET") {
			t.Errorf("%v without GitHub: err = %v, want one naming GITHUB_CLIENT_SECRET", args, err)
		}
	}

	// A bad value names the variable it came from.
	bad := func(string) string { return "soon" }
	if _, err := parseConfig([]string{"svc"}, bad, io.Discard); err == nil || !strings.Contains(err.Error(), "SHUTDOWN_DELAY") {
		t.Errorf("err = %v, want one naming SHUTDOWN_DELAY", err)
	}

	// An unknown flag is reported by the flag package, with the usage, so
	// main exits without logging it a second time.
	var stderr strings.Builder
	if _, err := parseConfig([]string{"svc", "-bogus"}, func(string) string { return "" }, &stderr); !errors.Is(err, errUsage) {
		t.Errorf("err = %v, want errUsage", err)
	}
	if !strings.Contains(stderr.String(), "-bogus") {
		t.Errorf("stderr = %q, want the flag package's message", stderr.String())
	}
}
