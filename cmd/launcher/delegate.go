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
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ctxmesh/agentry/internal/runcap"
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
	SubAgent string `json:"subAgent"`
	// Capability delegates by WHAT the sub-agent must be able to do rather than by its name (M141.4,
	// ADR 0120). Used only when SubAgent is empty: the launcher asks the platform's discovery edge to rank
	// the caller's registry by this capability and delegates to the best match. It exists because a
	// supervisor should be able to say "I need something that can summarize a PDF" without having been
	// wired, at authoring time, to the name of the agent that can.
	Capability string          `json:"capability,omitempty"`
	Input      json.RawMessage `json:"input"`
	Step       string          `json:"step"`   // supervisor loop iteration (idempotency)
	CallID     string          `json:"callId"` // the model's tool-call id (idempotency)
	// Suspend asks for the L7 durable-suspend path (ADR 0091): the launcher resolves + budget-checks the
	// delegation and returns a suspend-SIGNAL (endpoint, no spawn, no block) so the SDK can collect the
	// turn's delegations and SUSPEND once. Honored only at depth 0 (a root supervisor); a depth>0 request
	// falls through to the blocking path — nested suspension is v1-deferred (fail-closed).
	Suspend bool `json:"suspend,omitempty"`
}

// delegateResponse is what the SDK gets — the sub-run's answer, or an honest refusal/failure the model
// receives as the tool result (ok=false + error), or (L7) a suspend-signal the SDK turns into a durable
// suspend.
type delegateResponse struct {
	OK bool `json:"ok"`
	// SubAgent echoes the agent actually delegated to. It matters for delegate-by-capability (M141.4):
	// the caller named a capability, not an agent, so without this it would never learn WHO ran — and the
	// L7 suspend path would checkpoint an empty target, stranding the child run.
	SubAgent string `json:"subAgent,omitempty"`
	SubRun   string `json:"subRun,omitempty"`
	Answer   string `json:"answer,omitempty"`
	Error    string `json:"error,omitempty"`
	// Suspend + Endpoint are the L7 suspend-signal (ADR 0091): the launcher budget-checked and RESOLVED the
	// target endpoint but did NOT spawn or block. The SDK collects these and suspends once; the BFF worker
	// creates the child run at Endpoint inside the suspend transaction (m108.4b).
	Suspend  bool   `json:"suspend,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
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
	// Discover ranks the caller's registry by capability and returns the best-matching agent names, best
	// first. Empty (no error) means nobody advertises the capability — an honest answer, not a failure.
	Discover(ctx context.Context, capToken, capability string, topK int) ([]string, error)
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

// Discover relays a capability query to the BFF's capability-authorized discovery edge (M141.2). The
// launcher passes the run capability through and does NOT get to choose the candidate set: the BFF
// resolves the caller's registry from the control plane, so a compromised pod cannot widen its own reach
// by asking about someone else's registry.
func (c *httpSpawnClient) Discover(ctx context.Context, capToken, capability string, topK int) ([]string, error) {
	raw, _ := json.Marshal(map[string]any{wireKeyQuery: capability, wireKeyTopK: topK})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.bffURL+"/api/internal/discover",
		bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(runcap.HeaderName, capToken)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return nil, fmt.Errorf("discovery rejected (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		Agents []struct {
			Name string `json:"name"`
		} `json:"agents"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Agents))
	for _, a := range out.Agents {
		names = append(names, a.Name)
	}
	return names, nil
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

// discoverTopK is how many candidates the launcher asks for when delegating by capability. It takes the
// top one, but asks for a few: the extras cost nothing and make the refusal message able to say what the
// runner-up was, which is far more useful to a model than a bare "no match".
const discoverTopK = 3

// resolveByCapability turns "I need something that can do X" into a concrete sub-agent name via the
// platform's discovery edge. The launcher deliberately does NOT rank locally: the BFF resolves the
// caller's registry from the control plane, so a compromised pod cannot nominate its own candidate set.
func (s *delegateServer) resolveByCapability(ctx context.Context, capToken, capability string) (string, error) {
	names, err := s.client.Discover(ctx, capToken, capability, discoverTopK)
	if err != nil {
		return "", fmt.Errorf("could not resolve an agent for %q: %w", capability, err)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no agent in your registry advertises %q — name a sub-agent instead, "+
			"or have one publish that capability", capability)
	}
	return names[0], nil
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
	// The run-capability verifier (same public key the gateway uses) recovers a root supervisor's own run
	// id as the spawn-tree root when the spawn-root header did not propagate (L11). nil when the key is
	// absent/bad ⇒ the advisory guard degrades to the scope bucket, never failing a delegation.
	verifier := buildCapVerifier(func(f string, a ...any) { log.Printf(f, a...) })
	return &http.Server{
		Addr:    fmt.Sprintf(":%d", dr.Port),
		Handler: newDelegateServer(dr.cfg, guard, client, verifier).handler(),
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
	// verifier recovers THIS run's id from the run capability as the spawn-tree ROOT fallback (L11) when
	// the X-Ctxmesh-Spawn-Root header did not propagate to /delegate. nil ⇒ the fallback is disabled and
	// the advisory guard keys on the scope bucket as before (the BFF's server-side budget stays
	// authoritative, C19) — so a missing/bad capability public key never breaks a delegation.
	verifier *runcap.Verifier
}

