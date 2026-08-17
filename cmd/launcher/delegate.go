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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ctxmesh/agent-engine/internal/runcap"
)

// delegate.go — the launcher's delegate_to endpoint (M64, ADR 0057 Door 2): sub-agent-as-a-tool with
// SYNCHRONOUS tool-semantics over a durable-async sub-run. The supervisor's SDK POSTs delegate_to to this
// launcher-LOCAL endpoint (platform-owned — the agent's user code never stamps the spawn envelope). The
// launcher applies the spawn GUARD (m64.5), creates the sub-run via the BFF's capability-authorized spawn
// edge (m64.4), BLOCKS until the sub-run is terminal, then returns the result — so the tool loop + trace
// tree just work. A guard denial or a sub-run failure is returned as an honest tool result (the model
// sees it and can adapt), never a launcher crash.

// Spawn-context headers the run-worker sets when it invokes a run (m64.8 wires them); the launcher reads
// them to bound the spawn tree. Absent ⇒ a root supervisor (first delegation): depth 0, ancestry [self].
const (
	headerSpawnRoot  = "X-Ctxmesh-Spawn-Root"
	headerSpawnDepth = "X-Ctxmesh-Spawn-Depth"
	headerSpawnPath  = "X-Ctxmesh-Spawn-Path" // comma-separated agent ancestry, root→self
)

// runStatusSucceeded is the one terminal state that carries an answer (the others carry an error).
const runStatusSucceeded = "succeeded"

// terminalRunStatuses are the sub-run states that end an await (mirrors internal/run without importing it).
var terminalRunStatuses = map[string]bool{
	runStatusSucceeded: true, "failed": true, "cancelled": true, "expired": true,
}

// delegateRequest is the supervisor SDK's delegate_to body.
type delegateRequest struct {
	SubAgent string          `json:"subAgent"`
	Input    json.RawMessage `json:"input"`
	Step     string          `json:"step"`   // supervisor loop iteration (idempotency)
	CallID   string          `json:"callId"` // the model's tool-call id (idempotency)
}

// delegateResponse is what the SDK gets — the sub-run's answer, or an honest refusal/failure the model
// receives as the tool result (ok=false + error).
type delegateResponse struct {
	OK     bool   `json:"ok"`
	SubRun string `json:"subRun,omitempty"`
	Answer string `json:"answer,omitempty"`
	Error  string `json:"error,omitempty"`
}

// spawnedRunResult is the terminal outcome of a sub-run.
type spawnedRunResult struct {
	Status string
	Answer string
	Error  string
}

// bffSpawnBody is the launcher→BFF POST /api/internal/spawn body (mirrors bff.SpawnRunRequest). The
// budget is relayed from the launcher's controller-injected env (trusted) so the BFF enforces it
// AUTHORITATIVELY against the verified parent — the launcher's own guard reads agent-supplied inputs and
// is only an advisory fast-path (the M64 security-review P1-A fix).
type bffSpawnBody struct {
	SubAgent       string          `json:"subAgent"`
	Endpoint       string          `json:"endpoint"`
	Input          json.RawMessage `json:"input"`
	Step           string          `json:"step"`
	CallID         string          `json:"callId"`
	MaxSpawnDepth  int             `json:"maxSpawnDepth,omitempty"`
	MaxTotalSpawns int             `json:"maxTotalSpawns,omitempty"`
}

// spawnClient creates a sub-run via the BFF + awaits its terminal status, and relays a handoff to the
// BFF handoff edge (m67.6). An interface so the delegate + handoff handlers unit-test against a fake
// (no BFF). The production httpSpawnClient implements all three over the shared BFF base + HTTP client.
type spawnClient interface {
	Spawn(ctx context.Context, capToken string, body bffSpawnBody) (subRunID string, err error)
	Await(ctx context.Context, capToken, subRunID string) (spawnedRunResult, error)
	handoffClient
}

// httpSpawnClient is the production client over the BFF's capability-authorized spawn + await endpoints.
type httpSpawnClient struct {
	bffURL string
	hc     *http.Client
	poll   time.Duration
}

