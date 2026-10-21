package web

import "net/http"

// policy is the site's content security policy. Everything a page loads is
// the site's own, but for the YouTube players on /videos, so a script that
// slipped into a page could not run, nor send what it read anywhere else.
// No page may be framed, and forms post only to the site.
const policy = "default-src 'self'; " +
	"frame-src https://www.youtube-nocookie.com; " +
	"frame-ancestors 'none'; " +
	"form-action 'self'; " +
	"base-uri 'self'; " +
	"object-src 'none'"

// ContentSecurityPolicy sets the site's content security policy on every
// response.
func ContentSecurityPolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", policy)
		next.ServeHTTP(w, r)
	})
}