func newDelegateServer(
	cfg delegateConfig, guard *SpawnGuard, client spawnClient, verifier *runcap.Verifier,
) *delegateServer {
	return &delegateServer{cfg: cfg, guard: guard, client: client, verifier: verifier}
}

// selfRunID recovers THIS run's id from the VERIFIED run capability — the authoritative spawn-tree root
// for a ROOT supervisor whose X-Ctxmesh-Spawn-Root header did not reach /delegate (L11: an SDK-driven
// delegation historically relayed no spawn headers, so the guard degraded to root "" and every tree
// double-counted in ONE scope bucket). Returns "" when the token can't be verified (no verifier, or a
// bad/expired token): the advisory counter then keys on the scope bucket as before and NEVER fails the
// delegation — the BFF's server-side spawn budget is authoritative (C19). Verifying (not merely reading
// the `run` claim) keeps this security-adjacent partition key from being agent-forgeable.
func (s *delegateServer) selfRunID(capToken string) string {
	if s.verifier == nil || capToken == "" {
		return ""
	}
	verified, err := s.verifier.Verify(capToken)
	if err != nil {
		return ""
	}
	return verified.RunID
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
	req.Capability = strings.TrimSpace(req.Capability)
	req.Step = strings.TrimSpace(req.Step)
	req.CallID = strings.TrimSpace(req.CallID)
	if req.Step == "" || req.CallID == "" {
		writeDelegate(w, delegateResponse{OK: false, Error: "step and callId are required"})
		return
	}
	if req.SubAgent == "" && req.Capability == "" {
		writeDelegate(w, delegateResponse{OK: false, Error: "name a subAgent or describe the capability you need"})
		return
	}
	capToken := strings.TrimSpace(r.Header.Get(runcap.HeaderName))
	if capToken == "" {
		writeDelegate(w, delegateResponse{OK: false, Error: "no run capability — delegation needs an authenticated run"})
		return
	}

	// Delegate BY CAPABILITY (M141.4): with no name given, ask the platform who can do this. Resolution
	// happens BEFORE the spawn guard so the guard sees the real target — its per-target accounting and the
	// ancestry cycle check are meaningless against a placeholder. An honest refusal (nobody advertises the
	// capability, or discovery is unavailable) comes back as a tool result the model can react to, never
	// as a silent fallback to some arbitrary agent.
	if req.SubAgent == "" {
		resolved, dErr := s.resolveByCapability(r.Context(), capToken, req.Capability)
		if dErr != nil {
			writeDelegate(w, delegateResponse{OK: false, Error: dErr.Error()})
			return
		}
		req.SubAgent = resolved
	}

	// Spawn-tree context (from the run-worker's headers; first-hop defaults for a root supervisor).
	// L11: when the spawn-root header did not propagate (an SDK-driven delegation historically relayed
	// none), key the advisory guard on THIS run's own identity — the authoritative tree root for a root
	// supervisor — recovered from the VERIFIED run capability, instead of degrading to the scope-global
	// "" bucket where every tree double-counts. The BFF's server-side spawn budget stays authoritative.
	root := strings.TrimSpace(r.Header.Get(headerSpawnRoot))
	if root == "" {
		root = s.selfRunID(capToken)
	}
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

	// L7 durable suspend (ADR 0091), depth-agnostic (ADR 0108): a supervisor that intends to SUSPEND —
	// at ANY delegation depth — gets a resolve-only signal. The launcher has budget-checked (Admit above)
	// and resolves the target endpoint, but does NOT spawn and does NOT block. The SDK collects the turn's
	// signals and suspends ONCE (fan-out = one suspend); the BFF worker then creates the child run at this
	// endpoint inside the suspend transaction. M138 lifted the depth-0 gate — a sub-run that is itself a
	// supervisor now suspends too (the wake machinery is depth-generic: CompleteAndWake fires only on a
	// child's TERMINAL transition, so a mid-tree supervisor's waiting→queued churn is invisible upward).
	// The blocking spawn+await path below STAYS: the resume re-dispatch rides it (short-circuiting on the
	// already-terminal child), and a non-suspend delegation still blocks as before.
	if req.Suspend {
		writeDelegate(w, delegateResponse{
			OK: true, Suspend: true, SubAgent: req.SubAgent, Endpoint: s.targetURL(req.SubAgent),
		})
		return
	}

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
	writeDelegate(w, delegateResponse{OK: true, SubAgent: req.SubAgent, SubRun: subRunID, Answer: res.Answer})
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
