package ctxmesh

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// The contract gate proves each route STRING appears in this package, which a comment would
// satisfy. These tests prove the client actually issues the right method, path and body — and
// that a refusal from the plane arrives as something a caller can branch on.

// plane spins a fake launcher and returns a Client wired to it. It records what it was asked.
type call struct {
	Method string
	Path   string
	Body   string
}

func plane(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*Client, *[]call) {
	t.Helper()
	var seen []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			_, _ = r.Body.Read(b)
		}
		seen = append(seen, call{Method: r.Method, Path: r.URL.Path, Body: strings.TrimSpace(string(b))})
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	// Every plane shares the one fake, so a single server answers memory, feedback and AMP.
	cfg := FromConfig(Config{
		MemoryPort: p, FeedbackPort: p, AMPPort: p,
		ConversationID: "conv-1", MemoryWired: true, FeedbackWired: true,
		LongTermEnabled: true, KnowledgeEnabled: true,
	})
	return NewWithConfig(cfg), &seen
}

func TestEveryRouteIsActuallyCalled(t *testing.T) {
	c, seen := plane(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/memory/agent/search"):
			_, _ = w.Write([]byte(`{"results":[{"content":"f","score":0.9}]}`))
		case strings.HasPrefix(r.URL.Path, "/knowledge/search"):
			_, _ = w.Write([]byte(`{"results":[{"content":"c","documentRef":"d","score":0.5}]}`))
		case r.URL.Path == "/skills":
			_, _ = w.Write([]byte(`{"skills":[{"name":"s"}]}`))
		case r.URL.Path == "/skills/load":
			_, _ = w.Write([]byte(`{"content":"body"}`))
		case r.URL.Path == "/delegate":
			_, _ = w.Write([]byte(`{"runId":"r1","accepted":true}`))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/memory/"):
			_, _ = w.Write([]byte(`[{"role":"user","content":"hi"}]`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	ctx := context.Background()

	if _, err := c.Memory.Get(ctx, ""); err != nil {
		t.Fatalf("memory get: %v", err)
	}
	if err := c.Memory.Append(ctx, Entry{Role: "user", Content: "hi"}, ""); err != nil {
		t.Fatalf("memory append: %v", err)
	}
	if err := c.Memory.Remember(ctx, "a fact", map[string]string{"topic": "x"}); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if _, err := c.Memory.SearchAgent(ctx, "q", 3, 0); err != nil {
		t.Fatalf("search agent: %v", err)
	}
	if _, err := c.Knowledge.Search(ctx, "q", "kb", 3); err != nil {
		t.Fatalf("knowledge: %v", err)
	}
	if _, err := c.Skills.List(ctx); err != nil {
		t.Fatalf("skills list: %v", err)
	}
	if _, err := c.Skills.Load(ctx, "s"); err != nil {
		t.Fatalf("skills load: %v", err)
	}
	if err := c.Feedback.Score(ctx, "t1", "helpfulness", 1, "clear"); err != nil {
		t.Fatalf("feedback: %v", err)
	}
	if _, err := c.Mesh.Call(ctx, "other", map[string]any{"q": 1}); err != nil {
		t.Fatalf("amp: %v", err)
	}
	if _, err := c.Mesh.CallLegacy(ctx, "other", map[string]any{"q": 1}); err != nil {
		t.Fatalf("a2a: %v", err)
	}
	if _, err := c.Runs.Delegate(ctx, "sub", map[string]any{"x": 1}); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if err := c.Runs.Handoff(ctx, "other", true); err != nil {
		t.Fatalf("handoff: %v", err)
	}

	// Every launcher route the plane-client tier must reach, asserted as a PREFIX of a path the
	// fake actually received — the gate's string check cannot tell a call from a comment.
	want := []string{"/memory/", "/memory/agent", "/memory/agent/search", "/knowledge/search",
		"/skills", "/skills/load", "/feedback", "/amp/", "/a2a/", "/delegate", "/handoff"}
	for _, w := range want {
		hit := false
		for _, c := range *seen {
			if strings.HasPrefix(c.Path, w) {
				hit = true
				break
			}
		}
		if !hit {
			t.Errorf("route %s was never actually requested (seen: %v)", w, paths(*seen))
		}
	}
}

func paths(cs []call) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Method+" "+c.Path)
	}
	return out
}

