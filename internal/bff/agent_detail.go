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

package bff

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

// knativeServiceLabel is the label Knative stamps on every pod serving an agent.
// The agent's Knative Service name equals the AgentDeployment name (the controller
// creates the ksvc with metadata.name = deploy.Name), so this selector resolves an
// agent's pods without reading the Knative revision graph.
const knativeServiceLabel = "serving.knative.dev/service"

// maxLogTailLines bounds the ?tailLines value so a single request can never ask
// the API server for an unbounded backlog. It is the cap; a smaller ?tailLines is
// honored, a larger one is clamped down.
const maxLogTailLines = 5000

// defaultLogTailLines is the bounded backlog returned when ?tailLines is absent —
// enough to see why an agent is (not) coming up without buffering a huge history.
const defaultLogTailLines = 200

// handleAgentDetail serves GET /api/agents/{ns}/{name} — the agent-landing detail
// DTO (first-agent-flow.md §3). It is CALLER-SCOPED (ADR 0011): the AgentDeployment
// and every binding/version list is read through the caller's own client, so the
// K8s API server enforces the caller's RBAC. A viewer who can read the agent gets
// a 200; a not-found is a 404; a Forbidden (on the agent OR a namespace the caller
// can't read) surfaces as 403 — never a swallowed empty body.
//
// Bindings and versions are read namespace-scoped and filtered to those referencing
// this agent. A Forbidden on the AGENT read is fatal (403); a Forbidden on the
// binding/version lists is DEGRADED, not fatal — a caller allowed to read the agent
// but not the bindings still gets the agent detail with an empty bindings list,
// rather than a 403 that hides the agent they can see.
func (s *Server) handleAgentDetail(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	ns := strings.TrimSpace(r.PathValue("ns"))
	name := strings.TrimSpace(r.PathValue("name"))
	if ns == "" || name == "" {
		writeError(w, http.StatusBadRequest, "namespace and name are required")
		return
	}

	var ad agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ad); err != nil {
		s.writeGetError(w, err, "agent")
		return
	}

	// Bindings + versions are namespace-scoped lists filtered to this agent. A
	// Forbidden here is degraded (empty list), not fatal — the caller can see the
	// agent; hiding it behind a binding-RBAC 403 would be worse than an honest
	// partial view. Any other list error IS surfaced (500) so a real fault is not
	// masked as "no bindings".
	inNS := []client.ListOption{client.InNamespace(ns)}

	toolBindings := []agentsv1alpha1.MCPToolBinding{}
	if list, err := listMCPToolBindings(r.Context(), caller, inNS...); err != nil {
		if !apierrors.IsForbidden(err) {
			s.log.Error(err, "list MCPToolBindings for detail failed", "namespace", ns)
			writeError(w, http.StatusInternalServerError, "failed to read agent bindings")
			return
		}
	} else {
		toolBindings = list.Items
	}

	versions := []agentsv1alpha1.AgentVersion{}
	if list, err := listAgentVersions(r.Context(), caller, inNS...); err != nil {
		if !apierrors.IsForbidden(err) {
			s.log.Error(err, "list AgentVersions for detail failed", "namespace", ns)
			writeError(w, http.StatusInternalServerError, "failed to read agent versions")
			return
		}
	} else {
		versions = list.Items
	}

	// Edit-mode flags (ADR 0017). managedOutsideUI is the mechanical "no source-spec
	// annotation" fact; drift is only meaningful for a console-managed agent and is
	// computed by re-expanding the stored source-spec and comparing the console-
	// managed spec fields against the live object. A drift computation error (a
	// corrupt/unexpandable stored spec — should not happen for a spec we canonicalized
	// at write) is treated as "no drift" rather than failing the whole read: the
	// detail page is a read and must not 500 on a stale annotation.
	managedOutsideUI, drift := s.editModeFlags(&ad)

	detail := newAgentDetail(&ad, toolBindings, versions, managedOutsideUI, drift)

	// U13: read the agent's DURABLE published-template state so the "Published" badge + Unpublish
	// survive a reload (they were in-session only). A store read on the BFF's own cpDB (the same
	// GetLatest the fork path uses); best-effort — a nil store or an error degrades to no badge
	// (never a 500), since publish state is decoration, not the detail's contract.
	if s.publishedArtifactStore != nil {
		if pa, ok, err := s.publishedArtifactStore.GetLatest(r.Context(), kindAgent, ns, name); err != nil {
			s.log.Error(err, "agent detail: published-state lookup failed; badge omitted", "namespace", ns, "name", name)
		} else if ok {
			detail.Published = &PublishedRef{Visibility: pa.Visibility, Version: pa.Version}
		}
	}

	writeJSON(w, http.StatusOK, detail)
}

