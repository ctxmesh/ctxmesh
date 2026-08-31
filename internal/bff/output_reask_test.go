package bff

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/agentry/internal/run"
)

// scriptedInvokeAdapter returns a different response per call, so a test can model an agent that
// gets it wrong first and right on the re-ask — which the single-response fake cannot express.
type scriptedInvokeAdapter struct {
	mu        sync.Mutex
	responses [][]byte
	bodies    [][]byte
	calls     int
}

func (f *scriptedInvokeAdapter) Invoke(_ context.Context, _ string, body []byte) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies = append(f.bodies, body)
	i := f.calls
	f.calls++
	if i >= len(f.responses) {
		i = len(f.responses) - 1 // repeat the last scripted answer
	}
	return f.responses[i], "tr", nil
}

func (f *scriptedInvokeAdapter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *scriptedInvokeAdapter) body(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.bodies) {
		return ""
	}
	return string(f.bodies[i])
}

// THE BAR (m143.6, m52.J4): a non-SDK agent's near-miss is RECOVERED by one platform re-ask.
// Before M143 the run just died — an SDK agent repairs in-loop, but a custom-loop agent (which the
// platform explicitly supports) had no recovery tier at all.
func TestReask_ANearMissIsRecoveredByOneReask(t *testing.T) {
	agent := agentWithSchema(m65_4OutputSchema)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &scriptedInvokeAdapter{responses: [][]byte{
		[]byte(`{"output":"shipped, as prose","consent_required":[]}`),        // near-miss: not JSON
		[]byte(`{"output":"{\"answer\":\"shipped\"}","consent_required":[]}`), // repaired
	}}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "typed", Namespace: "prod", Input: json.RawMessage(`"ship it"`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })

	assert.Equal(t, run.StatusSucceeded, got.Status,
		"a near-miss the agent can repair must not kill the run")
	require.Len(t, got.Messages, 1)
	assert.Equal(t, `{"answer":"shipped"}`, got.Messages[0].Content,
		"the REPAIRED answer is the one persisted — never the rejected one")
	assert.Equal(t, 2, inv.count(), "exactly one re-ask, not a repair loop")

	// The re-ask must actually TELL the agent what was wrong; "try again" alone mostly reproduces
	// the same near-miss. And it must carry the original prompt, or it is a different question.
	second := inv.body(1)
	assert.Contains(t, second, "ship it", "the re-ask must carry the original prompt")
	assert.Contains(t, second, "answer", "the re-ask must state the required schema")
	assert.Contains(t, second, "REJECTED", "the re-ask must say the previous answer was rejected")

	body := runEventsBody(t, s, created.ID)
	assert.Contains(t, body, "output_schema_reask",
		"the repair must be visible on the run's stream, not an unexplained extra round-trip")
	assert.Contains(t, body, "Re-asking",
		"and must carry a HUMAN label — a step frame with no label renders as raw JSON in the console")
}

// The re-ask is a RECOVERY tier, not an escape from the control: an agent that is still wrong on the
// second try fails exactly as fail-closed as before, and the rejected answer is never surfaced.
func TestReask_StillNonConformingAfterTheReaskFailsHonestly(t *testing.T) {
	agent := agentWithSchema(m65_4OutputSchema)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &scriptedInvokeAdapter{responses: [][]byte{
		[]byte(`{"output":"{\"note\":\"nope\"}","consent_required":[]}`),
		[]byte(`{"output":"still wrong","consent_required":[]}`),
	}}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "typed", Namespace: "prod", Input: json.RawMessage(`{}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })

	assert.Equal(t, run.StatusFailed, got.Status, "one re-ask, then an honest failure")
	assert.Contains(t, got.Error, "schema")
	assert.Empty(t, got.Messages, "neither rejected answer may be persisted")
	assert.Equal(t, 2, inv.count(), "the re-ask is bounded at one — never a retry loop")
	assert.NotContains(t, runEventsBody(t, s, created.ID), "event: message")
}

// A conforming answer must cost exactly ONE invoke. The re-ask lives on the failure path only.
func TestReask_AHealthyRunIsNeverReasked(t *testing.T) {
	agent := agentWithSchema(m65_4OutputSchema)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &scriptedInvokeAdapter{responses: [][]byte{
		[]byte(`{"output":"{\"answer\":\"shipped\"}","consent_required":[]}`),
	}}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "typed", Namespace: "prod", Input: json.RawMessage(`{}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })

	require.Equal(t, run.StatusSucceeded, got.Status)
	assert.Equal(t, 1, inv.count(), "a conforming answer must not cost a second invoke")
}

