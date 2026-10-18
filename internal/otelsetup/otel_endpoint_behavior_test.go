package otelsetup

// U-40 behavioral verification: with otlptracehttp v1.47.0, WithEndpoint
// stores the string verbatim as the URL Host (a scheme-bearing value dials a
// garbage host), while WithEndpointURL parses scheme/host/path. Prove it by
// driving a real exporter against a local listener through the production
// code path.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestOTLPHTTPExporter_SchemeEndpointReachesCollector(t *testing.T) {
	var gotPath, gotHost bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = true
		if r.Host != "" {
			gotHost = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// Drive the production env-based constructor with an http:// URL.
	t.Setenv("CL_OTEL_ENDPOINT", "http://"+srv.Listener.Addr().String())
	t.Setenv("CL_OTEL_INSECURE", "true")

	exp, err := newOTLPExporter(context.Background())
	if err != nil {
		t.Fatalf("newOTLPExporter: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Export a REAL span and flush through a batcher: an empty exporter
	// performs no network round trip, so Shutdown alone proves nothing.
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
	tr := tp.Tracer("u40-test")
	_, span := tr.Start(ctx, "probe")
	span.End()
	// Shutdown flushes the batcher → the collector must see the export; with
	// the old WithEndpoint("http://…") the dial failed and it never did.
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatalf("provider shutdown: %v", err)
	}
	if !gotPath || !gotHost {
		t.Fatal("collector never received the export — scheme-bearing WithEndpoint dialed a garbage host")
	}
}
