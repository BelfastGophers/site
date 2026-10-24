// Scaffolded by httpgen. This file is yours: edit it to suit the service.

// Package endpoints holds the service's handlers and the list of its routes.
package endpoints

import (
	"net/http"

	"belfastgophers.com/site/api/rest"
	"belfastgophers.com/site/api/rest/health"
	"belfastgophers.com/site/web"
)

// AddRoutes maps the entire surface of the site in one place. protected is
// the organisers' group: its routes need an organiser's session.
//
// Every page is registered through web.Page, so that an error shows as a
// page rather than as JSON. Anything not matched here is the static
// content, so a route registered here takes the place of a static file at
// the same path.
//
// Request bodies are limited to the router's default. A route that takes
// more, such as an upload, raises its own limit where it is registered:
//
//	protected.Handle(http.MethodPost, "/api/v1/files", upload(), rest.MaxBody(50<<20))
//
// The server also stops reading a request after 30 seconds, which a large
// upload on a slow link outlasts. Give such a handler a later deadline of
// its own before it reads the body:
//
//	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(10 * time.Minute))
func AddRoutes(public, protected *rest.Group, deps Deps) {
	public.Handle(http.MethodGet, "/health", health.Live())
	public.Handle(http.MethodGet, "/ready", deps.Readiness.Handler(deps.Logger))

	site, signIn := deps.Site, deps.Auth
	public.Handle(http.MethodGet, "/", web.Static())
	public.Handle(http.MethodGet, "/{$}", web.Page(site.Home()))
	public.Handle(http.MethodGet, "/events/{id}/cover", web.Page(site.Cover()))
	public.Handle(http.MethodGet, "/sponsors", rest.Wrap(web.Info("sponsors", "Sponsors")))
	public.Handle(http.MethodGet, "/swag", rest.Wrap(web.Info("swag", "Swag")))
	public.Handle(http.MethodGet, "/videos", web.Page(site.Videos()))
	public.Handle(http.MethodGet, "/code-of-conduct", rest.Wrap(web.Info("code-of-conduct", "Code of Conduct")))
	public.Handle(http.MethodGet, "/privacy", rest.Wrap(web.Info("privacy", "Privacy")))
	public.Handle(http.MethodGet, "/speak", web.Page(site.Speak()))
	public.Handle(http.MethodPost, "/speak", web.Page(site.Propose()))
	public.Handle(http.MethodGet, "/speak/{$}", web.Redirect("/speak"))
	public.Handle(http.MethodGet, "/talk-ideas/{$}", web.Redirect("/speak"))
	public.Handle(http.MethodGet, "/raffle", web.Page(site.Raffle()))
	public.Handle(http.MethodPost, "/raffle", web.Page(site.EnterRaffle()))
	public.Handle(http.MethodGet, "/raffle/{$}", web.Redirect("/raffle"))
	public.Handle(http.MethodGet, "/auth/login", web.Page(signIn.Login()))
	public.Handle(http.MethodGet, "/auth/callback", web.Page(signIn.Callback(web.NotMember())))
	public.Handle(http.MethodPost, "/auth/logout", web.Page(signIn.Logout()))

	protected.Handle(http.MethodGet, "/organisers", web.Page(site.Organisers()))
	protected.Handle(http.MethodGet, "/organisers/events/{id}", web.Page(site.EventRaffle()))
	protected.Handle(http.MethodPost, "/organisers/events/{id}/draw", web.Page(site.Draw()))
	protected.Handle(http.MethodGet, "/organisers/proposals", web.Page(site.Proposals()))
}