func TestDeniedSurfacesAsErrDenied(t *testing.T) {
	// A 403 is the platform refusing, not the transport failing. An agent that must behave
	// differently when the delegate fence or a budget stops it needs to branch on this.
	c, _ := plane(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"spawn depth exceeded"}`))
	})
	_, err := c.Runs.Delegate(context.Background(), "sub", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("403 must map to ErrDenied, got %v", err)
	}
	if !strings.Contains(err.Error(), "spawn depth exceeded") {
		t.Fatalf("the platform's reason must survive: %v", err)
	}
}

func TestUnwiredCapabilitiesRefuseLocally(t *testing.T) {
	// The port is absent because the platform did not grant the capability. Dialling it anyway
	// turns a configuration answer into a connection-refused, which reads like an outage.
	c := NewWithConfig(FromConfig(Config{ConversationID: "c"}))
	ctx := context.Background()
	if _, err := c.Memory.Get(ctx, ""); !errors.Is(err, ErrNotWired) {
		t.Errorf("memory: want ErrNotWired, got %v", err)
	}
	if err := c.Feedback.Score(ctx, "t", "d", 1, ""); !errors.Is(err, ErrNotWired) {
		t.Errorf("feedback: want ErrNotWired, got %v", err)
	}
	if _, err := c.Knowledge.Search(ctx, "q", "", 1); !errors.Is(err, ErrNotWired) {
		t.Errorf("knowledge: want ErrNotWired, got %v", err)
	}
	// Memory is wired but long-term is not: the distinction must survive.
	c2 := NewWithConfig(FromConfig(Config{MemoryWired: true, MemoryPort: 1, ConversationID: "c"}))
	if err := c2.Memory.Remember(ctx, "x", nil); !errors.Is(err, ErrNotWired) {
		t.Errorf("longterm: want ErrNotWired, got %v", err)
	}
}

func TestFromEnv_NotInPod(t *testing.T) {
	_, err := fromEnv(func(string) (string, bool) { return "", false })
	if !errors.Is(err, ErrNotInPod) {
		t.Fatalf("outside a pod FromEnv must say so, got %v", err)
	}
}

func TestFromEnv_UnsetPortMeansNotWired(t *testing.T) {
	// The difference the whole error vocabulary rests on: a default port is not the same as a
	// granted capability.
	env := map[string]string{"AGENT_NAME": "a"}
	cfg, err := fromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatalf("in-pod with only AGENT_NAME should resolve: %v", err)
	}
	if cfg.MemoryWired {
		t.Error("MEMORY_PORT unset must mean memory is NOT wired")
	}
	if cfg.MemoryPort != defaultMemoryPort {
		t.Errorf("port should still default, got %d", cfg.MemoryPort)
	}
}

func TestFromEnv_RejectsABadPort(t *testing.T) {
	env := map[string]string{"MEMORY_PORT": "99999"}
	if _, err := fromEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok }); err == nil {
		t.Fatal("an out-of-range port must be rejected, not silently defaulted")
	}
}

func TestMemory_RefusesAnAmbiguousConversation(t *testing.T) {
	// Writing to a shared or guessed bucket is worse than failing.
	c := NewWithConfig(FromConfig(Config{MemoryWired: true, MemoryPort: 1}))
	if _, err := c.Memory.Get(context.Background(), ""); err == nil {
		t.Fatal("no conversation id anywhere must be an error")
	}
	if err := c.Memory.Append(context.Background(), Entry{}, "has/slash"); err == nil {
		t.Fatal("a separator in a conversation id must be rejected")
	}
}

func TestSearchAgent_MinScoreFilters(t *testing.T) {
	c, _ := plane(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"content":"strong","score":0.9},{"content":"weak","score":0.1}]}`))
	})
	got, err := c.Memory.SearchAgent(context.Background(), "q", 5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "strong" {
		b, _ := json.Marshal(got)
		t.Fatalf("minScore must drop weak hits, got %s", b)
	}
}
