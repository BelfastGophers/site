// Scaffolded by httpgen. This file is yours: edit it to suit the service.

package endpoints

import (
	"testing"

	"belfastgophers.com/site/api/rest"
)

// policyRouter builds the service's routes for the managed policy test, the
// way the service itself does: through newRouter, with each group's real
// middleware. The policy test checks that every protected route refuses a
// request without credentials. Set any dependency the routes' middleware needs, such as the
// authentication's.
func policyRouter(t *testing.T) *rest.Router {
	t.Helper()
	return newRouter(testDeps(t))
}
