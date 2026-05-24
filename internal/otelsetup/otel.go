package otelsetup

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// InitTracer initializes the OpenTelemetry tracing pipeline.
// It returns a shutdown function that must be called on application exit.
//
// Configuration via CL_OTEL_EXPORTER env var:
//   - "none" (default): tracing disabled, no-op provider
//   - "stdout": traces printed to stdout (development/debugging)
func InitTracer(ctx context.Context, serviceName, version string) (func(context.Context) error, error) {
	exporterType := os.Getenv("CL_OTEL_EXPORTER")
	if exporterType == "" {
		exporterType = "none"
	}

	if exporterType == "none" {
		return func(ctx context.Context) error { return nil }, nil
	}

	var exp sdktrace.SpanExporter
	var err error
	switch exporterType {
	case "stdout":
		exp, err = stdouttrace.New(stdouttrace.WithWriter(os.Stdout))
	default:
		return nil, fmt.Errorf("unsupported OTEL exporter: %s", exporterType)
	}
	if err != nil {
		return nil, fmt.Errorf("create exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
			semconv.ServiceVersionKey.String(version),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("create resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)

	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	slog.Info("OpenTelemetry tracing enabled", "exporter", exporterType)
	return provider.Shutdown, nil
}