// An UNCOMPILABLE schema is the one rejection the agent cannot possibly fix — asking it again just
// burns an invoke on an operator's broken contract. Fail closed immediately, as M65 did.
func TestReask_AnUncompilableSchemaIsNotReasked(t *testing.T) {
	agent := agentWithSchema(`{"type": 123}`)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &scriptedInvokeAdapter{responses: [][]byte{
		[]byte(`{"output":"{\"answer\":\"shipped\"}","consent_required":[]}`),
	}}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "typed", Namespace: "prod", Input: json.RawMessage(`{}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })

	assert.Equal(t, run.StatusFailed, got.Status)
	assert.Contains(t, got.Error, "not a valid JSON Schema")
	assert.Equal(t, 1, inv.count(),
		"an unenforceable schema is the operator's bug — do not spend a re-ask on it")
}

// A run with no schema never reaches the re-ask at all: the pre-M65 path stays byte-identical.
func TestReask_NoSchemaIsUntouched(t *testing.T) {
	agent := readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(agent).Build()
	inv := &scriptedInvokeAdapter{responses: [][]byte{
		[]byte(`{"output":"just prose","consent_required":[]}`),
	}}
	s := newInvokeServer(t, newFakeFactory(c), inv)

	created := createRun(t, s, InvokeRequest{Agent: "echo", Namespace: "prod", Input: json.RawMessage(`{}`)})
	got := pollRun(t, s, created.ID, func(st run.Status) bool { return st.IsTerminal() })

	require.Equal(t, run.StatusSucceeded, got.Status)
	assert.Equal(t, 1, inv.count())
}

// assertErr is a terse stand-in for a validation error in the body-rewrite tests.
func assertErr(msg string) error { return errors.New(msg) }

// ─── body-rewrite unit coverage ───────────────────────────────────────────────────────────────────

func TestReaskInvokeBody_WrapsBothInputShapes(t *testing.T) {
	verr := assertErr("missing required property 'answer'")

	t.Run("an object body keeps its other fields and extends input", func(t *testing.T) {
		out, ok := reaskInvokeBody([]byte(`{"input":"ship it","approvals":["tool:echo"]}`),
			`{"type":"object"}`, `{"note":"nope"}`, verr)
		require.True(t, ok)
		var got map[string]any
		require.NoError(t, json.Unmarshal(out, &got))
		assert.True(t, strings.HasPrefix(got["input"].(string), "ship it"),
			"the original prompt must lead — the repair turn is appended, not substituted")
		assert.Contains(t, got["input"].(string), "missing required property")
		assert.Equal(t, []any{"tool:echo"}, got["approvals"],
			"unrelated body fields (granted approvals) must survive the rewrite")
	})

	t.Run("a bare string body is wrapped", func(t *testing.T) {
		out, ok := reaskInvokeBody([]byte(`"ship it"`), `{"type":"object"}`, "prose", verr)
		require.True(t, ok)
		var got map[string]any
		require.NoError(t, json.Unmarshal(out, &got))
		assert.Contains(t, got["input"].(string), "ship it")
	})

	// A checkpoint replays the very loop that produced the rejected answer, so carrying it into the
	// repair turn would re-derive the same output and waste the one re-ask.
	t.Run("a supervisor checkpoint is dropped", func(t *testing.T) {
		out, ok := reaskInvokeBody([]byte(`{"input":"go","checkpoint":{"v":1}}`),
			`{"type":"object"}`, "prose", verr)
		require.True(t, ok)
		var got map[string]any
		require.NoError(t, json.Unmarshal(out, &got))
		_, has := got["checkpoint"]
		assert.False(t, has, "the re-ask is a fresh ask, not a replay of the loop that failed")
	})

	t.Run("a shape it cannot rewrite safely is skipped, not guessed", func(t *testing.T) {
		_, ok := reaskInvokeBody([]byte(`{"input":{"structured":true}}`), `{}`, "x", verr)
		assert.False(t, ok, "a non-string input is not a shape this re-ask understands")
		_, ok = reaskInvokeBody([]byte(`not json at all`), `{}`, "x", verr)
		assert.False(t, ok)
	})
}

// A pathological answer must not turn one re-ask into a context-blowing prompt.
func TestReaskInvokeBody_TruncatesAHugeAnswer(t *testing.T) {
	huge := strings.Repeat("x", maxReaskEcho*3)
	out, ok := reaskInvokeBody([]byte(`"go"`), `{"type":"object"}`, huge, assertErr("nope"))
	require.True(t, ok)
	assert.Less(t, len(out), maxReaskEcho*2, "the echoed answer must be bounded")
	assert.Contains(t, string(out), "truncated")
}

// The re-ask must not run once this worker has stopped being the one driving the run — re-invoking
// there is a DUPLICATE execution racing the peer that reclaimed it (the m143.2/.3 fencing rule).
func TestReask_SelfFencedWorkerDoesNotReask(t *testing.T) {
	inv := &scriptedInvokeAdapter{responses: [][]byte{[]byte(`{"output":"{}"}`)}}
	s := NewServer(Options{
		Scheme:   testScheme(t),
		Auth:     AllowAll{},
		Adapters: Adapters{Invoke: inv},
		Version:  "test",
		Log:      logr.Discard(),
		RunStore: run.NewMemStore(),
	})

	fenced := &atomic.Bool{}
	fenced.Store(true)
	ctx := contextWithSelfFence(context.Background(), fenced)
	_, ok := s.reaskForOutputSchema(ctx, "r1", "http://a", []byte(`"go"`),
		`{"type":"object"}`, "prose", assertErr("nope"))
	assert.False(t, ok, "a self-fenced worker must not re-invoke the agent")
	assert.Equal(t, 0, inv.count())

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok = s.reaskForOutputSchema(cancelled, "r1", "http://a", []byte(`"go"`),
		`{"type":"object"}`, "prose", assertErr("nope"))
	assert.False(t, ok, "a cancelled run must not re-invoke the agent")
	assert.Equal(t, 0, inv.count())
}
