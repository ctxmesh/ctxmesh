/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const tracerName = "agent-engine/launcher"

// setupOTel initialises a TracerProvider that exports via OTLP/gRPC to
// endpoint and installs a W3C TraceContext propagator globally.
//
// Tracing is BEST-EFFORT and NON-BLOCKING: the gRPC client is created lazily
// (no blocking dial), and a batch span processor drops spans silently when the
// collector is unreachable. If any step fails, the error is returned and the
// caller falls back to the global no-op provider.
//
// The returned shutdown function must be called on clean exit to flush
// in-flight spans.
func setupOTel(ctx context.Context, endpoint string) (func(context.Context) error, error) {
	// NewClient is non-blocking (lazy connect) — safe even if the collector
	// is down at startup.
	conn, err := grpc.NewClient(
		endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("otel: grpc client to %q: %w", endpoint, err)
	}

	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("otel: otlp exporter: %w", err)
	}

	// WithBatcher uses a batch processor: span export is async and
	// non-blocking on the request path.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
	)

	// Install globally so otel.Tracer() and otel.GetTextMapPropagator() work.
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return func(ctx context.Context) error {
		return tp.Shutdown(ctx)
	}, nil
}

// buildHandler returns an HTTP handler that reverse-proxies all requests to
// upstreamURL. Requests to paths for which shouldSpan returns true are wrapped
// in an agent.invoke server span with W3C context propagation; all other
// requests pass through without any tracing overhead.
//
// tracer and prop are explicit parameters (rather than read from the global
// otel package) so the function can be exercised in unit tests without global
// state mutations.
func buildHandler(
	tracer trace.Tracer,
	prop propagation.TextMapPropagator,
	upstreamURL *url.URL,
	cfg Config,
) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !shouldSpan(r.URL.Path) {
			// Health probes: pass through with no span.
			proxy.ServeHTTP(w, r)
			return
		}

		// Extract the caller's W3C traceparent/tracestate (continues an
		// existing trace for A2A calls; starts a new root if absent).
		ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

		ctx, span := tracer.Start(ctx, "agent.invoke",
			trace.WithSpanKind(trace.SpanKindServer),
		)

		start := time.Now()

		// Capture the HTTP status code written by the upstream.
		rw := &statusWriter{ResponseWriter: w, code: http.StatusOK}

		// Clone the request with the new context so the ReverseProxy sees it,
		// then inject the span context into the outbound headers so upstream
		// instrumentation can nest beneath this span.
		outReq := r.Clone(ctx)
		prop.Inject(ctx, propagation.HeaderCarrier(outReq.Header))

		proxy.ServeHTTP(rw, outReq)

		latencyMS := time.Since(start).Milliseconds()

		span.SetAttributes(
			attribute.String("agent.name", cfg.AgentName),
			attribute.String("agent.version", cfg.AgentVersion),
			attribute.String("agent.route", cfg.AgentRoute),
			attribute.Int("http.status_code", rw.code),
			attribute.Int64("latency_ms", latencyMS),
		)
		if rw.code >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(rw.code))
		}
		span.End()
	})
}

// statusWriter wraps http.ResponseWriter to capture the HTTP status code
// written by the upstream so it can be added as a span attribute.
type statusWriter struct {
	http.ResponseWriter
	code    int
	written bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if !sw.written {
		sw.code = code
		sw.written = true
	}
	sw.ResponseWriter.WriteHeader(code)
}
