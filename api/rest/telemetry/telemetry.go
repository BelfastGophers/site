// Managed by httpgen. Changes are overwritten by 'go tool httpgen sync'.

// Package telemetry installs OpenTelemetry for the process. The service
// produces its telemetry: a span and HTTP metrics for every request, from
// middleware.Trace, and Go runtime metrics from here. Where the telemetry
// goes is the service's choice, made in main.go: Setup sends it only to the
// exporters it is given, and with none it never leaves the process.
//
// The OpenTelemetry SDK is installed even with no exporter, so that every
// request has a trace id for its logs to carry.
package telemetry

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Option chooses where Setup sends telemetry.
type Option func(*options)

type options struct {
	trace  []sdktrace.TracerProviderOption
	metric []sdkmetric.Option
}

// WithSpanExporter sends spans to e, in batches, such as an OTLP exporter
// to a collector.
func WithSpanExporter(e sdktrace.SpanExporter) Option {
	return func(o *options) { o.trace = append(o.trace, sdktrace.WithBatcher(e)) }
}

// WithMetricReader reads metrics through r: a periodic reader around an
// exporter pushes them, and a Prometheus reader serves them to be scraped.
func WithMetricReader(r sdkmetric.Reader) Option {
	return func(o *options) { o.metric = append(o.metric, sdkmetric.WithReader(r)) }
}

// Setup installs the process's tracer and meter providers, W3C trace context
// and baggage propagation, and Go runtime metrics, and returns a function
// that flushes and stops them. Call it once, in run, and call shutdown after
// the server has stopped, so the last requests' telemetry is sent.
//
// The service is named by OTEL_SERVICE_NAME, and the rest of the standard
// OTEL_ environment variables apply too: OTEL_RESOURCE_ATTRIBUTES, and
// OTEL_TRACES_SAMPLER to sample fewer requests.
func Setup(ctx context.Context, opts ...Option) (shutdown func(context.Context) error, err error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	traces := sdktrace.NewTracerProvider(o.trace...)
	metrics := sdkmetric.NewMeterProvider(o.metric...)
	shutdown = func(ctx context.Context) error {
		return errors.Join(traces.Shutdown(ctx), metrics.Shutdown(ctx))
	}
	if err := runtime.Start(runtime.WithMeterProvider(metrics)); err != nil {
		return nil, errors.Join(fmt.Errorf("runtime metrics: %w", err), shutdown(ctx))
	}

	otel.SetTracerProvider(traces)
	otel.SetMeterProvider(metrics)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return shutdown, nil
}
