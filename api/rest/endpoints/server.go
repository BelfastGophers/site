// Scaffolded by httpgen. This file is yours: edit it to suit the service.

package endpoints

import (
	"cmp"
	"log/slog"
	"net/http"

	"belfastgophers.com/site/api/rest"
	"belfastgophers.com/site/api/rest/health"
	"belfastgophers.com/site/api/rest/middleware"
	"belfastgophers.com/site/auth"
	"belfastgophers.com/site/web"
)

// Deps is everything the service's routes and middleware need, built in
// run. To add a dependency, such as a database, add a field here, set it in
// run, and use it in AddRoutes.
//
// A field left unset gets a default, so a test sets only the fields it uses.
// The cost is that a dependency run forgets to set is not a compile error,
// so check new ones through run, with servetest, as TestRunDrains checks
// Readiness.
type Deps struct {
	// Logger is the service's logger. Unset, it is slog.Default().
	Logger *slog.Logger

	// Readiness answers /ready. Unset, it is one with no checks.
	Readiness *health.Readiness

	// MaxBody is the default limit on request bodies, in bytes. A route
	// that needs more, or less, passes rest.MaxBody to Handle in AddRoutes.
	// Unset, it is rest.DefaultMaxBody.
	MaxBody int64

	// Site is the site's pages. It has no default: every test that
	// requests a page sets it.
	Site *web.Site

	// Auth signs organisers in, and guards their pages. It has no default.
	Auth *auth.Auth
}

// withDefaults fills in the fields left unset.
func (d Deps) withDefaults() Deps {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Readiness == nil {
		d.Readiness = health.NewReadiness(nil)
	}
	d.MaxBody = cmp.Or(d.MaxBody, rest.DefaultMaxBody)
	return d
}

// NewServer returns the whole service as one handler: every route, behind
// every middleware. main serves it and tests call it, so a test request takes
// the same path through the service as a real one.
func NewServer(deps Deps) http.Handler {
	deps = deps.withDefaults()
	return stack(deps, newRouter(deps))
}

// stack is the middleware every request passes through, outermost first: a
// request id, so the logs of one request can be found together; a span for
// the request, in the caller's trace, so its logs and spans can be found
// with those of the services around it, leaving the health probes untraced;
// security headers, with the site's content security policy; a log line per
// request; protection from cross-site requests, inside the log line so
// refusals are logged; and recovery, so a panic is answered with a 500 that
// is logged like any other response.
//
// The cross-origin protection refuses a POST another site's page makes the
// browser send, such as one that would draw the raffle with an organiser's
// session: forms are only accepted from the site's own pages.
//
// The middleware is managed by httpgen, in the middleware package, so fixes
// to it arrive with a sync. Which of it runs, and in what order, is the
// service's choice, made here. Middleware of the service's own goes in a
// package of its own. Anything that should apply only to authenticated
// routes belongs on the protected group instead, in newRouter.
//
// Middleware between Logging and the router must pass the request on rather
// than a copy of it, or Logging cannot see the route pattern the router
// matched.
func stack(deps Deps, next http.Handler) http.Handler {
	// TODO: consider, when the service needs them:
	//   - http.TimeoutHandler on slow routes. A server-wide write timeout
	//     would break streaming responses, so put it where it belongs.
	next = middleware.Recover(deps.Logger, next)
	next = http.NewCrossOriginProtection().Handler(next)
	next = middleware.Logging(deps.Logger, next)
	next = web.ContentSecurityPolicy(next)
	next = middleware.SecurityHeaders(next)
	next = middleware.Trace(next, "/health", "/ready")
	return middleware.RequestID(next)
}

// newRouter registers every route on its group, with the group's middleware.
// It is separate from NewServer so tests can list what it registered.
func newRouter(deps Deps) *rest.Router {
	deps = deps.withDefaults()
	router := rest.NewRouter(deps.Logger, rest.MaxBody(deps.MaxBody))
	public := router.Group(rest.Public)
	// The organisers' pages: anyone without a session gets the sign-in
	// page, with 401, which the policy test checks.
	protected := router.Group("organisers", deps.Auth.RequireOrganiser(web.SignIn()))
	AddRoutes(public, protected, deps)
	return router
}
