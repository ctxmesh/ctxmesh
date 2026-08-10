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

// langfuseTraceTagsAttr is the OTel span-attribute key Langfuse's OTLP ingestion
// promotes to TRACE-LEVEL tags: a string-array attribute whose entries become the
// trace's `tags` (filterable via GET /api/public/traces?tags=...). Unlike the
// nested `agent.name` span attribute — which Langfuse buries as an observation
// attribute, NOT a trace-level field — a tag is a first-class trace filter. We use
// it to stamp the UNAMBIGUOUS per-agent identity so the BFF can list the runs of
// exactly one agent, never mixing two same-named agents in different namespaces.
const langfuseTraceTagsAttr = "langfuse.trace.tags"

// langfuseTraceNameAttr names the TRACE (not just the span). The BFF mints a
// traceparent with a seed span id (invoke.go), so the launcher's agent.invoke span
// is a CHILD of that seed — and the seed span is never exported, so the trace's root
// has no name and Langfuse shows an unnamed run (invisible in the runs list). Langfuse
// honors `langfuse.trace.name` from ANY span, so stamping it on agent.invoke gives the
// trace a stable, human-readable name regardless of the phantom root.
const langfuseTraceNameAttr = "langfuse.trace.name"

// agentInvokeSpanName is the launcher's root proxy span name and the trace-name
// fallback for an unnamed agent.
const agentInvokeSpanName = "agent.invoke"

// agentTagPrefix prefixes the per-agent identity tag value. The full tag is
// `agent:<namespace>/<name>` — the SAME `<ns>/<name>` key the BFF topology keys
// agents on — so a filter for one agent's runs cannot match a same-named agent in
// another namespace. Exported-shape kept in sync with the BFF's agentRunTag().
const agentTagPrefix = "agent:"

// versionTagPrefix prefixes the per-version trace tag value. The full tag is
// `version:<agentVersion>` — an ADDITIVE second trace tag alongside the agent tag
// (m69.5, ADR 0062 Fork 2) so the online-scoring worker can filter/group a trace by
// the exact agent version that served it. `agent.version` is stamped as a span
// ATTRIBUTE (below), which Langfuse buries as an observation field — NOT a
// trace-level tag — so it cannot back a trace filter; a tag can. Stamped only when
// the version is known (empty AgentVersion emits no version tag — nothing to group on).
const versionTagPrefix = "version:"

// agentIdentityTag builds the trace-level identity tag `agent:<ns>/<name>` from the
// launcher config. When the namespace is absent (a misconfiguration — the
// controller injects POD_NAMESPACE) it degrades to the bare `agent:<name>` rather
// than emitting a `agent:/name` with an empty namespace segment; an empty name in
// turn yields no meaningful tag, so the caller skips stamping it (an unnamed agent
// simply is not per-agent-filterable, which is honest, not a crash).
func agentIdentityTag(cfg Config) string {
	if cfg.AgentName == "" {
		return ""
	}
	if cfg.AgentNamespace == "" {
		return agentTagPrefix + cfg.AgentName
	}
	return agentTagPrefix + cfg.AgentNamespace + "/" + cfg.AgentName
}

// agentTraceName is the human-readable TRACE name stamped via langfuse.trace.name:
// the `<ns>/<name>` agent identity (so the runs list identifies WHICH agent ran),
// degrading to the bare name, then to "agent.invoke" for an unnamed agent — never
// empty, so a run is always named and visible in the runs list.
func agentTraceName(cfg Config) string {
	switch {
	case cfg.AgentName == "":
		return agentInvokeSpanName
	case cfg.AgentNamespace == "":
		return cfg.AgentName
	default:
		return cfg.AgentNamespace + "/" + cfg.AgentName
	}
}

