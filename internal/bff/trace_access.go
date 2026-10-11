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
	"context"
	"errors"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// msgTraceNotFound is the one body every refusal on the unrecorded-trace path carries, so an absent,
// untagged and unreadable trace cannot be told apart.
const msgTraceNotFound = "trace not found"

// traceAccess is what authorizeTraceRead proved: the run that maps the trace, or, when no run maps it,
// the trace detail it read to find the trace's originating agent. Exactly one is set.
type traceAccess struct {
	run    *run.Run
	detail *TraceDetail
}

// agent names the AgentDeployment the trace belongs to: the run's agent when a run maps the trace,
// otherwise the trace's agent tag. For a workflow run it is the Workflow's name.
func (a traceAccess) agent() (namespace, name string) {
	if a.run != nil {
		return a.run.Namespace, a.run.Agent
	}
	return a.detail.Rollup.AgentNs, a.detail.Rollup.AgentName
}

// authorizeTraceRead gates every read keyed by a trace id (ADR 0078 and its amendment). A trace a run
// maps is authorized through that run, exactly as authorizeRunAccess does (ownership, or RBAC on the
// backing resource; 403 on denial). A trace no run maps is authorized the way the Runs list authorizes
// its row: see authorizeUnrecordedTrace. It writes the error response and returns false on refusal.
func (s *Server) authorizeTraceRead(w http.ResponseWriter, r *http.Request, caller client.Client, traceID string) (traceAccess, bool) {
	rn, err := s.findRun(traceID)
	if err != nil {
		s.log.Error(err, "authorize trace access: run store lookup failed", "traceID", traceID)
		writeError(w, http.StatusInternalServerError, "failed to read run")
		return traceAccess{}, false
	}
	if rn != nil {
		if !s.authorizeResolvedRun(w, r, caller, rn, true) {
			return traceAccess{}, false
		}
		return traceAccess{run: rn}, true
	}
	detail, ok := s.authorizeUnrecordedTrace(w, r, caller, traceID)
	if !ok {
		return traceAccess{}, false
	}
	return traceAccess{detail: detail}, true
}

// authorizeUnrecordedTrace authorizes a trace that no run maps, such as a direct POST /api/invoke call
// (ADR 0150 §5 keeps those out of the run store while the Runs list still shows them). The rule is the
// list's: the trace's agent tag names an AgentDeployment, and the caller's own token must be able to
// read it. Every refusal is the same 404 — absent from the trace backend, untagged, an end-user bearer
// (authorized only by ownership, and nothing owns an unrecorded trace), an agent the caller may not
// read, a deleted agent, or a token the API server rejects — because an absent trace never reaches the
// API server, so any other answer for a present one would say that it exists.
func (s *Server) authorizeUnrecordedTrace(w http.ResponseWriter, r *http.Request, caller client.Client, traceID string) (*TraceDetail, bool) {
	if s.adapters.Langfuse == nil {
		writeError(w, http.StatusNotFound, msgTraceNotFound)
		return nil, false
	}
	detail, err := s.adapters.Langfuse.TraceDetail(r.Context(), traceID)
	if err != nil {
		if errors.Is(err, ErrTraceNotFound) {
			writeError(w, http.StatusNotFound, msgTraceNotFound)
			return nil, false
		}
		s.log.Error(err, "authorize trace access: trace backend read failed", "traceID", traceID)
		writeError(w, http.StatusBadGateway, "failed to fetch trace")
		return nil, false
	}
	ns, name := detail.Rollup.AgentNs, detail.Rollup.AgentName
	if ns == "" || name == "" {
		writeError(w, http.StatusNotFound, msgTraceNotFound)
		return nil, false
	}
	// An end-user bearer must never reach the API server (ADR 0107), so classify it before the Get.
	if _, _, isEndUser, _ := s.resolveEndUserPrincipal(r.Context(), r, ns); isEndUser {
		writeError(w, http.StatusNotFound, msgTraceNotFound)
		return nil, false
	}
	var ad agentsv1beta1.AgentDeployment
	if err := caller.Get(r.Context(), client.ObjectKey{Namespace: ns, Name: name}, &ad); err != nil {
		if apierrors.IsForbidden(err) || apierrors.IsNotFound(err) || apierrors.IsUnauthorized(err) {
			writeError(w, http.StatusNotFound, msgTraceNotFound)
			return nil, false
		}
		s.log.Error(err, "authorize trace access: agent read failed", "traceID", traceID, "namespace", ns, "agent", name)
		writeError(w, http.StatusInternalServerError, "failed to authorize trace access")
		return nil, false
	}
	return &detail, true
}

// traceRunSteps summarizes the run that maps a trace so a reader can tell a complete trace from one whose
// spans are still arriving: the trace backend ingests a run's generation and tool spans seconds after the
// launcher's root span. Step counts are reported only once the run is terminal, because a live run's
// event log is still growing and reading it would wait on the run.
func (s *Server) traceRunSteps(ctx context.Context, rn *run.Run) *TraceRunSteps {
	out := &TraceRunSteps{RunID: rn.ID, Status: string(rn.Status)}
	if !rn.Status.IsTerminal() {
		return out
	}
	out.EndedAt = rn.UpdatedAt.UTC().Format(time.RFC3339Nano)
	model, tool, err := s.countRecordedSteps(ctx, rn.ID)
	if err != nil {
		// Without the counts a reader falls back to watching the span set settle; the trace itself is
		// still served.
		s.log.Error(err, "trace detail: reading the run's step events failed", "run", rn.ID)
		return out
	}
	out.ModelSteps, out.ToolSteps = model, tool
	return out
}

// countRecordedSteps counts a terminal run's model and tool step frames (ADR 0071 §4). It reads until
// the store closes the stream, which for a terminal run happens once the backlog is delivered. The
// in-memory store closes early when a backlog overflows its buffer, so a very long run there can
// under-count.
func (s *Server) countRecordedSteps(ctx context.Context, runID string) (model, tool int, err error) {
	events, cancel, err := s.runStore.Subscribe(runID, 0)
	if err != nil {
		return 0, 0, err
	}
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case ev, open := <-events:
			if !open {
				return model, tool, nil
			}
			if ev.Kind != run.EventStep {
				continue
			}
			kind, _, ok := parseStepFrame(ev.Data)
			if !ok {
				continue
			}
			if kind == stepFrameKindModel {
				model++
			} else {
				tool++
			}
		}
	}
}
