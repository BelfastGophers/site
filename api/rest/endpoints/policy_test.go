// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

package endpoints

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"belfastgophers.com/site/api/rest"
)

// The routing policy: routes are protected unless registered on the public
// group, and a protected route, in any group but rest.Public, must refuse a
// request without credentials.
// The test checks the routes policyRouter, in policy_hook_test.go, builds:
// the service's own, with each group's real middleware.

// TestProtectedRoutesRefuseAnonymous checks that every route outside the
// public group refuses a request with no credentials, which catches a group
// registered without authentication, whatever its name.
func TestProtectedRoutesRefuseAnonymous(t *testing.T) {
	t.Parallel()
	router := policyRouter(t)
	for _, route := range router.Routes() {
		if route.Group == rest.Public {
			continue
		}
		req := httptest.NewRequestWithContext(t.Context(), route.Method, policyRequestPath(route.Path), nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("%s %s, in group %s, answered %d to a request with no credentials, want 401 or 403: put authentication on the group, or make the route public deliberately", route.Method, route.Path, route.Group, rec.Code)
		}
	}
}

// The helpers' names start with policy because this file shares the package
// with the service's own tests, and a name either declared twice would not
// compile.
var (
	// policyEndMarker matches {$}, which only marks the end of a pattern.
	policyEndMarker = regexp.MustCompile(`\{\$\}$`)
	// policyWildcard matches one {name} or {name...} segment of a pattern.
	policyWildcard = regexp.MustCompile(`\{[^}]*\}`)
)

// policyRequestPath turns a route pattern into a path that matches it: {$}
// is dropped, and each wildcard is filled with a placeholder segment.
func policyRequestPath(pattern string) string {
	return policyWildcard.ReplaceAllLiteralString(policyEndMarker.ReplaceAllLiteralString(pattern, ""), "x")
}