// defaultAgentRunLimit is the bounded per-agent run count when ?limit is absent.
const defaultAgentRunLimit = 20

// maxAgentRunLimit caps ?limit so one request can never ask Langfuse for an
// unbounded page. A larger ?limit is clamped down; a smaller one is honored.
const maxAgentRunLimit = 100

// handleAgentRuns serves GET /api/agents/{ns}/{name}/runs — the bounded recent-runs
// list for ONE agent (the agent detail page's per-agent run history, m15.9). It is
// CALLER-SCOPED (ADR 0011) for the EXISTENCE check: the caller must be able to `get`
// the AgentDeployment through their OWN client (K8s RBAC), exactly like the detail
// route. A viewer who can read the agent gets its runs; a not-found is 404; a
// Forbidden on the agent is 403 — never a swallowed empty body.
//
// The runs themselves come from Langfuse through the server-side adapter (its own
// keys, like the m14.8 run inspector — NOT the caller's token; Langfuse has no K8s
// RBAC), filtered to the `agent:<ns>/<name>` trace identity tag so a same-named
// agent in another namespace can never leak in (the cross-namespace correctness
// property). Redaction-honest: this is trace METADATA only (traceId/name/timestamp/
// cost/tokens/latency), never payload content.
//
// The route is registered only when BOTH the caller-client factory AND the Langfuse
// adapter are wired; when Langfuse is absent the route seam serves an honest 501
// (the m14.8 degrade pattern), never a 500. ?limit bounds the page (default 20,
// capped at 100).
func (s *Server) handleAgentRuns(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	ns := strings.TrimSpace(r.PathValue("ns"))
	name := strings.TrimSpace(r.PathValue("name"))
	if ns == "" || name == "" {
		writeError(w, http.StatusBadRequest, "namespace and name are required")
		return
	}

	// Caller-scoped existence + authorization gate: verify the caller can `get` the
	// agent BEFORE fetching any runs. A denial/absence surfaces honestly (403/404)
	// and no run metadata is returned for an agent the caller may not see.
	var ad agentsv1alpha1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ad); err != nil {
		s.writeGetError(w, err, "agent")
		return
	}

	limit := parseAgentRunLimit(r.URL.Query().Get("limit"))

	runs, err := s.adapters.Langfuse.RunsForAgent(r.Context(), ns, name, limit)
	if err != nil {
		s.log.Error(err, "fetch agent runs failed", "namespace", ns, "agent", name)
		writeError(w, http.StatusBadGateway, "failed to fetch agent runs")
		return
	}
	if runs == nil {
		runs = []RunSummary{}
	}
	writeJSON(w, http.StatusOK, AgentRunsResponse{Namespace: ns, Name: name, Runs: runs})
}

// parseAgentRunLimit resolves ?limit to a bounded page size: absent/invalid/non-
// positive → defaultAgentRunLimit; a value above maxAgentRunLimit is clamped down.
func parseAgentRunLimit(raw string) int {
	n := defaultAgentRunLimit
	if raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			n = v
		}
	}
	if n > maxAgentRunLimit {
		n = maxAgentRunLimit
	}
	return n
}

