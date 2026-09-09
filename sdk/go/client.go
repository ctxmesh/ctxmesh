package ctxmesh

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Client is the entry point. Construct it with New (in-pod) or NewWithConfig (tests).
type Client struct {
	cfg *Config
	t   *transport

	Memory    *MemoryClient
	Knowledge *KnowledgeClient
	Skills    *SkillsClient
	Feedback  *FeedbackClient
	Mesh      *MeshClient
	Runs      *RunsClient
}

// New reads the launcher environment and returns a Client. It returns ErrNotInPod outside a
// ctxmesh pod.
func New() (*Client, error) {
	cfg, err := FromEnv()
	if err != nil {
		return nil, err
	}
	return NewWithConfig(cfg), nil
}

// NewWithConfig builds a Client from an explicit Config — for tests, or a process that resolves
// the plane itself.
func NewWithConfig(cfg *Config) *Client {
	t := newTransport()
	c := &Client{cfg: cfg, t: t}
	c.Memory = &MemoryClient{c: c}
	c.Knowledge = &KnowledgeClient{c: c}
	c.Skills = &SkillsClient{c: c}
	c.Feedback = &FeedbackClient{c: c}
	c.Mesh = &MeshClient{c: c}
	c.Runs = &RunsClient{c: c}
	return c
}

// Config returns the resolved plane. Useful for asserting what the platform granted.
func (c *Client) Config() Config { return *c.cfg }

// ── memory: /memory and /memory/agent ────────────────────────────────────────

// MemoryClient covers conversation memory (/memory) and this agent's long-term memory
// (/memory/agent).
type MemoryClient struct{ c *Client }

// Entry is one conversation turn.
type Entry struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// Fact is one long-term memory, with the score its retrieval assigned.
type Fact struct {
	Content string            `json:"content"`
	Tags    map[string]string `json:"tags,omitempty"`
	Score   float64           `json:"score,omitempty"`
}

func (m *MemoryClient) require() error {
	if !m.c.cfg.MemoryWired {
		return fmt.Errorf("%w: memory (MEMORY_PORT is unset)", ErrNotWired)
	}
	return nil
}

// convID resolves the conversation: explicit argument first, else the injected CONVERSATION_ID.
// An empty result is an error rather than a silent write to a shared bucket.
func (m *MemoryClient) convID(id string) (string, error) {
	if id == "" {
		id = m.c.cfg.ConversationID
	}
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("ctxmesh: no conversation id (pass one, or CONVERSATION_ID must be set)")
	}
	if strings.ContainsAny(id, "/ \t\n") {
		return "", fmt.Errorf("ctxmesh: conversation id %q contains a separator or whitespace", id)
	}
	return id, nil
}

