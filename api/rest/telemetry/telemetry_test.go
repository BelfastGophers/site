// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

package telemetry

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// exporter keeps the spans it is sent, through shutdown.
type exporter struct {
	mu    sync.Mutex
	names []string
}

func (e *exporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range spans {
		e.names = append(e.names, s.Name())
	}
	return nil
}

func (*exporter) Shutdown(context.Context) error { return nil }

func (e *exporter) exported() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.names
}

// TestSetup checks that Setup installs providers that send to the exporters
// it is given, W3C propagation, and runtime metrics, and that shutdown
// flushes what is waiting. Not parallel: it sets the process's providers.
func TestSetup(t *testing.T) {
	spans := &exporter{}
	metrics := sdkmetric.NewManualReader()
	shutdown, err := Setup(t.Context(), WithSpanExporter(spans), WithMetricReader(metrics))
	if err != nil {
		t.Fatal(err)
	}

	ctx, span := otel.Tracer("test").Start(t.Context(), "work")
	header := http.Header{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(header))
	span.End()
	if want := "00-" + span.SpanContext().TraceID().String() + "-" + span.SpanContext().SpanID().String() + "-01"; header.Get("traceparent") != want {
		t.Errorf("traceparent = %q, want %q", header.Get("traceparent"), want)
	}

	// otelhttp records through the process's meter provider, as this does.
	counter, err := otel.Meter("test").Int64Counter("test.count")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(t.Context(), 1)
	var rm metricdata.ResourceMetrics
	if err := metrics.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go.goroutine.count", "test.count"} {
		if !hasMetric(rm, name) {
			t.Errorf("no %s in %+v", name, rm)
		}
	}

	// Spans are sent in batches, so the one ended above is still waiting.
	if err := shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := spans.exported(); len(got) != 1 || got[0] != "work" {
		t.Errorf("exported %v, want the work span", got)
	}
}

// TestSetupNoExporter checks that with no exporter the process still makes
// real spans, so requests have trace ids for their logs.
func TestSetupNoExporter(t *testing.T) {
	shutdown, err := Setup(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(t.Context()) })
	_, span := otel.Tracer("test").Start(t.Context(), "work")
	defer span.End()
	if !span.SpanContext().IsValid() {
		t.Error("span has no trace id with no exporter")
	}
}

func hasMetric(rm metricdata.ResourceMetrics, name string) bool {
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}