// handleAgentLogs serves GET /api/agents/{ns}/{name}/logs — the live SSE pod-log
// tail (first-agent-flow.md §3). It is CALLER-SCOPED via the `pods/log`
// subresource (ADR 0011): the log stream is opened with the CALLER'S token, so the
// caller's own RBAC on pods/pods/log governs it. The BFF SA has rules:[] and cannot
// read pod logs — only the caller can, which is correct.
//
// Query params:
//   - follow=<bool>    — true streams until the client disconnects; false (default)
//     returns a bounded tail then closes.
//   - container=<name> — pick a specific container (default: the pod default).
//   - tailLines=<n>    — bounded initial backlog (default defaultLogTailLines,
//     capped at maxLogTailLines).
//
// The response is Server-Sent Events. Failures surface, never hang:
//   - anon (no token)                → 401 before any K8s call.
//   - caller denied pods list/log    → 403 BEFORE the SSE stream opens.
//   - agent has no pod yet (starting) → a "waiting" SSE event, HTTP 200, no 500.
//   - the pod's log stream ends       → a terminal SSE event, the connection closes.
func (s *Server) handleAgentLogs(w http.ResponseWriter, r *http.Request) {
	// The pod-log stream needs a CALLER-SCOPED typed core client (the CRD client
	// cannot stream pods/log). A missing token is a 401 before any K8s call.
	accessor, err := s.callerClients.PodLogsForRequest(r)
	if err != nil {
		if errors.Is(err, errUnauthenticated) {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		s.log.Error(err, "build caller-scoped pod-log client failed")
		writeError(w, http.StatusInternalServerError, "failed to build caller client")
		return
	}

	ns := strings.TrimSpace(r.PathValue("ns"))
	name := strings.TrimSpace(r.PathValue("name"))
	if ns == "" || name == "" {
		writeError(w, http.StatusBadRequest, "namespace and name are required")
		return
	}

	follow := r.URL.Query().Get("follow") == "true"
	container := strings.TrimSpace(r.URL.Query().Get("container"))
	tail := parseTailLines(r.URL.Query().Get("tailLines"))

	// Resolve the agent's pods CALLER-SCOPED. A Forbidden here means the caller
	// cannot list pods in this namespace — surface a 403 BEFORE we switch the
	// response to a streaming SSE body (once the SSE headers are written we can no
	// longer set a status). This is the "don't hang on 403" contract: a denial is
	// an HTTP error, never an open connection.
	selector := knativeServiceLabel + "=" + name
	pods, err := accessor.ListPods(r.Context(), ns, selector)
	if err != nil {
		switch {
		case apierrors.IsForbidden(err):
			writeError(w, http.StatusForbidden, "forbidden: not allowed to read pods in this namespace")
		case apierrors.IsUnauthorized(err):
			writeError(w, http.StatusUnauthorized, "unauthorized: token rejected by the API server")
		default:
			s.log.Error(err, "list agent pods for logs failed", "namespace", ns, "agent", name)
			writeError(w, http.StatusInternalServerError, "failed to resolve agent pods")
		}
		return
	}

	pod := selectActivePod(pods.Items)

	// We are committed to an SSE response from here on. Set the streaming headers
	// and get a flusher so each event reaches the browser immediately.
	flusher, ok := s.beginSSE(w)
	if !ok {
		return
	}

	if pod == "" {
		// The agent is still starting — no pod is running yet. Emit a waiting event
		// and close cleanly (NOT a 500): the UI shows "waiting for the agent to
		// start" and retries. This is the pod-not-running graceful path.
		writeSSEEvent(w, flusher, "waiting", "agent is starting; no running pod yet")
		return
	}

	// Open the caller-scoped log stream. A Forbidden on pods/log (the caller can
	// list pods but not read their logs) can only be surfaced as an SSE error event
	// now — the SSE headers (200) are already written. It is still SURFACED (an
	// "error" event the UI shows), never a silent hang.
	stream, err := accessor.StreamPodLog(r.Context(), ns, pod, container, follow, tail)
	if err != nil {
		if apierrors.IsForbidden(err) {
			writeSSEEvent(w, flusher, "error", "forbidden: not allowed to read pod logs")
			return
		}
		s.log.Error(err, "open pod-log stream failed", "namespace", ns, "pod", pod)
		writeSSEEvent(w, flusher, "error", "failed to open log stream")
		return
	}
	defer func() { _ = stream.Close() }()

	streamPodLogSSE(r.Context(), w, flusher, stream)
}

// beginSSE writes the Server-Sent-Events headers and returns the flusher. It
// returns ok=false (with a 500 already written) when the ResponseWriter cannot
// flush — SSE is impossible without per-event flushing, so we fail before
// committing to a stream the client would never receive incrementally.
func (s *Server) beginSSE(w http.ResponseWriter) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return nil, false
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Defeat proxy buffering so events are delivered as they are flushed.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return flusher, true
}

