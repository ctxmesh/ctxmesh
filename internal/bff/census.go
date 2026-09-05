package bff

import (
	"context"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// census.go — GET /api/agents/census, the whole-fleet shape behind Home's lifecycle bar.
//
// GET /api/agents caps a page at maxListLimit, and the console's fleet window is set to
// exactly that ceiling. Above it the page degraded to "not yet known" — so the console
// said LESS the larger the install, which is backwards. A lower bound cannot repair it
// either: clamped at the page size, "200+" is the same string for a fleet of 201 and a
// fleet of 20,000, a fact about our fetch rather than their cluster.
//
// Two shortcuts are deliberately not taken. There is no shared informer to total up:
// every request runs through callerClient so Kubernetes RBAC stays the sole authority
// (ADR 0011). And Prometheus holds ctxmesh_agent_replicas, one count() from a total, but
// is not RBAC-scoped — the number would silently include agents the caller may not see.
// A count that leaks is worse than no count.
//
// What comes back is a FREQUENCY TABLE of status tuples, not bucket counts. The bucketing
// heuristic is six interacting regexes in resolveStatus (ui/src/components/kit/status-badge.tsx)
// where terminal reasons must beat converging ones; reimplementing that in Go would drift
// the moment either side is edited. Distinct (ready, phase, reason, isDraft) tuples are
// bounded — the controller writes enum-ish CamelCase reasons — so this response stays
// small however large the fleet grows.
const (
	// censusPageSize is the per-page ask. This is an in-cluster round trip rather than
	// a browser one, so it is larger than the public maxListLimit.
	censusPageSize = 500
	// censusMaxAgents bounds the work a single request will do. Past it Total is a
	// lower bound and Complete says so — never a flat number presented as a total.
	censusMaxAgents = 10000
	// censusMaxGroups bounds the table against a fleet whose reasons carry unique text.
	// Overflow is never dropped from Total; it is left out of Groups and declared.
	censusMaxGroups = 512
)

// CensusGroup is one distinct status tuple and how many agents carry it. The caller
// buckets it — these are the raw inputs to resolveStatus, not a verdict.
type CensusGroup struct {
	Ready   bool   `json:"ready"`
	Phase   string `json:"phase,omitempty"`
	Reason  string `json:"reason,omitempty"`
	IsDraft bool   `json:"isDraft"`
	Count   int    `json:"count"`
}

// CensusResponse is returned by GET /api/agents/census.
//
// Total always counts every agent scanned, including any whose tuple did not fit in
// Groups — so Total is never smaller than the truth about what was read. The remainder
// (Total minus the sum of Groups) is the unclassified count, and the caller renders it
// as such rather than silently losing it.
type CensusResponse struct {
	Total int `json:"total"`
	// Complete is false when the scan stopped at censusMaxAgents. Total is then a
	// lower bound, and the UI must render a bound rather than a number.
	Complete bool `json:"complete"`
	// GroupsComplete is false when distinct tuples exceeded censusMaxGroups.
	GroupsComplete bool          `json:"groupsComplete"`
	Groups         []CensusGroup `json:"groups"`
}

// handleAgentCensus serves GET /api/agents/census[?namespace=].
//
// Drafts are always included and flagged: the lifecycle bar has a draft stage, and
// excluding them here would make the stage permanently read zero.
func (s *Server) handleAgentCensus(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}

	resp, err := censusScan(r.Context(), caller, r.URL.Query().Get("namespace"))
	if err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "census: list AgentDeployments failed")
		writeError(w, http.StatusInternalServerError, "failed to count agents")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// censusScan walks every page the caller can see and tallies status tuples. It is a
// free function over the AgentReader seam so the paging loop, the ceiling and the group
// cap are testable without a live API server — the controller-runtime fake client
// ignores Limit and Continue entirely, so a test that goes through it proves the tally
// and nothing about the walk.
func censusScan(ctx context.Context, r AgentReader, namespace string) (CensusResponse, error) {
	type tuple struct {
		ready   bool
		phase   string
		reason  string
		isDraft bool
	}
	counts := make(map[tuple]int)
	resp := CensusResponse{Complete: true, GroupsComplete: true, Groups: []CensusGroup{}}

	cursor := ""
	for {
		opts := []client.ListOption{client.Limit(int64(censusPageSize))}
		if cursor != "" {
			opts = append(opts, client.Continue(cursor))
		}
		if namespace != "" {
			opts = append(opts, client.InNamespace(namespace))
		}

		list, err := listAgentDeployments(ctx, r, opts...)
		if err != nil {
			return CensusResponse{}, err
		}

		for i := range list.Items {
			ad := &list.Items[i]
			sum := newAgentSummary(ad)
			k := tuple{
				ready:   sum.Ready,
				phase:   sum.Phase,
				reason:  sum.Reason,
				isDraft: isDraftAgent(ad),
			}
			// Total counts the agent either way. Dropping it here to keep the table
			// small would undercount the fleet, which is the defect this endpoint
			// exists to remove.
			resp.Total++
			if _, seen := counts[k]; !seen && len(counts) >= censusMaxGroups {
				resp.GroupsComplete = false
				continue
			}
			counts[k]++
		}

		cursor = list.Continue
		if cursor == "" {
			break
		}
		if resp.Total >= censusMaxAgents {
			resp.Complete = false
			break
		}
	}

	for k, n := range counts {
		resp.Groups = append(resp.Groups, CensusGroup{
			Ready:   k.ready,
			Phase:   k.phase,
			Reason:  k.reason,
			IsDraft: k.isDraft,
			Count:   n,
		})
	}
	return resp, nil
}