// Get returns the conversation so far.
func (m *MemoryClient) Get(ctx context.Context, conversationID string) ([]Entry, error) {
	if err := m.require(); err != nil {
		return nil, err
	}
	id, err := m.convID(conversationID)
	if err != nil {
		return nil, err
	}
	var out []Entry
	u := m.c.cfg.memoryBase() + "/memory/" + url.PathEscape(id)
	if err := m.c.t.do(ctx, "GET", u, nil, &out, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// Append adds one entry to the conversation.
func (m *MemoryClient) Append(ctx context.Context, e Entry, conversationID string) error {
	if err := m.require(); err != nil {
		return err
	}
	id, err := m.convID(conversationID)
	if err != nil {
		return err
	}
	u := m.c.cfg.memoryBase() + "/memory/" + url.PathEscape(id) + "/append"
	return m.c.t.do(ctx, "POST", u, e, nil, nil)
}

// Search searches this conversation's memory. capability is optional: without it a per-user
// agent silently reads the agent-wide bucket rather than the caller's own.
func (m *MemoryClient) Search(ctx context.Context, query, conversationID, capability string) ([]Entry, error) {
	if err := m.require(); err != nil {
		return nil, err
	}
	id, err := m.convID(conversationID)
	if err != nil {
		return nil, err
	}
	var out []Entry
	u := m.c.cfg.memoryBase() + "/memory/" + url.PathEscape(id) + "/search?q=" + url.QueryEscape(query)
	h := map[string]string{}
	if capability != "" {
		h[CapabilityHeader] = capability
	}
	if err := m.c.t.do(ctx, "GET", u, nil, &out, h); err != nil {
		return nil, err
	}
	return out, nil
}

// Put replaces the conversation wholesale.
func (m *MemoryClient) Put(ctx context.Context, entries []Entry, conversationID string) error {
	if err := m.require(); err != nil {
		return err
	}
	id, err := m.convID(conversationID)
	if err != nil {
		return err
	}
	u := m.c.cfg.memoryBase() + "/memory/" + url.PathEscape(id)
	return m.c.t.do(ctx, "PUT", u, entries, nil, nil)
}

// Remember writes a fact to this agent's long-term memory.
func (m *MemoryClient) Remember(ctx context.Context, content string, tags map[string]string) error {
	if err := m.require(); err != nil {
		return err
	}
	if !m.c.cfg.LongTermEnabled {
		return fmt.Errorf("%w: long-term memory (MEMORY_LONGTERM_ENABLED is not true)", ErrNotWired)
	}
	body := map[string]any{"content": content}
	if len(tags) > 0 {
		body["tags"] = tags
	}
	return m.c.t.do(ctx, "POST", m.c.cfg.memoryBase()+"/memory/agent/remember", body, nil, nil)
}

// SearchAgent retrieves facts from long-term memory. minScore filters weak matches; pass 0 to
// take whatever the backend returns.
func (m *MemoryClient) SearchAgent(ctx context.Context, query string, topK int, minScore float64) ([]Fact, error) {
	if err := m.require(); err != nil {
		return nil, err
	}
	if !m.c.cfg.LongTermEnabled {
		return nil, fmt.Errorf("%w: long-term memory (MEMORY_LONGTERM_ENABLED is not true)", ErrNotWired)
	}
	if topK <= 0 {
		topK = 5
	}
	body := map[string]any{"query": query, "topK": topK}
	var out struct {
		Results []Fact `json:"results"`
	}
	u := m.c.cfg.memoryBase() + "/memory/agent/search"
	if err := m.c.t.do(ctx, "POST", u, body, &out, nil); err != nil {
		return nil, err
	}
	kept := make([]Fact, 0, len(out.Results))
	for _, f := range out.Results {
		if f.Score >= minScore {
			kept = append(kept, f)
		}
	}
	return kept, nil
}

// ── knowledge: /knowledge/search ─────────────────────────────────────────────

// KnowledgeClient searches the knowledge bases bound to this agent.
type KnowledgeClient struct{ c *Client }

// Chunk is one retrieval hit, with the provenance a citation needs.
type Chunk struct {
	Content       string  `json:"content"`
	DocumentRef   string  `json:"documentRef"`
	KnowledgeBase string  `json:"knowledgeBase"`
	Score         float64 `json:"score"`
}

// Search runs retrieval over one knowledge base. knowledgeBase is REQUIRED — the launcher answers
// 400 "knowledgeBase is required" without it, so an omit-to-search-all mode does not exist.
func (k *KnowledgeClient) Search(ctx context.Context, query, knowledgeBase string, topK int) ([]Chunk, error) {
	if !k.c.cfg.KnowledgeEnabled {
		return nil, fmt.Errorf("%w: knowledge (KNOWLEDGE_BASE_ENABLED is not true)", ErrNotWired)
	}
	if topK <= 0 {
		topK = 5
	}
	if strings.TrimSpace(knowledgeBase) == "" {
		return nil, fmt.Errorf("ctxmesh: knowledgeBase is required")
	}
	body := map[string]any{"query": query, "topK": topK, "knowledgeBase": knowledgeBase}
	var out struct {
		Results []Chunk `json:"results"`
	}
	// Per-request, NOT by writing the shared client's Timeout: that field is read concurrently by
	// every other call, so mutating it was both a process-wide side effect and a data race.
	sctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	u := k.c.cfg.memoryBase() + "/knowledge/search"
	if err := k.c.t.do(sctx, "POST", u, body, &out, nil); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// ── skills: /skills and /skills/load ─────────────────────────────────────────

// SkillsClient lists and loads the skills attached to this agent.
type SkillsClient struct{ c *Client }

// Skill is one attached skill.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Digest      string `json:"digest,omitempty"`
}

// List returns the skills the platform attached.
func (s *SkillsClient) List(ctx context.Context) ([]Skill, error) {
	var out struct {
		Skills []Skill `json:"skills"`
	}
	u := s.c.cfg.memoryBase() + "/skills"
	if err := s.c.t.do(ctx, "GET", u, nil, &out, nil); err != nil {
		return nil, err
	}
	return out.Skills, nil
}

// Load fetches a skill's body by name.
func (s *SkillsClient) Load(ctx context.Context, name string) (string, error) {
	// The launcher answers {"body": "..."} — reading "content" yielded an empty string with NO
	// error, so a skill loaded as nothing and the model carried on without it.
	var out struct {
		Body string `json:"body"`
	}
	u := s.c.cfg.memoryBase() + "/skills/load"
	if err := s.c.t.do(ctx, "POST", u, map[string]any{"name": name}, &out, nil); err != nil {
		return "", err
	}
	return out.Body, nil
}

// ── feedback: /feedback ──────────────────────────────────────────────────────

// FeedbackClient submits the signal that drives evals and canary promotion.
type FeedbackClient struct{ c *Client }

// Score records a score against a trace. dimension names what is being scored
// ("helpfulness", "accuracy"); comment is optional.
func (f *FeedbackClient) Score(ctx context.Context, traceID, dimension string, score float64, comment string) error {
	if !f.c.cfg.FeedbackWired {
		return fmt.Errorf("%w: feedback (FEEDBACK_PORT is unset)", ErrNotWired)
	}
	// name/value, not dimension/score: the handler decodes those field names and relays to
	// Langfuse. Sending the wrong keys returned 202 while writing a nameless zero score.
	body := map[string]any{"traceId": traceID, "name": dimension, "value": score}
	if comment != "" {
		body["comment"] = comment
	}
	return f.c.t.do(ctx, "POST", f.c.cfg.feedbackBase()+"/feedback", body, nil, nil)
}

// ── mesh: /amp and /a2a ──────────────────────────────────────────────────────

// MeshClient makes agent-to-agent calls.
//
// /amp is the current surface; /a2a is the retired spelling, still served so agents built
// against it keep working (ADR 0138). Call takes /amp. CallLegacy exists so this SDK covers
// the route the launcher still serves rather than pretending it is gone.
type MeshClient struct{ c *Client }

// Call invokes another agent through AMP and returns its raw reply.
func (m *MeshClient) Call(ctx context.Context, targetAgent string, payload any) (map[string]any, error) {
	if strings.TrimSpace(targetAgent) == "" {
		return nil, fmt.Errorf("ctxmesh: target agent is required")
	}
	var out map[string]any
	u := m.c.cfg.ampBase() + "/amp/" + url.PathEscape(targetAgent)
	if err := m.c.t.do(ctx, "POST", u, payload, &out, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// CallLegacy invokes another agent through the retired /a2a path. Prefer Call.
func (m *MeshClient) CallLegacy(ctx context.Context, targetAgent string, payload any) (map[string]any, error) {
	if strings.TrimSpace(targetAgent) == "" {
		return nil, fmt.Errorf("ctxmesh: target agent is required")
	}
	var out map[string]any
	u := m.c.cfg.ampBase() + "/a2a/" + url.PathEscape(targetAgent)
	if err := m.c.t.do(ctx, "POST", u, payload, &out, nil); err != nil {
		return nil, err
	}
	return out, nil
}

// ── runs: /delegate and /handoff ─────────────────────────────────────────────

// RunsClient spawns sub-runs and hands conversations off.
type RunsClient struct{ c *Client }

// Delegation is what /delegate answers. The launcher returns HTTP 200 for every outcome and
// signals success in OK, so a refusal decoded as a transport success is silent data loss —
// Answer is the entire point of delegating.
type Delegation struct {
	OK       bool   `json:"ok"`
	SubAgent string `json:"subAgent,omitempty"`
	SubRun   string `json:"subRun,omitempty"`
	Answer   string `json:"answer,omitempty"`
	Error    string `json:"error,omitempty"`
	// Suspend and Endpoint are the L7 suspend signal: the launcher resolved the target and
	// budget-checked it but did NOT spawn. The caller suspends once and the BFF creates the child.
	Suspend  bool   `json:"suspend,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

// Handoff is what /handoff answers, with the same OK-not-status convention.
type Handoff struct {
	OK          bool   `json:"ok"`
	RunID       string `json:"runId,omitempty"`
	SourceRun   string `json:"sourceRun,omitempty"`
	HandedOffTo string `json:"handedOffTo,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Delegate spawns a sub-run on another agent.
//
// step and callID are the idempotency key the launcher hard-requires: step is the supervisor's
// loop iteration and callID the model's tool-call id, so a reclaimed supervisor resolves to the
// SAME sub-run rather than spawning a second one. capability is the run capability
// (X-Ctxmesh-Run-Capability); delegation is refused without an authenticated run.
//
// The launcher answers 200 for every outcome, so check Delegation.OK — a refusal is not an error.
func (r *RunsClient) Delegate(ctx context.Context, subAgent, step, callID, capability string, input any) (*Delegation, error) {
	if strings.TrimSpace(subAgent) == "" {
		return nil, fmt.Errorf("ctxmesh: sub-agent is required")
	}
	if strings.TrimSpace(step) == "" || strings.TrimSpace(callID) == "" {
		return nil, fmt.Errorf("ctxmesh: step and callId are required (they are the idempotency key)")
	}
	if strings.TrimSpace(capability) == "" {
		return nil, fmt.Errorf("%w: delegation needs the run capability (%s)", ErrNotWired, CapabilityHeader)
	}
	body := map[string]any{"subAgent": subAgent, "input": input, "step": step, "callId": callID}
	var out Delegation
	u := r.c.cfg.delegateBase() + "/delegate"
	if err := r.c.t.do(ctx, "POST", u, body, &out, map[string]string{CapabilityHeader: capability}); err != nil {
		return nil, err
	}
	return &out, nil
}

// Handoff transfers the conversation to another agent.
//
// includeHistory carries the transcript across; the launcher treats an ABSENT field as true, so
// this sends it explicitly. message is B's opening note — without history and without a message
// the receiver is handed nothing.
//
// Like Delegate, the launcher answers 200 for every outcome: check Handoff.OK.
func (r *RunsClient) Handoff(ctx context.Context, targetAgent, capability, message string, includeHistory bool) (*Handoff, error) {
	if strings.TrimSpace(targetAgent) == "" {
		return nil, fmt.Errorf("ctxmesh: target agent is required")
	}
	if strings.TrimSpace(capability) == "" {
		return nil, fmt.Errorf("%w: handoff needs the run capability (%s)", ErrNotWired, CapabilityHeader)
	}
	body := map[string]any{"targetAgent": targetAgent, "includeHistory": includeHistory}
	if message != "" {
		body["message"] = message
	}
	var out Handoff
	u := r.c.cfg.delegateBase() + "/handoff"
	if err := r.c.t.do(ctx, "POST", u, body, &out, map[string]string{CapabilityHeader: capability}); err != nil {
		return nil, err
	}
	return &out, nil
}