// streamPodLogSSE relays a pod-log reader to the client as SSE "log" events, one
// per line, flushing each so the tail is live. It reads with a BOUNDED scanner
// buffer (a single log line can never force an unbounded allocation). The loop
// ends when the stream closes (pod died/finished/log rotated) or the request
// context is cancelled (client disconnected) — either way it emits a terminal
// "end" event and returns, so the connection is closed gracefully, never hung.
func streamPodLogSSE(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, stream io.Reader) {
	scanner := bufio.NewScanner(stream)
	// Bound the per-line buffer: start at 64 KiB, cap at maxLogLineBytes so a
	// pathological single line cannot OOM the BFF (bufio's default cap is 64 KiB;
	// we raise the cap deliberately but keep it bounded).
	scanner.Buffer(make([]byte, 0, 64*1024), maxLogLineBytes)
	for scanner.Scan() {
		// Stop promptly if the client went away — don't keep draining a stream no
		// one is reading.
		if ctx.Err() != nil {
			return
		}
		writeSSEEvent(w, flusher, "log", scanner.Text())
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		// A read error that is NOT a clean EOF/cancel — surface it as an SSE error
		// so the UI knows the tail broke (e.g. the pod was killed mid-stream).
		writeSSEEvent(w, flusher, "error", "log stream interrupted")
		return
	}
	// Clean end of the log (follow=false tail exhausted, or the pod's log closed).
	writeSSEEvent(w, flusher, "end", "log stream ended")
}

// maxLogLineBytes bounds a single scanned log line (SSE event payload). A line
// longer than this is dropped by the scanner with an error, which the loop
// surfaces as an SSE error rather than buffering without limit.
const maxLogLineBytes = 256 * 1024

// writeSSEEvent writes one Server-Sent Event (an `event:` line + a `data:` line +
// the blank separator) and flushes it. Newlines in the payload are split into
// multiple data: lines per the SSE grammar so a multi-line message stays one
// event. A best-effort write: an error means the client disconnected, which the
// surrounding loop's context check already handles.
func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event, data string) {
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteByte('\n')
	for line := range strings.SplitSeq(data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	_, _ = io.WriteString(w, b.String())
	flusher.Flush()
}

// selectActivePod picks the pod whose logs to tail from an agent's pods. It
// prefers a Running pod (the live revision serving traffic); if none is Running it
// falls back to the newest pod by creation time (e.g. a Pending pod that is still
// pulling the image, so the user can watch it come up). It returns "" when there
// are no pods at all — the agent is still starting / scaled to zero. Ties are
// broken by name for a deterministic pick.
func selectActivePod(pods []corev1.Pod) string {
	best := -1
	for i := range pods {
		if best == -1 || betterPod(&pods[i], &pods[best]) {
			best = i
		}
	}
	if best == -1 {
		return ""
	}
	return pods[best].Name
}

// betterPod reports whether a is a better log target than b: a Running pod beats a
// non-Running one; between two of the same running-ness, the newer creation time
// wins, then the lexically-greater name (deterministic tie-break).
func betterPod(a, b *corev1.Pod) bool {
	aRun := a.Status.Phase == corev1.PodRunning
	bRun := b.Status.Phase == corev1.PodRunning
	if aRun != bRun {
		return aRun
	}
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.After(b.CreationTimestamp.Time)
	}
	return a.Name > b.Name
}

// parseTailLines resolves ?tailLines to a bounded backlog: absent/invalid/non-
// positive → defaultLogTailLines; a value above maxLogTailLines is clamped down.
// It returns a *int64 because corev1.PodLogOptions.TailLines is a pointer (nil =
// the API default); we always pass a bounded value so no request asks for an
// unbounded backlog.
func parseTailLines(raw string) *int64 {
	n := int64(defaultLogTailLines)
	if raw != "" {
		if v, err := strconv.ParseInt(raw, 10, 64); err == nil && v > 0 {
			n = v
		}
	}
	if n > maxLogTailLines {
		n = maxLogTailLines
	}
	return &n
}
