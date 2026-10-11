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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/ctxmesh/internal/run"
)

// runEventsGolden is the run event stream's wire bytes as the handler writes them. The console and
// every SDK that reads the stream parse this same file in their own tests, so a change to the frame
// shape fails here first and then in each client that would misread it. Regenerate it with
//
//	UPDATE_FIXTURES=1 go test ./internal/bff -run TestRunEventsWire_MatchesGolden
var runEventsGolden = filepath.Join("..", "..", "sdk", "run-events.golden.sse")

// Step metadata as the agent loop streams it (ADR 0071 §4).
const (
	goldenStepModel1 = `{"type":"step","step":1,"kind":"model","tokens":{"prompt":11,"completion":7},"ref":null}`
	goldenStepTool   = `{"type":"step","step":1,"kind":"tool","tool":"echo_tool","tokens":{"prompt":0,"completion":0},"ref":null}`
	goldenStepModel2 = `{"type":"step","step":2,"kind":"model","tokens":{"prompt":40,"completion":12},"ref":null}`
)

// seedGoldenRun stores a run that pauses for consent mid-run, is resumed, and completes: the shape of
// the recorded m14 run, with a token whose text spans lines and one that starts with a space.
func seedGoldenRun(t *testing.T, s *Server) string {
	t.Helper()
	const id = "run-golden"
	now := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	require.NoError(t, s.runStore.Create(run.New(id, "prod", "echo", json.RawMessage(`{"input":"ping"}`), "", now)))
	update := func(fn func(*run.Run) error) {
		t.Helper()
		_, err := s.runStore.Update(id, fn)
		require.NoError(t, err)
	}
	appendEv := func(kind run.EventKind, data string) {
		t.Helper()
		require.NoError(t, s.runStore.AppendEvent(id, kind, data))
	}
	update(func(r *run.Run) error { return r.Transition(run.StatusRunning, now) })
	appendEv(run.EventStep, goldenStepModel1)
	appendEv(run.EventToken, "Calling echo_tool.\n")
	appendEv(run.EventStep, goldenStepTool)
	update(func(r *run.Run) error {
		r.RequiresAction = &run.Action{Kind: run.ActionConsentRequired, Servers: []string{"echo-mcp"}}
		return r.Transition(run.StatusRequiresAction, now)
	})
	update(func(r *run.Run) error {
		r.RequiresAction = nil
		return r.Transition(run.StatusRunning, now)
	})
	appendEv(run.EventStep, goldenStepModel2)
	appendEv(run.EventToken, "MOCK_TOOL_OK echo_tool returned:\nFIXTURE_ECHO: ping\n")
	appendEv(run.EventToken, " Done.")
	appendEv(run.EventMessage, "Calling echo_tool.\nMOCK_TOOL_OK echo_tool returned:\nFIXTURE_ECHO: ping\n Done.")
	update(func(r *run.Run) error { return r.Transition(run.StatusSucceeded, now) })
	return id
}

func readRunEvents(t *testing.T, s *Server, id, lastEventID string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+id+"/events", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	return rec.Body.String()
}

// sseEvent is one dispatched event, read by the SSE grammar (the reference every client implements).
type sseEvent struct {
	id, event, data string
}

func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	var cur sseEvent
	var data []string
	hasData := false
	for line := range strings.SplitSeq(body, "\n") {
		if line == "" {
			if hasData {
				cur.data = strings.Join(data, "\n")
				out = append(out, cur)
			}
			cur, data, hasData = sseEvent{}, nil, false
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "id":
			cur.id = value
		case "event":
			cur.event = value
		case "data":
			data = append(data, value)
			hasData = true
		}
	}
	return out
}