// setAgentIdentityTag stamps the trace-level `langfuse.trace.name` + `langfuse.trace.tags`
// attributes: the NAME so the run is visible/identifiable in the runs list (the trace root
// is the BFF's never-exported seed span, so without this the trace is unnamed), and the
// per-agent identity TAG so the BFF can filter one agent's runs. Called on EVERY
// agent.invoke span — including the guard-denied path — so a denied run is still named +
// attributed to its agent.
func setAgentIdentityTag(span trace.Span, cfg Config) {
	span.SetAttributes(attribute.String(langfuseTraceNameAttr, agentTraceName(cfg)))
	// Build the trace-level tag slice: the per-agent identity tag `agent:<ns>/<name>`,
	// plus an ADDITIVE `version:<agentVersion>` tag when the version is known (m69.5,
	// ADR 0062 Fork 2) so the online-scoring worker can group a trace by the exact
	// version that served it. Order is agent-first, then version. An unnamed agent
	// yields no agent tag; an empty version yields no version tag — so the slice is
	// only stamped when it has at least one entry (never an empty tags array).
	tags := make([]string, 0, 2)
	if tag := agentIdentityTag(cfg); tag != "" {
		tags = append(tags, tag)
	}
	if cfg.AgentVersion != "" {
		tags = append(tags, versionTagPrefix+cfg.AgentVersion)
	}
	if len(tags) > 0 {
		span.SetAttributes(attribute.StringSlice(langfuseTraceTagsAttr, tags))
	}
	// Tenant attribution (M47, ADR 0046): stamp the owning tenant on the run so cost + usage
	// are attributable per tenant (PRD §13). Empty for an untenanted agent — omitted, not noise.
	if cfg.TenantID != "" {
		span.SetAttributes(attribute.String("tenant.id", cfg.TenantID))
	}
}

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

// inboundGuard is the callee-side access-control hook the proxy runs on the
// inbound path, INSIDE the agent.invoke span, before forwarding to the user
// container. It returns true when the request may proceed; on a denial it has
// already written the typed error response and returns false. nil ⇒ no guard
// (the agent is not a registry member), and every request proceeds.
//
// It is an interface (not a concrete *a2aGuard) so buildHandler stays decoupled
// from the A2A surface and unit tests can inject a stub.
type inboundGuard interface {
	enforceInbound(ctx context.Context, w http.ResponseWriter, r *http.Request) bool
}

// asyncHandler is the async A2A consumer hook the proxy runs when an inbound
// request is a CloudEvent (an eventing agent's Trigger delivery). It decodes the
// envelope, dedupes on messageId, and invokes the agent — acking or NACKing the
// event. nil ⇒ the agent is not an eventing consumer, and a CloudEvent-shaped
// request (there won't be one) would fall through to the ordinary proxy path.
//
// It is an interface (not a concrete *asyncConsumer) so buildHandler stays
// decoupled from the async surface and unit tests can inject a stub.
type asyncHandler interface {
	consume(w http.ResponseWriter, r *http.Request)
}

