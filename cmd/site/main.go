// Scaffolded by httpgen. This file is yours: edit it to suit the service.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"belfastgophers.com/site/api/rest"
	"belfastgophers.com/site/api/rest/endpoints"
	"belfastgophers.com/site/api/rest/errlog"
	"belfastgophers.com/site/api/rest/health"
	"belfastgophers.com/site/api/rest/logging"
	"belfastgophers.com/site/api/rest/middleware"
	"belfastgophers.com/site/api/rest/serve"
	"belfastgophers.com/site/api/rest/telemetry"
	"belfastgophers.com/site/auth"
	"belfastgophers.com/site/meetup"
	"belfastgophers.com/site/store"
	"belfastgophers.com/site/web"
	"belfastgophers.com/site/youtube"
)

// upkeepEvery is how often events and videos are brought in from Meetup and
// YouTube, and expired raffle entries and sessions deleted.
const upkeepEvery = time.Hour

func main() {
	err := run(context.Background(), os.Args, os.Getenv, os.Stdout, os.Stderr)
	switch {
	case errors.Is(err, errUsage):
		os.Exit(2) // as the flag package exits for a bad command line
	case err != nil:
		slog.Error("fatal", "err", errlog.Value(err))
		os.Exit(1)
	}
}

// errUsage marks a command line the flag package rejected. It has already
// printed why, with the usage, so main does not log it again.
var errUsage = errors.New("usage")

// run is the whole program. It takes operating system fundamentals as
// arguments so tests can call it directly with their own values. Logs go to
// stdout; flag usage and parse errors go to stderr, as the flag package's
// own default does.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	cfg, err := parseConfig(args, getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}

	logger, err := logging.New(stdout, cfg.logLevel, cfg.logFormat)
	if err != nil {
		return err
	}
	// Package-level slog calls, including those in libraries, use this
	// logger too.
	slog.SetDefault(logger)

	// Every request gets a span and HTTP metrics, and the process Go runtime
	// metrics, but nothing leaves the process until Setup is given an
	// exporter. To send them to an OpenTelemetry collector, for example:
	//
	//	spans, err := otlptracehttp.New(ctx) // reads OTEL_EXPORTER_OTLP_ENDPOINT
	//	if err != nil {
	//		return err
	//	}
	//	stopTelemetry, err := telemetry.Setup(ctx, telemetry.WithSpanExporter(spans))
	//
	// Metrics go the same way, through telemetry.WithMetricReader: an
	// sdkmetric.NewPeriodicReader around otlpmetrichttp.New(ctx) to push
	// them, or the Prometheus exporter for a /metrics endpoint to scrape.
	stopTelemetry, err := telemetry.Setup(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// After the server has stopped, so the last requests are sent.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := stopTelemetry(ctx); err != nil {
			logger.ErrorContext(ctx, "stopping telemetry", "err", errlog.Value(err))
		}
	}()

	// Defers run after the server has shut down, so in-flight requests
	// finish before the database is closed.
	st, err := store.Open(ctx, cfg.db)
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.ErrorContext(ctx, "closing database", "err", errlog.Value(err))
		}
	}()
	// For the calls to GitHub, Meetup and YouTube.
	client := &http.Client{Transport: middleware.Propagate(http.DefaultTransport), Timeout: 10 * time.Second}
	defer every(ctx, upkeepEvery,
		job{"meetup events", func(ctx context.Context) error {
			events, err := meetup.Events(ctx, client, cfg.meetupFeed)
			if err != nil {
				return err
			}
			return st.SyncEvents(ctx, events, time.Now())
		}},
		job{"youtube videos", func(ctx context.Context) error {
			videos, err := youtube.Videos(ctx, client, cfg.youtubeFeed)
			if err != nil {
				return err
			}
			return st.SaveVideos(ctx, videos)
		}},
		job{"prune", func(ctx context.Context) error {
			entries, sessions, err := st.Prune(ctx, time.Now())
			if entries+sessions > 0 {
				slog.InfoContext(ctx, "pruned", "raffle_entries", entries, "sessions", sessions)
			}
			return err
		}},
	)()

	deps := endpoints.Deps{
		Logger:    logger,
		Readiness: health.NewReadiness(map[string]health.Check{"db": st.Ping}),
		MaxBody:   cfg.maxBody,
		Site:      web.New(st),
		Auth: auth.New(auth.Config{
			ClientID:     cfg.githubClientID,
			ClientSecret: cfg.githubClientSecret,
			CallbackURL:  strings.TrimSuffix(cfg.baseURL, "/") + "/auth/callback",
			Org:          cfg.githubOrg,
			Client:       client,
		}, st),
	}

	server := serve.Config{
		Addr:      net.JoinHostPort(cfg.host, cfg.port),
		Handler:   endpoints.NewServer(deps),
		Logger:    logger,
		Readiness: deps.Readiness,
		DrainFor:  cfg.drainFor,
		PprofAddr: cfg.pprofAddr,
	}
	// With a domain, the site serves its own HTTPS, and plain HTTP only
	// redirects to it. Without, as in development, it is plain HTTP.
	if cfg.domain != "" {
		certs := certificates(cfg.domain, cfg.certDir)
		stop, err := servePlain(ctx, net.JoinHostPort(cfg.host, cfg.httpPort), certs.HTTPHandler(toHTTPS(cfg.domain)))
		if err != nil {
			return err
		}
		defer stop()
		server.TLSConfig = certs.TLSConfig()
		server.Handler = secure(cfg.domain, server.Handler)
	}
	return serve.Run(ctx, server)
}

