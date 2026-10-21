// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.
//
// Tracing, through OpenTelemetry: a span for every request, joined to the
// caller's trace, and the trace passed on to the services this one calls.
// Where the spans go is set up by package telemetry.

package middleware

import (
	"net/http"
	"slices"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// TraceParentHeader carries a W3C trace context between services:
// https://www.w3.org/TR/trace-context/. Load balancers, gateways and
// OpenTelemetry already send and read it.
const TraceParentHeader = "traceparent"

// TraceStateHeader carries vendor data that travels with traceparent. The
// specification requires it to be passed on, unchanged, by any service that
// joins the trace.
const TraceStateHeader = "tracestate"

// Trace gives every request a span, joined to the caller's W3C trace when it
// sent one, and records the standard HTTP server metrics: request duration,
// and request and response sizes. The span is named after the route the
// router matched, such as "GET /api/v1/things/{id}", as are the metrics.
//
// Requests for the untraced paths, such as health probes, which arrive every
// few seconds and say nothing, get no span or metrics. They still join the
// caller's trace, so their logs carry its trace id.
//
// It uses the process's tracer and meter providers, which telemetry.Setup
// installs. Middleware between Trace and the router must pass the request
// on rather than a copy of it, or the span cannot be named after the route.
func Trace(next http.Handler, untraced ...string) http.Handler {
	return traced(next, untraced)
}

// traced is Trace with otelhttp's options, so that tests can give it
// providers of their own.
func traced(next http.Handler, untraced []string, opts ...otelhttp.Option) http.Handler {
	spans := otelhttp.NewHandler(next, "", opts...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(untraced, r.URL.Path) {
			spans.ServeHTTP(w, r)
			return
		}
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Propagate returns a transport that passes the request's trace and request
// id on to the services it calls, so their logs can be found with this
// one's, and records a span for each call. Use it for every client that
// calls another service:
//
//	client := &http.Client{Transport: middleware.Propagate(http.DefaultTransport)}
//	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
//
// The outgoing request must carry the incoming request's context, which is
// where the trace is.
func Propagate(base http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(requestIDTransport{base})
}

// requestIDTransport passes the request id on.
type requestIDTransport struct{ base http.RoundTripper }

func (t requestIDTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	id := RequestIDFrom(req.Context())
	if id == "" {
		return t.base.RoundTrip(req)
	}
	// A RoundTripper must not change the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set(RequestIDHeader, id)
	return t.base.RoundTrip(req)
}
