package ctxmesh

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// The fake is generated from sdk/launcher-routes.json, whose goldens cmd/launcher proves against the
// real handlers. It 404s anything the launcher does not serve and records every request that breaks
// a route's schema, so a client calling the wrong path, or sending the wrong field, fails here.

const testCapability = "probe-run-capability"

func clientFor(f *fakeLauncher) *Client {
	p := f.port()
	return NewWithConfig(FromConfig(Config{
		MemoryPort: p, FeedbackPort: p, AMPPort: p, DelegatePort: p,
		ConversationID: "conv-1", MemoryWired: true, FeedbackWired: true,
		LongTermEnabled: true, KnowledgeEnabled: true,
	}))
}

func TestEveryLauncherRouteIsExercised(t *testing.T) {
	fake := newFakeLauncher(t)
	c := clientFor(fake)
	ctx := context.Background()

	// Every call the plane-client tier owes. One fake serves every listener, so a route sent to the
	// wrong PORT still reaches it; TestDelegateUsesItsOwnListener covers ports.
	calls := map[string]func() error{
		"GET /memory/{conversationId}": func() error { _, err := c.Memory.Get(ctx, ""); return err },
		"POST /memory/{conversationId}/append": func() error {
			return c.Memory.Append(ctx, Entry{Role: "user", Content: "hi"}, "")
		},
		"PUT /memory/{conversationId}": func() error {
			return c.Memory.Put(ctx, []Entry{{Role: "user", Content: "hi"}}, "")
		},
		"GET /memory/{conversationId}/search": func() error {
			_, err := c.Memory.Search(ctx, "metric", "", testCapability)
			return err
		},
		"POST /memory/agent/remember": func() error {
			return c.Memory.Remember(ctx, "The user prefers metric units.", map[string]string{"topic": "preferences"})
		},
		"POST /memory/agent/search": func() error { _, err := c.Memory.SearchAgent(ctx, "units", 3, 0); return err },
		"POST /knowledge/search": func() error {
			_, err := c.Knowledge.Search(ctx, "When do deploys go out?", "handbook", 3)
			return err
		},
		"GET /skills":       func() error { _, err := c.Skills.List(ctx); return err },
		"POST /skills/load": func() error { _, err := c.Skills.Load(ctx, "deploy"); return err },
		"ANY /feedback": func() error {
			return c.Feedback.Score(ctx, "0af7651916cd43dd8448eb211c80319c", "helpfulness", 0.9, "clear answer")
		},
		"POST /amp/{targetAgent}": func() error {
			_, err := c.Mesh.Call(ctx, "peer-agent", map[string]any{"question": "ping"})
			return err
		},
		"POST /a2a/{targetAgent}": func() error {
			_, err := c.Mesh.CallLegacy(ctx, "peer-agent", map[string]any{"question": "ping"})
			return err
		},
		"POST /delegate": func() error {
			_, err := c.Runs.Delegate(ctx, "researcher", "1", "call-1", testCapability, "Summarise the quarterly report")
			return err
		},
		"POST /handoff": func() error {
			_, err := c.Runs.Handoff(ctx, "billing", testCapability, "The customer is asking about an invoice.", true)
			return err
		},
	}
	for key, call := range calls {
		if err := call(); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
	for _, key := range fake.routeKeys() {
		switch why := fake.waived(key); {
		case why != "" && fake.isCovered(key):
			t.Errorf("%s is waived for Go (%s) but the SDK reaches it now: delete the waiver", key, why)
		case why == "" && !fake.isCovered(key):
			t.Errorf("the launcher serves %s and no SDK call reached it", key)
		}
	}
	fake.assertConforms(t)
}

func TestResponsesDecodeAsTheLauncherSendsThem(t *testing.T) {
	fake := newFakeLauncher(t)
	c := clientFor(fake)
	ctx := context.Background()

	entries, err := c.Memory.Get(ctx, "conv-1")
	if err != nil || len(entries) != 2 || entries[1].Content != "You prefer metric units." {
		t.Errorf("memory.Get = %+v, %v", entries, err)
	}
	facts, err := c.Memory.SearchAgent(ctx, "units", 3, 0)
	if err != nil || len(facts) != 1 || facts[0].Content != "The user prefers metric units." || facts[0].Score != 0.91 {
		t.Errorf("searchAgent = %+v, %v", facts, err)
	}
	chunks, err := c.Knowledge.Search(ctx, "When do deploys go out?", "handbook", 3)
	if err != nil || len(chunks) != 1 || chunks[0].DocumentRef != "handbook/deploys.md" {
		t.Errorf("knowledge = %+v, %v", chunks, err)
	}
	skills, err := c.Skills.List(ctx)
	if err != nil || len(skills) != 1 || skills[0].Name != "deploy" || skills[0].Digest == "" {
		t.Errorf("skills = %+v, %v", skills, err)
	}
	reply, err := c.Mesh.Call(ctx, "peer-agent", map[string]any{"question": "ping"})
	if err != nil || reply["answer"] != "pong" {
		t.Errorf("mesh.Call = %v, %v", reply, err)
	}
	h, err := c.Runs.Handoff(ctx, "billing", testCapability, "over to you", true)
	if err != nil || !h.OK || h.RunID != "run-billing-1" || h.HandedOffTo != "billing" {
		t.Errorf("handoff = %+v, %v", h, err)
	}
	fake.assertConforms(t)
}

func TestSkillsLoadReturnsTheBody(t *testing.T) {
	// The launcher answers {"body": …}. Reading "content" returned "" with NO error, so a skill
	// loaded as nothing and the model carried on without it.
	fake := newFakeLauncher(t)
	var want struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(fake.golden("POST /skills/load", "").Response.Body, &want); err != nil {
		t.Fatal(err)
	}
	got, err := clientFor(fake).Skills.Load(context.Background(), "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if got == "" || got != want.Body {
		t.Fatalf("skills.Load = %q, the launcher sends %q", got, want.Body)
	}
}

func TestRefusalsSurfaceTheLauncherStatus(t *testing.T) {
	fake := newFakeLauncher(t)
	fake.useGolden("POST /knowledge/search", "refuses-a-knowledge-base-not-granted")
	fake.useGolden("POST /skills/load", "refuses-a-skill-not-attached")
	c := clientFor(fake)
	ctx := context.Background()

	var apiErr *APIError
	if _, err := c.Knowledge.Search(ctx, "salaries", "payroll", 3); !errors.As(err, &apiErr) || apiErr.Status != 403 {
		t.Errorf("an ungranted knowledge base must surface the launcher's 403, got %v", err)
	}
	if _, err := c.Skills.Load(ctx, "payroll"); !errors.As(err, &apiErr) || apiErr.Status != 403 {
		t.Errorf("an unattached skill must surface the launcher's 403, got %v", err)
	}
}

func TestDelegateRefusesWithoutItsRequiredFields(t *testing.T) {
	// step, callId and the capability are hard-required by the handler. Refusing locally turns a
	// confusing remote refusal into a clear one.
	c := NewWithConfig(FromConfig(Config{MemoryPort: 1, DelegatePort: 1, ConversationID: "c", MemoryWired: true}))
	ctx := context.Background()
	if _, err := c.Runs.Delegate(ctx, "sub", "", "call", "cap", nil); err == nil {
		t.Error("missing step must be refused")
	}
	if _, err := c.Runs.Delegate(ctx, "sub", "step", "", "cap", nil); err == nil {
		t.Error("missing callId must be refused")
	}
	if _, err := c.Runs.Delegate(ctx, "sub", "step", "call", "", nil); err == nil {
		t.Error("missing run capability must be refused")
	}
	if _, err := c.Runs.Handoff(ctx, "other", "", "", true); err == nil {
		t.Error("handoff without a capability must be refused")
	}
}

func TestDelegateUsesItsOwnListener(t *testing.T) {
	// /delegate and /handoff are served by a DIFFERENT listener. Sending them to the memory port was
	// a 404 in all four plane-client SDKs.
	memory, delegate := newFakeLauncher(t), newFakeLauncher(t)
	c := NewWithConfig(FromConfig(Config{
		MemoryPort: memory.port(), DelegatePort: delegate.port(), ConversationID: "c", MemoryWired: true,
	}))
	if _, err := c.Runs.Delegate(context.Background(), "researcher", "1", "call-1", testCapability, "x"); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if !delegate.isCovered("POST /delegate") {
		t.Error("/delegate did not reach the delegate listener")
	}
	if memory.isCovered("POST /delegate") {
		t.Error("/delegate reached the MEMORY listener")
	}
}

func TestDelegateDecodesSuccessAndRefusal(t *testing.T) {
	// The launcher answers 200 for every outcome and signals success in "ok", so a refusal decoded as
	// a transport success would be silent data loss, and "answer" is the point of delegating.
	fake := newFakeLauncher(t)
	c := clientFor(fake)
	ctx := context.Background()

	d, err := c.Runs.Delegate(ctx, "researcher", "1", "call-1", testCapability, "Summarise the quarterly report")
	if err != nil {
		t.Fatal(err)
	}
	if !d.OK || d.Answer != "The quarterly report is ready." || d.SubRun != "sub-run-1" {
		t.Errorf("success decoded as %+v", d)
	}
	fake.useGolden("POST /delegate", "refuses-without-a-run-capability")
	d, err = c.Runs.Delegate(ctx, "researcher", "1", "call-1", testCapability, "Summarise the quarterly report")
	if err != nil {
		t.Fatal(err)
	}
	if d.OK || d.Error == "" {
		t.Errorf("a refusal must decode as ok=false with the reason, got %+v", d)
	}
	fake.assertConforms(t)
}

func TestTheFakeRefusesWhatTheLauncherRefuses(t *testing.T) {
	// The fake is only worth generating if it catches the defects hand-written fakes let through.
	fake := newFakeLauncher(t)
	c := clientFor(fake)
	ctx := context.Background()
	_ = c.t.do(ctx, "POST", c.cfg.memoryBase()+"/memory/agent/remember", map[string]any{"text": "a fact"}, nil, nil)
	if len(fake.violations()) == 0 {
		t.Error("a remember body without content passed the fake")
	}
	err := c.t.do(ctx, "POST", c.cfg.memoryBase()+"/memory/agent/forget", map[string]any{}, nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Errorf("a path the launcher does not serve must 404, got %v", err)
	}
	err = c.t.do(ctx, "GET", c.cfg.memoryBase()+"/skills/load", nil, nil, nil)
	if !errors.As(err, &apiErr) || apiErr.Status != 405 {
		t.Errorf("a served path under the wrong method must 405, got %v", err)
	}
}
