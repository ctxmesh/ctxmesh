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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// resolverScheme builds a scheme knowing AgentDeployment + EvalSuite so the fake client can read them.
func resolverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	return scheme
}

// The resolver tests exercise one fixed (namespace, agent, suite) — the resolver reads (namespace,
// agentName) → evalSuiteRef → suite, all in the same namespace. What VARIES across tests is the
// evalSuiteRef + the online block, not the identities, so those are constants.
const (
	resolverNs    = "default"
	resolverAgent = "foo"
	resolverSuite = "suite-a"
)

// mkAgent builds an AgentDeployment (resolverNs/resolverAgent) with the given evalSuiteRef.
func mkAgent(evalSuiteRef string) *agentsv1alpha1.AgentDeployment {
	a := &agentsv1alpha1.AgentDeployment{ObjectMeta: metav1.ObjectMeta{Namespace: resolverNs, Name: resolverAgent}}
	a.Spec.EvalSuiteRef = evalSuiteRef
	return a
}

// mkSuite builds an EvalSuite (resolverNs/resolverSuite) with the given optional online block.
func mkSuite(online *agentsv1alpha1.OnlineScoringSpec) *agentsv1alpha1.EvalSuite {
	s := &agentsv1alpha1.EvalSuite{ObjectMeta: metav1.ObjectMeta{Namespace: resolverNs, Name: resolverSuite}}
	s.Spec.Dataset = agentsv1alpha1.DatasetRef{Ref: "ds"}
	s.Spec.Scorers = []agentsv1alpha1.ScorerSpec{{Name: "m", Type: "mock", Weight: 1}}
	s.Spec.Threshold = "0.8"
	s.Spec.Online = online
	return s
}

func newResolver(t *testing.T, objs ...client.Object) OnlineConfigResolver {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(resolverScheme(t)).WithObjects(objs...).Build()
	return NewK8sOnlineConfigResolver(c)
}

// Test: an AgentDeployment → EvalSuite(online) resolves to the parsed config.
func TestResolveOnline_ParsedConfig(t *testing.T) {
	t.Parallel()

	online := &agentsv1alpha1.OnlineScoringSpec{
		SampleRate:      "0.05",
		MaxScoredPerDay: 25,
		Window:          "24h",
		MinSamples:      10,
	}
	r := newResolver(t,
		mkAgent("suite-a"),
		mkSuite(online),
	)

	got, err := r.ResolveOnline(context.Background(), "default", "foo")
	require.NoError(t, err)
	require.NotNil(t, got, "an agent with an online-block EvalSuite resolves to a config")
	assert.InDelta(t, 0.05, got.SampleRate, 1e-9)
	assert.Equal(t, 25, got.MaxScoredPerDay)
	assert.Equal(t, 24*time.Hour, got.Window)
	assert.Equal(t, 10, got.MinSamples)
}

// Test: an agent with NO evalSuiteRef ⇒ (nil, nil) — no policy, worker uses defaults.
func TestResolveOnline_NoEvalSuiteRef(t *testing.T) {
	t.Parallel()

	r := newResolver(t, mkAgent(""))
	got, err := r.ResolveOnline(context.Background(), "default", "foo")
	require.NoError(t, err)
	assert.Nil(t, got, "no evalSuiteRef ⇒ (nil, nil): process defaults")
}

// Test: an EvalSuite with NO online block ⇒ (nil, nil).
func TestResolveOnline_NoOnlineBlock(t *testing.T) {
	t.Parallel()

	r := newResolver(t,
		mkAgent("suite-a"),
		mkSuite(nil), // no online block
	)
	got, err := r.ResolveOnline(context.Background(), "default", "foo")
	require.NoError(t, err)
	assert.Nil(t, got, "EvalSuite without an online block ⇒ (nil, nil): process defaults")
}

// Test: a dangling evalSuiteRef (suite not found) ⇒ (nil, nil), NOT an error — degrade to defaults.
func TestResolveOnline_DanglingSuiteRef(t *testing.T) {
	t.Parallel()

	r := newResolver(t, mkAgent("missing-suite"))
	got, err := r.ResolveOnline(context.Background(), "default", "foo")
	require.NoError(t, err, "a dangling evalSuiteRef degrades to defaults, not a hard error")
	assert.Nil(t, got)
}

// Test: the agent itself not found ⇒ (nil, nil) — a trace can outlive its AgentDeployment.
func TestResolveOnline_AgentNotFound(t *testing.T) {
	t.Parallel()

	r := newResolver(t) // no objects
	got, err := r.ResolveOnline(context.Background(), "default", "ghost")
	require.NoError(t, err, "an absent agent ⇒ (nil, nil), not an error")
	assert.Nil(t, got)
}

// Test: bad sampleRate / window strings parse to zero (not an error) — the worker then applies its floors.
func TestResolveOnline_BadStringsParseToZero(t *testing.T) {
	t.Parallel()

	online := &agentsv1alpha1.OnlineScoringSpec{
		SampleRate:      "not-a-number",
		MaxScoredPerDay: 5,
		Window:          "banana",
		MinSamples:      3,
	}
	r := newResolver(t,
		mkAgent("suite-a"),
		mkSuite(online),
	)

	got, err := r.ResolveOnline(context.Background(), "default", "foo")
	require.NoError(t, err, "malformed strings degrade to zero, never an error")
	require.NotNil(t, got)
	assert.Zero(t, got.SampleRate, "unparseable sampleRate ⇒ 0 (judge OFF)")
	assert.Equal(t, time.Duration(0), got.Window, "unparseable window ⇒ 0 (worker applies its default)")
	assert.Equal(t, 5, got.MaxScoredPerDay, "the direct int fields are unaffected")
	assert.Equal(t, 3, got.MinSamples)
}

// Test: parseSampleRate / parseWindow unit contracts (empty + valid + invalid).
func TestParseHelpers(t *testing.T) {
	t.Parallel()

	assert.Zero(t, parseSampleRate(""), "empty ⇒ 0")
	assert.InDelta(t, 0.5, parseSampleRate("0.5"), 1e-9)
	assert.Zero(t, parseSampleRate("xyz"), "malformed ⇒ 0")

	assert.Equal(t, time.Duration(0), parseWindow(""), "empty ⇒ 0")
	assert.Equal(t, time.Hour, parseWindow("1h"))
	assert.Equal(t, time.Duration(0), parseWindow("nope"), "malformed ⇒ 0")
	assert.Equal(t, time.Duration(0), parseWindow("-5m"), "non-positive ⇒ 0")
}
