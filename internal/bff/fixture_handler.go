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
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ctxmesh/ctxmesh/internal/replay"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// RunFixtureDTO is the GET /api/runs/{id}/fixture response — the O10a fixture stepper's data (ADR
// 0071 §5). It is the console face of the CI replay fixture: the run's step timeline joined to its
// WIRE-EXACT recorded I/O (the same bytes `dev --replay` re-serves — "what you see is what CI
// replays"). Recorded=false means the run was not recorded; Steps then carries the timeline with
// every step a gap.
type RunFixtureDTO struct {
	RunID    string          `json:"runId"`
	Agent    string          `json:"agent"`
	Recorded bool            `json:"recorded"`
	Steps    []replay.StepIO `json:"steps"`
}

// handleGetRunFixture serves GET /api/runs/{id}/fixture — the recorded fixture's per-step wire I/O for
// the console stepper (O10a, ADR 0071 §5). CALLER-SCOPED (ADR 0011): a fixture holds full prompts +
// tool results (sensitive-by-default, ADR 0071 C4), so authorizeRunAccess proves the caller can read
// the run's backing through their OWN RBAC before any bytes are served — a run id is never a
// cross-tenant read oracle. The join (step timeline ⋈ fixture channels) is the fail-honest resolver
// (replay.ResolveSteps, m109.1): a step it cannot confidently back becomes a gap, never a mis-join.
func (s *Server) handleGetRunFixture(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	rn, ok := s.authorizeRunAccess(w, r, caller, id, true) // 403 on denial, 404 on missing (no oracle)
	if !ok {
		return
	}

	// The step timeline comes from the run's persisted EventStep frames; the wire I/O from the fixture.
	steps := s.runStepDescriptors(id)

	// Load the recorded fixture. No object store configured ⇒ honest 501 (the export-handler
	// docStore-nil pattern) — the feature needs OBJECT_STORE_* on the BFF (m109.3). A run with no
	// fixture (not recorded / wrong id) is NOT an error — it is an honest "recorded: false" with the
	// timeline still shown (every step a gap), so the console can say "this run was not recorded".
	if s.docStore == nil {
		writeError(w, http.StatusNotImplemented, "run fixtures are unavailable: no object store is configured")
		return
	}
	fs, err := replay.NewFixtureStore(s.docStore)
	if err != nil {
		s.log.Error(err, "run: could not open the fixture store", "run", id)
		writeError(w, http.StatusInternalServerError, "could not open the fixture store")
		return
	}
	fx, err := fs.GetRun(r.Context(), id)
	recorded := true
	if err != nil {
		if errors.Is(err, replay.ErrNoFixture) {
			fx, recorded = nil, false // honest empty result — the timeline still renders as gaps
		} else {
			s.log.Error(err, "run: could not read the run fixture", "run", id)
			writeError(w, http.StatusBadGateway, "could not read the run fixture")
			return
		}
	}

	writeJSON(w, http.StatusOK, RunFixtureDTO{
		RunID:    rn.ID,
		Agent:    rn.Agent,
		Recorded: recorded,
		Steps:    replay.ResolveSteps(fx, steps),
	})
}

// runStepDescriptors reads a run's persisted EventStep frames (in seq order) and projects them to the
// ordered model/tool step descriptors the fixture-stepper join consumes. It drains the event BACKLOG
// only (Subscribe from 0, cancel immediately) — the stepper is post-hoc over a completed run, so it
// never waits for live events. EventStep is also used for workflow plan-approval labels (plain text,
// not a step frame); those don't parse to a model/tool frame and are skipped.
func (s *Server) runStepDescriptors(runID string) []replay.StepDescriptor {
	ch, cancel, err := s.runStore.Subscribe(runID, 0)
	if err != nil {
		return nil
	}
	cancel() // drain the backlog, do not block on live events
	var out []replay.StepDescriptor
	for ev := range ch {
		if ev.Kind != run.EventStep {
			continue
		}
		var frame struct {
			Kind string `json:"kind"`
			Tool string `json:"tool"`
		}
		if json.Unmarshal([]byte(ev.Data), &frame) != nil {
			continue // a non-frame EventStep (e.g. a workflow plan-approval label)
		}
		if frame.Kind != stepFrameKindModel && frame.Kind != stepFrameKindTool {
			continue
		}
		out = append(out, replay.StepDescriptor{Kind: frame.Kind, ToolName: frame.Tool})
	}
	return out
}

// The EventStep frame kinds the fixture stepper joins on (mirrors the SDK `_step_frame` `kind`).
const (
	stepFrameKindModel = "model"
	stepFrameKindTool  = "tool"
)
