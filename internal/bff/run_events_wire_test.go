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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
