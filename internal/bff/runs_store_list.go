package bff

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ctxmesh/ctxmesh/internal/run"
)

// The Runs list when no trace store is wired (ADR 0150). The run store is on in every install, and every
// run made through POST /api/runs is in it; before this, GET /api/runs answered 501 on any install
// without Langfuse, so a stock install's console could not list a single run.

// storeRunSummary is one Runs-list row read from the run store. Cost and tokens are absent rather than
// zero: they come from a trace backend. LatencyMs is present only once the run has finished.
type storeRunSummary struct {
	RunID     string   `json:"runId"`
	TraceID   string   `json:"traceId"`
	Name      string   `json:"name"`
	Timestamp string   `json:"timestamp"`
	LatencyMs *float64 `json:"latencyMs,omitempty"`
	AgentNs   string   `json:"agentNs"`
	AgentName string   `json:"agentName"`
	Status    string   `json:"status"`
}

type storeRunListResponse struct {
	Runs       []storeRunSummary `json:"runs"`
	NextCursor string            `json:"nextCursor"`
}

type storeAgentRunsResponse struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Runs      []storeRunSummary `json:"runs"`
}

// handleStoreRuns serves GET /api/runs from the run store. Same query contract and the same caller
// scoping as the trace-store list: ?agent=ns/name authorizes that agent; the global list covers only the
// agents the caller can list (in ?namespace= when given). ?enrich= is accepted and ignored.
func (s *Server) handleStoreRuns(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	qs := r.URL.Query()
	f := run.RootListFilter{Limit: defaultRunLimit}
	if raw := qs.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		f.Limit = min(n, maxRunLimit)
	}
	var err error
	if f.From, err = parseRunTime("from", qs.Get("from")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if f.To, err = parseRunTime("to", qs.Get("to")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(qs.Get("status")) != "" {
		writeError(w, http.StatusBadRequest, "status filtering is not supported on the runs list; the State column shows each run's outcome")
		return
	}
	if f.Before, err = decodeRootCursor(qs.Get("cursor")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if agent := strings.TrimSpace(qs.Get("agent")); agent != "" {
		if !s.authorizeAgentKey(w, r, caller, agent) {
			return
		}
		ns, name := splitAgentKey(agent)
		f.Agents = []run.AgentKey{{Namespace: ns, Name: name}}
	} else {
		allow, lErr := s.callerVisibleAgentKeys(r.Context(), caller, strings.TrimSpace(qs.Get("namespace")))
		if lErr != nil {
			if status, msg, isRBAC := classifyReadError(lErr); isRBAC {
				writeError(w, status, msg)
				return
			}
			s.log.Error(lErr, "runs list: caller agent-visibility check failed")
			writeError(w, http.StatusInternalServerError, "failed to authorize runs list")
			return
		}
		f.Agents = agentKeysOf(allow)
	}

	page, next, err := s.listRootPage(r, f)
	if err != nil {
		s.log.Error(err, "runs list: run store read failed")
		writeError(w, http.StatusInternalServerError, "failed to list runs")
		return
	}
	// q filters the page that was read, as on the trace-store list; the cursor still advances by the store.
	if q := strings.ToLower(strings.TrimSpace(qs.Get("q"))); q != "" {
		kept := page[:0]
		for _, row := range page {
			if strings.Contains(strings.ToLower(row.Name), q) || strings.Contains(strings.ToLower(row.AgentNs), q) {
				kept = append(kept, row)
			}
		}
		page = kept
	}
	writeJSON(w, http.StatusOK, storeRunListResponse{Runs: page, NextCursor: next})
}

// handleStoreAgentRuns serves GET /api/agents/{ns}/{name}/runs from the run store: the agent page's
// recent runs, after proving the caller can get the agent.
func (s *Server) handleStoreAgentRuns(w http.ResponseWriter, r *http.Request) {
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
	if !s.authorizeAgentKey(w, r, caller, ns+"/"+name) {
		return
	}
	page, _, err := s.listRootPage(r, run.RootListFilter{
		Agents: []run.AgentKey{{Namespace: ns, Name: name}},
		Limit:  parseAgentRunLimit(r.URL.Query().Get("limit")),
	})
	if err != nil {
		s.log.Error(err, "agent runs: run store read failed", "namespace", ns, "agent", name)
		writeError(w, http.StatusInternalServerError, "failed to list agent runs")
		return
	}
	writeJSON(w, http.StatusOK, storeAgentRunsResponse{Namespace: ns, Name: name, Runs: page})
}

// listRootPage reads one page, fetching a row past the limit to learn whether another page exists.
func (s *Server) listRootPage(r *http.Request, f run.RootListFilter) ([]storeRunSummary, string, error) {
	out := []storeRunSummary{}
	if len(f.Agents) == 0 {
		return out, "", nil
	}
	limit := f.Limit
	f.Limit = limit + 1
	rows, err := s.runStore.ListRoots(r.Context(), f)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = encodeRootCursor(run.RootCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	for _, rr := range rows {
		out = append(out, toStoreRunSummary(rr))
	}
	return out, next, nil
}

func toStoreRunSummary(rr run.RootRun) storeRunSummary {
	sum := storeRunSummary{
		RunID:     rr.ID,
		TraceID:   rr.TraceID,
		Name:      rr.Namespace + "/" + rr.Agent, // the trace list names a run by its agent, ns/name
		Timestamp: rr.CreatedAt.UTC().Format(time.RFC3339Nano),
		AgentNs:   rr.Namespace,
		AgentName: rr.Agent,
		Status:    storeRunStatus(rr.Status),
	}
	if rr.Status.IsTerminal() {
		ms := float64(rr.UpdatedAt.Sub(rr.CreatedAt).Milliseconds())
		sum.LatencyMs = &ms
	}
	return sum
}

// storeRunStatus maps a run's state onto the list's vocabulary: "ok" and "error" as the trace-store list
// reports them, any other state by its own name.
// The trace-store list's outcome words, which the console's Runs page renders.
const (
	listStatusOK    = "ok"
	listStatusError = "error"
)

func storeRunStatus(st run.Status) string {
	switch st {
	case run.StatusSucceeded:
		return listStatusOK
	case run.StatusFailed:
		return listStatusError
	default:
		return string(st)
	}
}

func agentKeysOf(allow map[string]bool) []run.AgentKey {
	keys := make([]run.AgentKey, 0, len(allow))
	for k, ok := range allow {
		if !ok {
			continue
		}
		ns, name := splitAgentKey(k)
		if ns == "" || name == "" {
			continue
		}
		keys = append(keys, run.AgentKey{Namespace: ns, Name: name})
	}
	slices.SortFunc(keys, func(a, b run.AgentKey) int {
		if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return keys
}

func parseRunTime(field, raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be RFC3339, got %q", field, raw)
	}
	return t, nil
}

func encodeRootCursor(c run.RootCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID))
}

var errBadRootCursor = errors.New("cursor is not one this list returned")

func decodeRootCursor(raw string) (*run.RootCursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, errBadRootCursor
	}
	at, id, ok := strings.Cut(string(b), "|")
	if !ok || id == "" {
		return nil, errBadRootCursor
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, errBadRootCursor
	}
	return &run.RootCursor{CreatedAt: t, ID: id}, nil
}
