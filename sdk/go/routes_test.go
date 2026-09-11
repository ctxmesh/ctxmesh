package ctxmesh

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

// Coverage is asserted against sdk/launcher-routes.json — generated from cmd/launcher — and the
// fake 404s anything unregistered. The gate can no longer assert this itself: it used to grep the
// SDK source for route substrings, which a COMMENT satisfied, so an SDK replaced by six lines of
// comments passed.

func TestEveryLauncherRouteIsExercised(t *testing.T) {
	routes, err := loadRoutes()
	if err != nil {
		t.Fatalf("load launcher-routes.json: %v — the fixture is the contract, a missing one is a failure", err)
	}
	if len(routes) == 0 {
		t.Fatal("the fixture is empty; this test would assert nothing")
	}

	fake := newFakePlane(routes)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	p, _ := strconv.Atoi(u.Port())

	// One fake serves every listener, so a route sent to the wrong PORT still reaches it. Port
	// correctness is covered by TestDelegateUsesItsOwnListener below.
	c := NewWithConfig(FromConfig(Config{
		MemoryPort: p, FeedbackPort: p, AMPPort: p, DelegatePort: p,
		ConversationID: "conv-1", MemoryWired: true, FeedbackWired: true,
		LongTermEnabled: true, KnowledgeEnabled: true,
	}))
	ctx := context.Background()
	const cap = "cap-token"

	// Every call the plane-client tier owes. A failure here is a real wire-contract break.
	if _, err := c.Memory.Get(ctx, ""); err != nil {
		t.Errorf("memory.Get: %v", err)
	}
	if err := c.Memory.Append(ctx, Entry{Role: "user", Content: "hi"}, ""); err != nil {
		t.Errorf("memory.Append: %v", err)
	}
	if err := c.Memory.Put(ctx, []Entry{{Role: "user", Content: "hi"}}, ""); err != nil {
		t.Errorf("memory.Put: %v", err)
	}
	if _, err := c.Memory.Search(ctx, "hi", "", cap); err != nil {
		t.Errorf("memory.Search: %v", err)
	}
	if err := c.Memory.Remember(ctx, "a fact", nil); err != nil {
		t.Errorf("remember: %v", err)
	}
	if _, err := c.Memory.SearchAgent(ctx, "q", 3, 0); err != nil {
		t.Errorf("searchAgent: %v", err)
	}
	if _, err := c.Knowledge.Search(ctx, "q", "kb", 3); err != nil {
		t.Errorf("knowledge: %v", err)
	}
	if _, err := c.Skills.List(ctx); err != nil {
		t.Errorf("skills.List: %v", err)
	}
	if _, err := c.Skills.Load(ctx, "s"); err != nil {
		t.Errorf("skills.Load: %v", err)
	}
	if err := c.Feedback.Score(ctx, "t1", "helpfulness", 1, "clear"); err != nil {
		t.Errorf("feedback: %v", err)
	}
	if _, err := c.Mesh.Call(ctx, "other", map[string]any{"q": 1}); err != nil {
		t.Errorf("amp: %v", err)
	}
	if _, err := c.Mesh.CallLegacy(ctx, "other", map[string]any{"q": 1}); err != nil {
		t.Errorf("a2a: %v", err)
	}
	if _, err := c.Runs.Delegate(ctx, "sub", "step-1", "call-1", cap, map[string]any{"x": 1}); err != nil {
		t.Errorf("delegate: %v", err)
	}
	if _, err := c.Runs.Handoff(ctx, "other", cap, "over to you", true); err != nil {
		t.Errorf("handoff: %v", err)
	}

	for _, r := range routes {
		if !fake.covered(r.Path, r.Method) {
			t.Errorf("launcher serves %s %s and no SDK call reached it (requests: %v)",
				r.Method, r.Path, fake.requests())
		}
	}
}

func TestSkillsLoadReturnsTheBody(t *testing.T) {
	// The launcher answers {"body": …}. Reading "content" returned "" with NO error, so a skill
	// loaded as nothing and the model carried on without it — the worst failure mode available.
	routes, err := loadRoutes()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newFakePlane(routes))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	p, _ := strconv.Atoi(u.Port())
	c := NewWithConfig(FromConfig(Config{MemoryPort: p, ConversationID: "c", MemoryWired: true}))

	got, err := c.Skills.Load(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if got == "" {
		t.Fatal("skills.Load returned an empty body with no error")
	}
	if got != "the skill body" {
		t.Fatalf("got %q", got)
	}
}

func TestDelegateRefusesWithoutItsRequiredFields(t *testing.T) {
	// step, callId and the capability are hard-required by the handler. Sending a request without
	// them is refused server-side, so refusing locally turns a confusing remote error into a clear
	// one — and makes it impossible to ship an SDK that cannot delegate at all.
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
	// /delegate and /handoff are served by a DIFFERENT listener. Sending them to the memory port
	// was a 404 in all four SDKs, and the old gate could not see it because it had no port model.
	routes, err := loadRoutes()
	if err != nil {
		t.Fatal(err)
	}
	memory := newFakePlane(routes)
	delegate := newFakePlane(routes)
	ms := httptest.NewServer(memory)
	ds := httptest.NewServer(delegate)
	t.Cleanup(ms.Close)
	t.Cleanup(ds.Close)
	mu, _ := url.Parse(ms.URL)
	du, _ := url.Parse(ds.URL)
	mp, _ := strconv.Atoi(mu.Port())
	dp, _ := strconv.Atoi(du.Port())

	c := NewWithConfig(FromConfig(Config{
		MemoryPort: mp, DelegatePort: dp, ConversationID: "c", MemoryWired: true,
	}))
	if _, err := c.Runs.Delegate(context.Background(), "sub", "s", "c", "cap", nil); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if !delegate.covered("/delegate", "POST") {
		t.Error("/delegate did not reach the delegate listener")
	}
	if memory.covered("/delegate", "POST") {
		t.Error("/delegate reached the MEMORY listener — the 404 bug is back")
	}
}

func TestDelegateRefusalIsNotSuccess(t *testing.T) {
	// The launcher answers HTTP 200 for every outcome and signals success in "ok". Decoding
	// {runId, accepted} made a refusal, a failure and a success indistinguishable — and dropped
	// "answer", which is the entire point of delegating.
	routes, err := loadRoutes()
	if err != nil {
		t.Fatal(err)
	}
	fake := newFakePlane(routes)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	p, _ := strconv.Atoi(u.Port())
	c := NewWithConfig(FromConfig(Config{MemoryPort: p, DelegatePort: p, ConversationID: "c", MemoryWired: true}))

	d, err := c.Runs.Delegate(context.Background(), "sub", "s", "c", "cap", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !d.OK {
		t.Error("ok must be decoded")
	}
	if d.Answer != "42" {
		t.Errorf("answer must survive; got %q", d.Answer)
	}
}