// buildHandler returns an HTTP handler that reverse-proxies all requests to
// upstreamURL. Requests to paths for which shouldSpan returns true are wrapped
// in an agent.invoke server span with W3C context propagation; all other
// requests pass through without any tracing overhead.
//
// guard, when non-nil, enforces A2A access control on the inbound path inside
// the span (a denied caller is rejected before the request reaches the user
// container). nil disables it.
//
// consumer, when non-nil, handles a CloudEvent-shaped inbound POST (a Trigger
// delivery to an eventing agent): it is dispatched to the async consumer BEFORE
// the ordinary /invoke span/proxy path, so an async A2A event is deduped and
// invoked through its own a2a.async.consume span. An ordinary /invoke (no
// CloudEvent headers) is unaffected. nil disables the async path.
//
// tracer and prop are explicit parameters (rather than read from the global
// otel package) so the function can be exercised in unit tests without global
// state mutations.
func buildHandler(
	tracer trace.Tracer,
	prop propagation.TextMapPropagator,
	upstreamURL *url.URL,
	cfg Config,
	guard inboundGuard,
	consumer asyncHandler,
) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !shouldSpan(r.URL.Path) {
			// Health probes: pass through with no span.
			proxy.ServeHTTP(w, r)
			return
		}

		// Async A2A: a CloudEvent-shaped POST (Trigger delivery) is handled by the
		// async consumer — decode → dedupe → invoke — on its own span, not the
		// sync /invoke path. Checked before the agent.invoke span so a redelivered
		// duplicate is acked without ever opening an invoke span.
		if consumer != nil && isCloudEventRequest(r) {
			consumer.consume(w, r)
			return
		}

		// Extract the caller's W3C traceparent/tracestate (continues an
		// existing trace for A2A calls; starts a new root if absent).
		ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

		ctx, span := tracer.Start(
			ctx, agentInvokeSpanName,
			trace.WithSpanKind(trace.SpanKindServer),
		)
		// Deferred so the span is always ended (and exported) even if the
		// upstream ServeHTTP panics — otherwise the span would be dropped.
		defer span.End()

		start := time.Now()

		// Capture the HTTP status code written by the upstream.
		rw := &statusWriter{ResponseWriter: w, code: http.StatusOK}

		// A2A inbound access control (callee side): if this /invoke carries an
		// A2A envelope, enforce the callee's allowlist/role rules INSIDE the
		// span before the request reaches the user container. A denial is
		// written here and short-circuits — but the span still records the
		// (denied) status and latency via the deferred End + attribute block.
		if guard != nil && !guard.enforceInbound(ctx, rw, r) {
			span.SetAttributes(
				attribute.String("agent.name", cfg.AgentName),
				attribute.Int("http.status_code", rw.code),
				attribute.Int64("latency_ms", time.Since(start).Milliseconds()),
			)
			// A denied run is still THIS agent's run — stamp the trace-level identity
			// tag so it shows up in the agent's run list (never mis-attributed).
			setAgentIdentityTag(span, cfg)
			return
		}

		// Clone the request with the new context so the ReverseProxy sees it,
		// then inject the span context into the outbound headers so upstream
		// instrumentation can nest beneath this span.
		outReq := r.Clone(ctx)
		prop.Inject(ctx, propagation.HeaderCarrier(outReq.Header))

		// Per-hop messageId (ADR 0035, m33.4): when this /invoke arrived via A2A, surface the
		// envelope's messageId to the user container as X-Message-Id, so the agent's memory writes
		// attribute to THIS hop (the :2998 endpoint stamps it, m33.1) and "who said what to whom"
		// is addressable. A top-level /invoke (no envelope) sets nothing — the memory endpoint mints
		// one. The messageId also already rides the trace (a2a.message.id).
		if mid := a2aMessageIDFromEnvelope(r.Header.Get(a2aEnvelopeHeader)); mid != "" {
			outReq.Header.Set(messageIDHeader, mid)
		}

		proxy.ServeHTTP(rw, outReq)

		latencyMS := time.Since(start).Milliseconds()

		span.SetAttributes(
			attribute.String("agent.name", cfg.AgentName),
			attribute.String("agent.version", cfg.AgentVersion),
			attribute.String("agent.route", cfg.AgentRoute),
			attribute.Int("http.status_code", rw.code),
			attribute.Int64("latency_ms", latencyMS),
		)
		// Trace-level identity tag `agent:<ns>/<name>` (Langfuse promotes
		// langfuse.trace.tags to the trace's tags): the UNAMBIGUOUS per-agent filter
		// key the BFF's per-agent run list uses. Additive — the existing
		// agent.name/version/route attributes above are untouched, so RecentRuns /
		// CostUsage / the run inspector keep working exactly as before.
		setAgentIdentityTag(span, cfg)
		// prompt.version (M9): the resolved git-pointer prompt identifier, so
		// Langfuse can display which prompt this run used. DISPLAY ONLY — git stays
		// the source of truth. Stamped only when the agent has a promptRef (a
		// PROMPT_VERSION was injected); an image-bundled-prompt agent leaves the span
		// unchanged (no empty attribute).
		if cfg.PromptVersion != "" {
			span.SetAttributes(attribute.String("prompt.version", cfg.PromptVersion))
		}
		if rw.code >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(rw.code))
		}
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
