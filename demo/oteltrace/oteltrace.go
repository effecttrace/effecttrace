// Package oteltrace configures OpenTelemetry tracing for the demo binaries.
package oteltrace

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Setup returns a tracer provider exporting OTLP/HTTP. The endpoint is taken
// from the standard OTEL_EXPORTER_OTLP_ENDPOINT / _TRACES_ENDPOINT variables.
func Setup(ctx context.Context, service string) (*sdktrace.TracerProvider, propagation.TextMapPropagator, error) {
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, nil, err
	}
	res := resource.NewSchemaless(attribute.String("service.name", service))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(500*time.Millisecond)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	return tp, propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}), nil
}