func newHTTPSpawnClient(bffURL string) *httpSpawnClient {
	return &httpSpawnClient{
		bffURL: strings.TrimRight(bffURL, "/"),
		hc:     &http.Client{Timeout: 30 * time.Second, CheckRedirect: refuseRedirect},
		poll:   500 * time.Millisecond,
	}
}

func (c *httpSpawnClient) Spawn(ctx context.Context, capToken string, body bffSpawnBody) (string, error) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.bffURL+"/api/internal/spawn", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(runcap.HeaderName, capToken)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return "", fmt.Errorf("spawn rejected (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (c *httpSpawnClient) Await(ctx context.Context, capToken, subRunID string) (spawnedRunResult, error) {
	ticker := time.NewTicker(c.poll)
	defer ticker.Stop()
	for {
		res, terminal, err := c.poll1(ctx, capToken, subRunID)
		if err != nil {
			return spawnedRunResult{}, err
		}
		if terminal {
			return res, nil
		}
		select {
		case <-ctx.Done():
			return spawnedRunResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *httpSpawnClient) poll1(ctx context.Context, capToken, subRunID string) (spawnedRunResult, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.bffURL+"/api/internal/runs/"+subRunID, nil)
	if err != nil {
		return spawnedRunResult{}, false, err
	}
	req.Header.Set(runcap.HeaderName, capToken)
	resp, err := c.hc.Do(req)
	if err != nil {
		return spawnedRunResult{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return spawnedRunResult{}, false, fmt.Errorf("await failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Status string `json:"status"`
		Answer string `json:"answer"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return spawnedRunResult{}, false, err
	}
	return spawnedRunResult{Status: out.Status, Answer: out.Answer, Error: out.Error}, terminalRunStatuses[out.Status], nil
}

// delegateConfig is the delegate endpoint's config (parsed from env alongside the A2A config).
type delegateConfig struct {
	SelfName  string
	Namespace string
	Scope     string   // the guard counter partition (tenant id, or namespace when untenanted)
	Roster    []string // the team roster member names (from DELEGATE_ROSTER) — the trust boundary for handoff
	Budget    SpawnBudget
}

// delegateRuntime is the resolved delegate wiring — enabled only for a team SUPERVISOR (the controller
// injects DELEGATE_ENABLED + the spawn budget), and only when the BFF spawn edge + the shared Valkey are
// reachable. nil-of-both (Enabled false) ⇒ no delegate listener (a plain agent is unchanged).
type delegateRuntime struct {
	Enabled   bool
	Port      int
	BFFURL    string
	QuotaAddr string
	// ProxyURL/TokenPath (M94): when STATELAYER_PROXY_URL is set the spawn guard goes through the pod-authed
	// state-layer proxy (httpSpawnStore) instead of direct Valkey (QuotaAddr) — so a supervisor holds no
	// direct :6379 path. The controller injects one OR the other (proxy-on gates off TENANT_QUOTA_ADDR).
	ProxyURL  string
	TokenPath string
	cfg       delegateConfig
}

// defaultDelegatePort is the launcher-local port the delegate listener binds (A2A :2997, gateway :2996,
// feedback :2995 are taken — delegate takes :2994).
const defaultDelegatePort = 2994

// loadDelegateConfig parses the delegate wiring from env. Disabled unless DELEGATE_ENABLED=true (a
// supervisor). Budget defaults to the AgentTeam CRD defaults (4/3/20) when the SPAWN_* envs are unset.
func loadDelegateConfig(lookup func(string) string) delegateRuntime {
	if strings.TrimSpace(lookup("DELEGATE_ENABLED")) != "true" {
		return delegateRuntime{}
	}
	scope := strings.TrimSpace(lookup("TENANT_ID"))
	if scope == "" {
		scope = strings.TrimSpace(lookup("POD_NAMESPACE"))
	}
	return delegateRuntime{
		Enabled:   true,
		Port:      envIntDefault(lookup, "DELEGATE_PORT", defaultDelegatePort),
		BFFURL:    strings.TrimSpace(lookup("BFF_INTERNAL_URL")),
		QuotaAddr: strings.TrimSpace(lookup("TENANT_QUOTA_ADDR")),
		ProxyURL:  strings.TrimSpace(lookup("STATELAYER_PROXY_URL")),
		TokenPath: resolvePodTokenPath(lookup("STATELAYER_TOKEN_PATH")),
		cfg: delegateConfig{
			SelfName:  strings.TrimSpace(lookup("AGENT_NAME")),
			Namespace: strings.TrimSpace(lookup("POD_NAMESPACE")),
			Scope:     scope,
			Roster:    parseRosterNames(lookup("DELEGATE_ROSTER")),
			Budget: SpawnBudget{
				MaxFanOut:      envIntDefault(lookup, "SPAWN_MAX_FANOUT", 4),
				MaxSpawnDepth:  envIntDefault(lookup, "SPAWN_MAX_DEPTH", 3),
				MaxTotalSpawns: envIntDefault(lookup, "SPAWN_MAX_TOTAL", 20),
			},
		},
	}
}

func envIntDefault(lookup func(string) string, name string, def int) int {
	if v := strings.TrimSpace(lookup(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// buildServer returns the http.Server for the delegate listener (nil when disabled or under-configured).
func (dr delegateRuntime) buildServer() *http.Server {
	// The spawn guard needs a counter store: the state-layer PROXY (preferred — no direct Valkey path) or,
	// pre-cutover, direct Valkey (QuotaAddr). Under-configured (neither) ⇒ no delegate listener.
	if !dr.Enabled || dr.BFFURL == "" || (dr.QuotaAddr == "" && dr.ProxyURL == "") {
		return nil
	}
	guard := NewSpawnGuard(dr.spawnStore())
	client := newHTTPSpawnClient(dr.BFFURL)
	return &http.Server{
		Addr:    fmt.Sprintf(":%d", dr.Port),
		Handler: newDelegateServer(dr.cfg, guard, client).handler(),
	}
}

// spawnStore selects the spawn-guard counter store: the pod-authed state-layer PROXY when STATELAYER_PROXY_URL
// is set (M94 — no direct Valkey path), else the direct-Valkey store (pre-cutover). The proxy path fails
// CLOSED on any proxy error (SpawnGuard.Admit maps a store error to SpawnDeniedError).
func (dr delegateRuntime) spawnStore() spawnGuardStore {
	if dr.ProxyURL != "" {
		return newHTTPSpawnStore(dr.ProxyURL, dr.TokenPath)
	}
	return newRedisSpawnStore(dr.QuotaAddr)
}

// delegateServer serves the launcher-local delegate_to endpoint.
type delegateServer struct {
	cfg    delegateConfig
	guard  *SpawnGuard
	client spawnClient
}

func newDelegateServer(cfg delegateConfig, guard *SpawnGuard, client spawnClient) *delegateServer {
	return &delegateServer{cfg: cfg, guard: guard, client: client}
}

// targetURL resolves a roster member's cluster-local ksvc URL (the A2A convention).
func (s *delegateServer) targetURL(subAgent string) string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local", subAgent, s.cfg.Namespace)
}

func (s *delegateServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /delegate", s.handleDelegate)
	// handoff_to (m67.6, ADR 0060 §5) shares this listener — a supervisor with a roster can both delegate
	// (call-and-return) and hand off (transfer-of-control) to the same roster.
	mux.HandleFunc("POST /handoff", s.handleHandoff)
	return mux
}

// parseRosterNames extracts the roster member names from the DELEGATE_ROSTER env (a JSON array of
// {"name","description"} the controller injects from the AgentTeam roster). It is the trust boundary for
// handoff (B must be a roster member). A malformed/empty env ⇒ nil (an empty roster admits any target at
// the launcher — the BFF is still authoritative).
func parseRosterNames(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if n := strings.TrimSpace(e.Name); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// handleDelegate runs the synchronous delegate: guard → spawn → await → result. It always returns 200 with
// a delegateResponse (ok true/false) so the SDK surfaces a refusal/failure as the tool RESULT the model
// reads — a denied or failed delegation is information for the supervisor, not an HTTP error.
func (s *delegateServer) handleDelegate(w http.ResponseWriter, r *http.Request) {
	var req delegateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeDelegate(w, delegateResponse{OK: false, Error: "invalid delegate request"})
		return
	}
	req.SubAgent = strings.TrimSpace(req.SubAgent)
	req.Step = strings.TrimSpace(req.Step)
	req.CallID = strings.TrimSpace(req.CallID)
	if req.SubAgent == "" || req.Step == "" || req.CallID == "" {
		writeDelegate(w, delegateResponse{OK: false, Error: "subAgent, step, and callId are required"})
		return
	}
	capToken := strings.TrimSpace(r.Header.Get(runcap.HeaderName))
	if capToken == "" {
		writeDelegate(w, delegateResponse{OK: false, Error: "no run capability — delegation needs an authenticated run"})
		return
	}

	// Spawn-tree context (from the run-worker's headers; first-hop defaults for a root supervisor).
	root := strings.TrimSpace(r.Header.Get(headerSpawnRoot))
	depth := headerInt(r, headerSpawnDepth, 0)
	ancestry := splitPath(r.Header.Get(headerSpawnPath), s.cfg.SelfName)

	// (m64.5) Guard admission — fail-closed. A denial is an honest tool result, not a spawn.
	dec, err := s.guard.Admit(r.Context(), SpawnRequest{
		Scope: s.cfg.Scope, RootRunID: root, ChildDepth: depth + 1,
		TargetAgent: req.SubAgent, Ancestry: ancestry, Budget: s.cfg.Budget,
	})
	if dec != SpawnAdmitted {
		writeDelegate(w, delegateResponse{OK: false, Error: fmt.Sprintf("delegation to %q refused: %s", req.SubAgent, dec)})
		return
	}
	// From here an in-flight slot is held — release it once the sub-run is terminal (or on failure).
	defer func() { _ = s.guard.Release(context.Background(), s.cfg.Scope, root) }()
	_ = err // Admit returns (SpawnAdmitted, nil) here

	subRunID, err := s.client.Spawn(r.Context(), capToken, bffSpawnBody{
		SubAgent: req.SubAgent, Endpoint: s.targetURL(req.SubAgent), Input: req.Input,
		Step: req.Step, CallID: req.CallID,
		// Relay the TRUSTED budget (env, controller-injected) so the BFF enforces it authoritatively.
		MaxSpawnDepth: s.cfg.Budget.MaxSpawnDepth, MaxTotalSpawns: s.cfg.Budget.MaxTotalSpawns,
	})
	if err != nil {
		writeDelegate(w, delegateResponse{OK: false, Error: "spawn failed: " + err.Error()})
		return
	}

	res, err := s.client.Await(r.Context(), capToken, subRunID)
	if err != nil {
		writeDelegate(w, delegateResponse{OK: false, SubRun: subRunID, Error: "await failed: " + err.Error()})
		return
	}
	if res.Status != runStatusSucceeded {
		errMsg := res.Error
		if errMsg == "" {
			errMsg = "sub-run ended " + res.Status
		}
		writeDelegate(w, delegateResponse{OK: false, SubRun: subRunID, Error: errMsg})
		return
	}
	writeDelegate(w, delegateResponse{OK: true, SubRun: subRunID, Answer: res.Answer})
}

func writeDelegate(w http.ResponseWriter, resp delegateResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func headerInt(r *http.Request, name string, def int) int {
	if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// splitPath parses the comma-separated ancestry header and appends self (so the cycle guard sees the full
// root→self path). An empty header ⇒ just [self] (a root supervisor's first delegation).
func splitPath(raw, self string) []string {
	out := make([]string, 0, 4)
	for p := range strings.SplitSeq(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return append(out, self)
}
