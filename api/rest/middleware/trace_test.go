// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// TestMain installs W3C propagation, as telemetry.Setup does. Each test
// gives Trace providers of its own, so that its spans are its own.
func TestMain(m *testing.M) {
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	os.Exit(m.Run())
}

const (
	callerTrace  = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpan   = "00f067aa0ba902b7"
	callerParent = "00-" + callerTrace + "-" + callerSpan + "-01"
)

// tracing is Trace with providers that keep what it records in memory.
type tracing struct {
	spans   *tracetest.InMemoryExporter
	metrics *sdkmetric.ManualReader
	opts    []otelhttp.Option
}

func newTracing(t *testing.T) *tracing {
	t.Helper()
	tr := &tracing{spans: tracetest.NewInMemoryExporter(), metrics: sdkmetric.NewManualReader()}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(tr.spans))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(tr.metrics))
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		_ = tp.Shutdown(ctx)
		_ = mp.Shutdown(ctx)
	})
	tr.opts = []otelhttp.Option{otelhttp.WithTracerProvider(tp), otelhttp.WithMeterProvider(mp)}
	return tr
}

// middleware returns Trace, with the test's providers, leaving /health
// untraced.
func (tr *tracing) middleware(next http.Handler) http.Handler {
	return traced(next, []string{"/health"}, tr.opts...)
}

// serve sends a GET for path, with traceparent if it is set, through Trace
// to a router with a /things/{id} route, and returns the context the
// handler saw.
func (tr *tracing) serve(t *testing.T, path, traceparent string) context.Context {
	t.Helper()
	var ctx context.Context
	mux := http.NewServeMux()
	see := func(_ http.ResponseWriter, r *http.Request) { ctx = r.Context() }
	mux.HandleFunc("GET /things/{id}", see)
	mux.HandleFunc("GET /health", see)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	if traceparent != "" {
		req.Header.Set(TraceParentHeader, traceparent)
	}
	tr.middleware(mux).ServeHTTP(httptest.NewRecorder(), req)
	return ctx
}

// TestTrace checks that a request gets a span of this service's own, named
// after its route, in the caller's trace when the caller sent a valid one
// and in a new trace when it did not.
func TestTrace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, header string
		joined       bool
	}{
		{"valid joined", callerParent, true},
		{"missing started", "", false},
		{"unknown version", "ff-" + callerTrace + "-" + callerSpan + "-01", false},
		{"zero trace", "00-" + strings.Repeat("0", 32) + "-" + callerSpan + "-01", false},
		{"uppercase", "00-" + strings.ToUpper(callerTrace) + "-" + callerSpan + "-01", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tr := newTracing(t)
			ctx := tr.serve(t, "/things/1", tt.header)

			spans := tr.spans.GetSpans()
			if len(spans) != 1 {
				t.Fatalf("recorded %d spans, want 1", len(spans))
			}
			span := spans[0]
			if span.Name != "GET /things/{id}" {
				t.Errorf("span name = %q, want the route", span.Name)
			}
			if got := trace.SpanContextFromContext(ctx); got.SpanID() != span.SpanContext.SpanID() || got.IsRemote() {
				t.Errorf("handler's span = %v, want the recorded span %v", got.SpanID(), span.SpanContext.SpanID())
			}
			if joined := span.SpanContext.TraceID().String() == callerTrace; joined != tt.joined {
				t.Errorf("trace id = %s, want joined = %v", span.SpanContext.TraceID(), tt.joined)
			}
			if tt.joined && span.Parent.SpanID().String() != callerSpan {
				t.Errorf("parent = %s, want the caller's span %s", span.Parent.SpanID(), callerSpan)
			}
		})
	}
}

// TestTraceMetrics checks that a traced request is counted by route.
func TestTraceMetrics(t *testing.T) {
	t.Parallel()
	tr := newTracing(t)
	tr.serve(t, "/things/1", "")

	var rm metricdata.ResourceMetrics
	if err := tr.metrics.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "http.server.request.duration" {
				continue
			}
			for _, point := range m.Data.(metricdata.Histogram[float64]).DataPoints {
				if route, _ := point.Attributes.Value("http.route"); route.AsString() == "/things/{id}" {
					return
				}
			}
		}
	}
	t.Errorf("no http.server.request.duration for route /things/{id} in %+v", rm)
}

// TestTraceUntraced checks that an untraced path gets no span, but still
// joins the caller's trace, so its logs carry the trace id.
func TestTraceUntraced(t *testing.T) {
	t.Parallel()
	tr := newTracing(t)
	ctx := tr.serve(t, "/health", callerParent)

	if spans := tr.spans.GetSpans(); len(spans) != 0 {
		t.Errorf("recorded %d spans for an untraced path, want none", len(spans))
	}
	if got := trace.SpanContextFromContext(ctx); got.TraceID().String() != callerTrace || !got.IsRemote() {
		t.Errorf("context = %v, want the caller's trace %s", got, callerTrace)
	}
}

// TestPropagate checks that a call made with a request's context carries
// its trace, tracestate and request id to the next service, as a span of
// its own within the request's.
func TestPropagate(t *testing.T) {
	t.Parallel()
	received := make(chan http.Header, 1)
	downstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
	}))
	t.Cleanup(downstream.Close)
	client := &http.Client{Transport: Propagate(http.DefaultTransport)}

	// call makes one request with ctx and returns the headers that arrived.
	call := func(ctx context.Context) http.Header {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, downstream.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
		// A RoundTripper must not change the caller's request.
		if len(req.Header) != 0 {
			t.Errorf("Propagate set headers on the caller's request: %v", req.Header)
		}
		return <-received
	}

	const state = "vendor=opaque-value"
	tr := newTracing(t)
	in := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	in.Header.Set(TraceParentHeader, callerParent)
	in.Header.Set(TraceStateHeader, state)
	ctx := capture(t, func(h http.Handler) http.Handler { return RequestID(tr.middleware(h)) }, in)
	got := call(ctx)

	// The call has a span of its own, the request's child, and it is the
	// downstream's parent.
	request := trace.SpanContextFromContext(ctx)
	var callSpan trace.SpanID
	for _, span := range tr.spans.GetSpans() {
		if span.Parent.SpanID() == request.SpanID() {
			callSpan = span.SpanContext.SpanID()
		}
	}
	if !callSpan.IsValid() {
		t.Fatalf("no span for the call among %v", tr.spans.GetSpans())
	}
	if want := "00-" + callerTrace + "-" + callSpan.String() + "-01"; got.Get(TraceParentHeader) != want {
		t.Errorf("traceparent = %q, want %q", got.Get(TraceParentHeader), want)
	}
	if got.Get(TraceStateHeader) != state {
		t.Errorf("tracestate = %q, want the caller's %q passed on", got.Get(TraceStateHeader), state)
	}
	if got.Get(RequestIDHeader) != RequestIDFrom(ctx) {
		t.Errorf("request id = %q, want %q", got.Get(RequestIDHeader), RequestIDFrom(ctx))
	}

	// Outside a request there is nothing to pass on.
	got = call(t.Context())
	if got.Get(TraceParentHeader) != "" || got.Get(RequestIDHeader) != "" {
		t.Errorf("headers %v sent without a request context", got)
	}
}