// TestRunEventsWire_PayloadIsTheEventData proves each frame's payload is the event's Data itself,
// not a JSON envelope around it: tokens rejoin to the exact text (newlines included), a state frame
// says the status, a step frame is the step metadata.
func TestRunEventsWire_PayloadIsTheEventData(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")).Build()
	s := newInvokeServerNoWorkers(t, newFakeFactory(c), &fakeInvokeAdapter{})
	id := seedGoldenRun(t, s)

	events := parseSSE(t, readRunEvents(t, s, id, ""))
	require.Len(t, events, 11)

	var tokens strings.Builder
	var states, steps []string
	for i, ev := range events {
		seq, err := strconv.Atoi(ev.id)
		require.NoError(t, err)
		assert.Equal(t, i+1, seq, "seq is the SSE id")
		switch ev.event {
		case string(run.EventToken):
			tokens.WriteString(ev.data)
		case string(run.EventState):
			states = append(states, ev.data)
		case string(run.EventStep):
			steps = append(steps, ev.data)
		case string(run.EventMessage):
			assert.Equal(t, "Calling echo_tool.\nMOCK_TOOL_OK echo_tool returned:\nFIXTURE_ECHO: ping\n Done.", ev.data)
		default:
			t.Fatalf("unexpected event kind %q", ev.event)
		}
	}
	assert.Equal(t, "Calling echo_tool.\nMOCK_TOOL_OK echo_tool returned:\nFIXTURE_ECHO: ping\n Done.", tokens.String())
	assert.Equal(t, []string{
		string(run.StatusRunning), string(run.StatusRequiresAction), string(run.StatusRunning), string(run.StatusSucceeded),
	}, states)
	assert.Equal(t, []string{goldenStepModel1, goldenStepTool, goldenStepModel2}, steps)
}

// TestRunEventsWire_MatchesGolden pins the handler's bytes to the committed fixture the clients'
// tests parse, and proves a Last-Event-ID resume replays exactly the frames after the cursor.
func TestRunEventsWire_MatchesGolden(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithObjects(readyAgent("echo", "prod", "http://echo.prod.svc.cluster.local")).Build()
	s := newInvokeServerNoWorkers(t, newFakeFactory(c), &fakeInvokeAdapter{})
	id := seedGoldenRun(t, s)

	body := readRunEvents(t, s, id, "")
	if os.Getenv("UPDATE_FIXTURES") == "1" {
		require.NoError(t, os.WriteFile(runEventsGolden, []byte(body), 0o644))
	}
	want, err := os.ReadFile(runEventsGolden)
	require.NoError(t, err, "the committed fixture is missing; regenerate it with UPDATE_FIXTURES=1")
	require.Equal(t, string(want), body,
		"the run event stream's bytes changed; every client parses %s, so regenerate it (UPDATE_FIXTURES=1) and run their tests", runEventsGolden)

	// The resume cursor: everything after seq 5 (the requires_action state), byte for byte.
	cut := strings.Index(body, "id: 6\n")
	require.Positive(t, cut)
	assert.Equal(t, body[cut:], readRunEvents(t, s, id, "5"))
}

func TestSSEFrame(t *testing.T) {
	tok := string(run.EventToken)
	cases := []struct {
		name, id, event, data, want string
	}{
		{"single line", "3", "state", "succeeded", "id: 3\nevent: state\ndata: succeeded\n\n"},
		{"empty payload still dispatches", "4", tok, "", "id: 4\nevent: token\ndata:\n\n"},
		{"one data line per payload line", "5", tok, "a\nb\n", "id: 5\nevent: token\ndata: a\ndata: b\ndata:\n\n"},
		{"leading space survives the one stripped by the reader", "6", tok, " x", "id: 6\nevent: token\ndata:  x\n\n"},
		{"CR and CRLF end a line", "7", tok, "a\r\nb\rc", "id: 7\nevent: token\ndata: a\ndata: b\ndata: c\n\n"},
		{"no id", "", "log", "line", "event: log\ndata: line\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sseFrame(tc.id, tc.event, tc.data)
			assert.Equal(t, tc.want, got)
			if tc.id != "" {
				evs := parseSSE(t, got)
				require.Len(t, evs, 1)
				want := strings.ReplaceAll(strings.ReplaceAll(tc.data, "\r\n", "\n"), "\r", "\n")
				assert.Equal(t, want, evs[0].data, "the reader gets the payload back")
			}
		})
	}
}

