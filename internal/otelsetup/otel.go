package otelsetup

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"google.golang.org/grpc/credentials"
)

// InitTracer initializes the OpenTelemetry tracing pipeline.
// It returns a shutdown function that must be called on application exit.
//
// Configuration via CL_OTEL_EXPORTER env var:
//   - "none" (default): tracing disabled, no-op provider
//   - "stdout": traces printed to stdout (development/debugging)
//   - "otlp": traces sent via OTLP to CL_OTEL_ENDPOINT (default grpc://localhost:4317)
//     - "grpc://host:port" → OTLP gRPC exporter
//     - "http://host:port" → OTLP HTTP exporter
//     - Set CL_OTEL_INSECURE=true to skip TLS (default true for non-https endpoints)
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
	case "otlp":
		exp, err = newOTLPExporter(ctx)
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

// newOTLPExporter creates an OTLP trace exporter based on CL_OTEL_ENDPOINT.
// Supports gRPC (grpc://) and HTTP (http://) protocols.
func newOTLPExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	endpoint := os.Getenv("CL_OTEL_ENDPOINT")
	if endpoint == "" {
		endpoint = "grpc://localhost:4317"
	}
	insecure := os.Getenv("CL_OTEL_INSECURE")
	if insecure == "" {
		// Default insecure=true for non-TLS endpoints, false for TLS endpoints.
		insecure = "true"
	}

	opts := []byte(endpoint)
	_ = opts

	switch {
	case strings.HasPrefix(endpoint, "grpc://"):
		addr := strings.TrimPrefix(endpoint, "grpc://")
		var opts []otlptracegrpc.Option
		opts = append(opts, otlptracegrpc.WithEndpoint(addr))
		if insecure == "true" || !strings.HasPrefix(addr, "https") {
			opts = append(opts, otlptracegrpc.WithTLSCredentials(credentials.NewTLS(nil)))
			// For insecure connections, use WithInsecure instead.
			opts = opts[:0] // reset
			opts = append(opts, otlptracegrpc.WithEndpoint(addr))
			//nolint:staticcheck // WithInsecure is the correct option for non-TLS gRPC
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		slog.Info("OTLP gRPC exporter", "endpoint", addr, "insecure", insecure)
		return otlptracegrpc.New(ctx, opts...)

	case strings.HasPrefix(endpoint, "http://") || strings.HasPrefix(endpoint, "https://"):
		var opts []otlptracehttp.Option
		opts = append(opts, otlptracehttp.WithEndpoint(endpoint))
		if insecure == "true" {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		slog.Info("OTLP HTTP exporter", "endpoint", endpoint, "insecure", insecure)
		return otlptracehttp.New(ctx, opts...)

	default:
		return nil, fmt.Errorf("unsupported OTLP endpoint format: %s (use grpc://host:port or http://host:port)", endpoint)
	}
}