// job is a piece of upkeep: bringing data in from Meetup or YouTube, or
// deleting what has expired.
type job struct {
	name string
	run  func(context.Context) error
}

// every runs the jobs now, then every interval, until the function it
// returns is called, which waits for them to stop. A job that fails is
// logged, and tried again next time; until then what it keeps in step stays
// as it was.
func every(ctx context.Context, interval time.Duration, jobs ...job) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			for _, j := range jobs {
				if err := j.run(ctx); err != nil && ctx.Err() == nil {
					slog.ErrorContext(ctx, "upkeep failed", "job", j.name, "err", errlog.Value(err))
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// config is the service's configuration.
type config struct {
	host, port          string
	logLevel, logFormat string
	maxBody             int64
	drainFor            time.Duration
	pprofAddr           string
	domain, certDir     string
	httpPort            string

	db                                 string
	baseURL                            string
	githubOrg                          string
	meetupFeed, youtubeFeed            string
	githubClientID, githubClientSecret string
}

// parseConfig reads the configuration from flags, each of which falls back
// to an environment variable named after it: -max-body-size to
// MAX_BODY_SIZE. A flag given on the command line wins over the environment.
//
// To add a setting, add a field to config and a flag for it here; its
// environment variable comes with it.
func parseConfig(args []string, getenv func(string) string, stderr io.Writer) (config, error) {
	name, flagArgs := "server", []string(nil)
	if len(args) > 0 {
		name, flagArgs = filepath.Base(args[0]), args[1:]
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		_, _ = fmt.Fprintf(flags.Output(), "Usage of %s:\n", name)
		flags.PrintDefaults()
		_, _ = fmt.Fprintln(flags.Output(), "\nEach flag falls back to an environment variable named after it: -max-body-size to MAX_BODY_SIZE.")
	}

	var cfg config
	flags.StringVar(&cfg.host, "host", "", "host to listen on; every interface when empty")
	flags.StringVar(&cfg.port, "port", "8080", "port to listen on")
	flags.StringVar(&cfg.logLevel, "log-level", "info", "log level: debug, info, warn or error")
	flags.StringVar(&cfg.logFormat, "log-format", "json", "log format: json or text")
	flags.Int64Var(&cfg.maxBody, "max-body-size", rest.DefaultMaxBody, "largest request body a route accepts, in bytes, unless the route sets its own")
	flags.DurationVar(&cfg.drainFor, "shutdown-delay", 5*time.Second, "how long /ready reports draining before the server stops accepting connections; at least the time a load balancer takes to notice")
	flags.StringVar(&cfg.pprofAddr, "pprof-addr", "", "address to serve runtime profiles on, such as localhost:6060; off when empty")
	flags.StringVar(&cfg.domain, "domain", "", "domain to serve HTTPS for, and its www, with certificates from Let's Encrypt: belfastgophers.com in production, with -port 443; plain HTTP when empty")
	flags.StringVar(&cfg.certDir, "cert-dir", "certs", "directory the Let's Encrypt certificates are kept in, when -domain is set; it must survive restarts")
	flags.StringVar(&cfg.httpPort, "http-port", "80", "port plain HTTP is redirected to HTTPS from, when -domain is set")
	flags.StringVar(&cfg.db, "db", "site.db", "path of the SQLite database, created if it does not exist")
	flags.StringVar(&cfg.baseURL, "base-url", "http://localhost:8080", "address the site is reached at, for GitHub to return organisers to: https://belfastgophers.com in production")
	flags.StringVar(&cfg.githubOrg, "github-org", "BelfastGophers", "GitHub organisation whose members are organisers")
	flags.StringVar(&cfg.githubClientID, "github-client-id", "", "client id of the GitHub App organisers sign in with")
	flags.StringVar(&cfg.meetupFeed, "meetup-feed", "https://www.meetup.com/belfast-gophers/events/ical/", "the Meetup group's iCalendar feed, the events come from")
	flags.StringVar(&cfg.youtubeFeed, "youtube-feed", "https://www.youtube.com/feeds/videos.xml?channel_id=UClH4m_4FPMOYYw7hGKHS1UA", "the YouTube channel's feed, the videos come from")

	if err := fromEnv(flags, getenv); err != nil {
		return config{}, err
	}
	if err := flags.Parse(flagArgs); err != nil {
		return config{}, fmt.Errorf("%w: %w", errUsage, err)
	}
	if cfg.maxBody <= 0 {
		return config{}, fmt.Errorf("max body size %d: want a positive number of bytes", cfg.maxBody)
	}
	// A secret, so read from the environment only: a flag's value shows in
	// ps to anyone on the machine.
	cfg.githubClientSecret = getenv("GITHUB_CLIENT_SECRET")
	if cfg.githubClientID == "" || cfg.githubClientSecret == "" {
		return config{}, errors.New("GITHUB_CLIENT_ID and GITHUB_CLIENT_SECRET are required: the GitHub App organisers sign in with")
	}
	return cfg, nil
}

// fromEnv sets each flag from its environment variable, when that is set:
// HOST for -host, LOG_LEVEL for -log-level. Each value is checked as the
// flag's own would be, so a bad one names the variable it came from.
func fromEnv(flags *flag.FlagSet, getenv func(string) string) error {
	var errs []error
	flags.VisitAll(func(f *flag.Flag) {
		env := strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
		if value := getenv(env); value != "" {
			if err := flags.Set(f.Name, value); err != nil {
				errs = append(errs, fmt.Errorf("%s=%q: %w", env, value, err))
			}
		}
	})
	return errors.Join(errs...)
}