// The descendant-pause goldens: a supervisor delegates, its sub-agent pauses for consent, the person
// connects and resumes the sub-run, and the supervisor finishes. runEventsDescendantGolden is the
// supervisor's stream; runDescendantPausedGolden is GET /api/runs/{sub} while the sub-run waits (its
// timestamps fixed, since they are wall-clock). Regenerate both with
//
//	UPDATE_FIXTURES=1 go test ./internal/bff -run TestRunEventsWire_DescendantPauseMatchesGolden
var (
	runEventsDescendantGolden = filepath.Join("..", "..", "sdk", "run-events-descendant.golden.sse")
	runDescendantPausedGolden = filepath.Join("..", "..", "sdk", "run-descendant-paused.golden.json")
)

const (
	goldenSupervisorEndpoint = "http://supervisor.prod.svc.cluster.local"
	goldenResearcherEndpoint = "http://researcher.prod.svc.cluster.local"
	goldenSupervisorAnswer   = "The researcher found 3 sources."
)

// scriptedInvoke answers each endpoint's calls in order, streaming a reply's tokens when it has any.
type scriptedInvoke struct {
	mu      sync.Mutex
	replies map[string][]scriptedReply
}

type scriptedReply struct {
	tokens []string
	body   string
}

func (s *scriptedInvoke) next(endpoint string) (scriptedReply, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.replies[endpoint]
	if len(q) == 0 {
		return scriptedReply{}, fmt.Errorf("no scripted reply left for %s", endpoint)
	}
	s.replies[endpoint] = q[1:]
	return q[0], nil
}

func (s *scriptedInvoke) Invoke(_ context.Context, endpoint string, _ []byte) ([]byte, string, error) {
	r, err := s.next(endpoint)
	return []byte(r.body), "trace-supervisor", err
}

func (s *scriptedInvoke) InvokeStream(_ context.Context, endpoint string, _ []byte, onToken func(string), _ func(string)) ([]byte, string, error) {
	r, err := s.next(endpoint)
	for _, tok := range r.tokens {
		onToken(tok)
	}
	return []byte(r.body), "trace-supervisor", err
}

