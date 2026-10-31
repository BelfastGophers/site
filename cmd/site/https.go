package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"belfastgophers.com/site/api/rest/errlog"
)

// certificates gets the site's certificates from Let's Encrypt, for the
// domain and its www, when they are first asked for, and renews them before
// they expire. They are kept in dir, so a restart does not ask for them
// again: Let's Encrypt limits how often it issues them.
func certificates(domain, dir string) *autocert.Manager {
	return &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		Cache:      autocert.DirCache(dir),
		HostPolicy: autocert.HostWhitelist(domain, "www."+domain),
	}
}

// toHTTPS sends every request to the same page on the domain over HTTPS, in
// one step whichever host it was for. Only the path and query come from the
// request, so it cannot send anyone to another site.
func toHTTPS(domain string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := url.URL{Scheme: "https", Host: domain, Path: r.URL.Path, RawQuery: r.URL.RawQuery}
		w.Header().Set("Location", target.String())
		w.WriteHeader(http.StatusMovedPermanently)
	})
}

// secure is the site served over HTTPS: a request for www is sent to the
// same page on the domain, and browsers are told to use HTTPS for the next
// year, so they no longer try plain HTTP first, for the domain and every
// name under it: none is served over plain HTTP.
func secure(domain string, next http.Handler) http.Handler {
	redirect := toHTTPS(domain)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		if r.Host == "www."+domain {
			redirect.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// servePlain serves h on addr, plain HTTP, until the function it returns is
// called, which stops it. It is the site's port 80: Let's Encrypt's checks,
// and the redirect to HTTPS.
func servePlain(ctx context.Context, addr string, h http.Handler) (stop func(), err error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for http: %w", err)
	}
	srv := &http.Server{
		Handler:           h,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	slog.InfoContext(ctx, "redirecting to https", "addr", ln.Addr().String())
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			slog.ErrorContext(ctx, "serving http", "err", errlog.Value(err))
		}
	}()
	return func() {
		// Nothing here is worth waiting long for: a redirect, or a check
		// Let's Encrypt tries again.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}