// runDescendantPause drives the descendant-pause scenario through the real run paths (executeRun, the
// delegate suspend, the resume handler, the wake) and returns the supervisor's and sub-run's ids and
// the sub-run's detail as GET /api/runs/{sub} served it while it waited.
func runDescendantPause(t *testing.T) (s *Server, rootID, subID string, paused []byte) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		readyAgent("supervisor", "prod", goldenSupervisorEndpoint),
		readyAgent("researcher", "prod", goldenResearcherEndpoint),
	).Build()
	inv := &scriptedInvoke{replies: map[string][]scriptedReply{
		goldenSupervisorEndpoint: {
			{body: `{"delegate_waiting":{"checkpoint":"cp-1","delegates":[{"sub_agent":"researcher","endpoint":"` +
				goldenResearcherEndpoint + `","input":{"input":"find sources"},"step":"1","call_id":"call-1"}]}}`},
			{tokens: []string{"The researcher ", "found 3 sources."}, body: `{"output":"` + goldenSupervisorAnswer + `"}`},
		},
		goldenResearcherEndpoint: {
			{body: `{"consent_required":["search-mcp"]}`},
			{body: `{"output":"3 sources"}`},
		},
	}}
	s = newInvokeServerNoWorkers(t, newFakeFactory(c), inv)
	ctx := context.Background()

	rootID = "run-supervisor"
	input := []byte(`{"input":"research ctxmesh"}`)
	require.NoError(t, s.runStore.Create(run.New(rootID, "prod", "supervisor", input, "", time.Now())))
	s.executeRun(ctx, rootID, goldenSupervisorEndpoint, input)
	root, err := s.runStore.Get(rootID)
	require.NoError(t, err)
	require.Equal(t, run.StatusWaiting, root.Status, root.Error)

	subID = run.SpawnRunID(rootID, "1", "call-1")
	s.executeRun(ctx, subID, goldenResearcherEndpoint, []byte(`{"input":"find sources"}`))
	sub, err := s.runStore.Get(subID)
	require.NoError(t, err)
	require.Equal(t, run.StatusRequiresAction, sub.Status, sub.Error)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+subID, nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var detail map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
	detail["createdAt"], detail["updatedAt"] = "2026-10-12T09:00:00Z", "2026-10-12T09:00:01Z"
	paused, err = json.MarshalIndent(detail, "", "  ")
	require.NoError(t, err)
	paused = append(paused, '\n')

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/runs/"+subID+"/resume", nil)
	req.Header.Set("Authorization", "Bearer developer-persona-token")
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Eventually(t, func() bool {
		r, gErr := s.runStore.Get(rootID)
		return gErr == nil && r.Status == run.StatusQueued
	}, 5*time.Second, 10*time.Millisecond, "the sub-run's completion wakes the supervisor")

	s.executeRun(ctx, rootID, goldenSupervisorEndpoint, input)
	root, err = s.runStore.Get(rootID)
	require.NoError(t, err)
	require.Equal(t, run.StatusSucceeded, root.Status, root.Error)
	return s, rootID, subID, paused
}

// TestRunEventsWire_DescendantPauseMatchesGolden pins the supervisor's stream and the paused sub-run's
// detail to the fixtures the console's chat test replays, and proves the supervisor's stream names the
// paused sub-run and resumes after it.
func TestRunEventsWire_DescendantPauseMatchesGolden(t *testing.T) {
	s, rootID, subID, paused := runDescendantPause(t)

	body := readRunEvents(t, s, rootID, "")
	if os.Getenv("UPDATE_FIXTURES") == "1" {
		require.NoError(t, os.WriteFile(runEventsDescendantGolden, []byte(body), 0o644))
		require.NoError(t, os.WriteFile(runDescendantPausedGolden, paused, 0o644))
	}
	want, err := os.ReadFile(runEventsDescendantGolden)
	require.NoError(t, err, "the committed fixture is missing; regenerate it with UPDATE_FIXTURES=1")
	require.Equal(t, string(want), body, "the supervisor's stream changed; regenerate %s (UPDATE_FIXTURES=1)", runEventsDescendantGolden)
	wantDetail, err := os.ReadFile(runDescendantPausedGolden)
	require.NoError(t, err, "the committed fixture is missing; regenerate it with UPDATE_FIXTURES=1")
	require.Equal(t, string(wantDetail), string(paused), "the paused sub-run's detail changed; regenerate %s", runDescendantPausedGolden)

	var crumb sseEvent
	for _, ev := range parseSSE(t, body) {
		if ev.event == string(run.EventDescendantAction) {
			crumb = ev
		}
	}
	require.Equal(t, subID, crumb.data, "the supervisor's stream names the paused sub-run")
	cut := strings.Index(body, "id: "+crumb.id+"\n")
	require.GreaterOrEqual(t, cut, 0)
	after := body[cut+strings.Index(body[cut:], "\n\n")+2:]
	assert.Equal(t, after, readRunEvents(t, s, rootID, crumb.id), "a client resuming after the pause gets the rest")
	assert.Contains(t, after, "data: "+goldenSupervisorAnswer)

	var detail RunDetailDTO
	require.NoError(t, json.Unmarshal(paused, &detail))
	require.NotNil(t, detail.RequiresAction)
	assert.Equal(t, []string{"search-mcp"}, detail.RequiresAction.Servers)
	assert.Equal(t, rootID, detail.RootRunID)
}
