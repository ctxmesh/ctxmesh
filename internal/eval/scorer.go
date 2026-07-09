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

// Package eval scores an AgentDeployment's candidate revision against an
// EvalSuite and decides the deploy gate (M9, specs/eval-prompts-feedback.md §1).
//
// The gate is scorer-agnostic: the controller depends only on the Scorer
// interface, so the deploy-gate mechanism is identical whether a deterministic
// mock scorer backs it (dev / envtest / e2e — no Langfuse, no judge model) or a
// real Langfuse-backed evaluator does (production). This is the same mock⇄real
// seam the M9 prompt Resolver and the mock LLM provider use.
//
// v1 ships the deterministic MockScorer fully. The llm-judge / code scorers
// delegate to Langfuse's managed evaluators; they are documented-future impls of
// the same interface (like the prompt package's go-git resolver) and are NOT
// built in v1, which keeps the gate green OFFLINE and reproducible in CI
// (ADR 0004, mock-first). ScorerFor returns ErrScorerUnavailable for those types
// so the gate fails CLOSED under gate:block rather than silently promoting.
package eval

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// Scorer types (mirror the CRD enum on EvalSuite.spec.scorers[].type).
const (
	// ScorerTypeMock is the platform's deterministic scorer: a reproducible score
	// derived from the dataset + candidate, with NO Langfuse or judge-model
	// dependency. This is what CI/tier0-1 and the gate e2e exercise.
	ScorerTypeMock = "mock"
	// ScorerTypeLLMJudge delegates to Langfuse's LLM-as-judge evaluator (requires
	// Langfuse up). Documented-future in v1 (see package doc).
	ScorerTypeLLMJudge = "llm-judge"
	// ScorerTypeCode delegates to Langfuse's code evaluator (requires Langfuse up).
	// Documented-future in v1 (see package doc).
	ScorerTypeCode = "code"
)

// ErrScorerUnavailable is returned by ScorerFor for a scorer type whose backing
// (a live Langfuse) is not wired in v1 (llm-judge / code). The controller treats
// it as "cannot score": gate:block holds the rollout at blocked with a clear
// reason (fail-closed, spec §"Langfuse down"); gate:warn promotes with the
// eval.unscored annotation. The mock scorer never returns it.
var ErrScorerUnavailable = errors.New("eval: scorer type not available offline (requires Langfuse; v1 is mock-first)")

// Scorer scores a candidate revision against a dataset. Each scorer returns a
// score in [0,1]; the suite score is the weighted mean of its scorers
// (WeightedMean). It is the single seam between the controller's deploy-gate and
// the mechanism that actually scores — swapping the impl (mock ⇄ Langfuse)
// changes nothing in the gate state machine.
type Scorer interface {
	// Score returns a score in [0,1] for the candidate against the dataset. dataset
	// is the EvalSuite's dataset ref (a Langfuse dataset name or an inline fixture
	// key); candidate identifies the revision under test (its revision name / spec
	// digest). An error means the scorer could not produce a score (e.g. Langfuse
	// unreachable) — the caller decides the fail-closed/warn behaviour by gate mode.
	Score(ctx context.Context, dataset, candidate string) (float64, error)
}

// MockScorer is the deterministic, OFFLINE scorer used in dev / envtest / e2e. It
// never touches the network or a model — the score is a pure function of the
// scorer name, the dataset ref, and the candidate identity — yet it exercises the
// full gate state machine reproducibly: the SAME (name, dataset, candidate)
// always yields the SAME score, so the e2e trips block-then-promote
// deterministically.
//
// A per-scorer Seed table lets a test PIN an exact score for a (dataset,
// candidate) so it can drive the score deliberately across the threshold; an
// unseeded input uses a deterministic hash-derived fallback in [0,1].
type MockScorer struct {
	// name is the scorer's id within the suite (namespaces the derived score so two
	// mock scorers over the same dataset/candidate can differ).
	name string
	// seed pins an exact score for a "dataset\x00candidate" key. Optional; an
	// unseeded input uses the hash fallback.
	seed map[string]float64
}

// NewMockScorer returns a MockScorer with the given scorer name and no seeds
// (every input uses the deterministic hash fallback).
func NewMockScorer(name string) *MockScorer {
	return &MockScorer{name: name, seed: map[string]float64{}}
}

// Seed pins score for an exact (dataset, candidate) pair. Chainable. score is
// clamped to [0,1]. Used by tests/e2e to drive a candidate deliberately above or
// below the threshold.
func (m *MockScorer) Seed(dataset, candidate string, score float64) *MockScorer {
	m.seed[dataset+"\x00"+candidate] = clamp01(score)
	return m
}

// Score implements Scorer. It is pure and deterministic: no I/O, no clock, no
// randomness — the same (name, dataset, candidate) always yields the same score.
func (m *MockScorer) Score(_ context.Context, dataset, candidate string) (float64, error) {
	if s, ok := m.seed[dataset+"\x00"+candidate]; ok {
		return s, nil
	}
	// Deterministic fallback: hash (name, dataset, candidate) → a stable score in
	// [0,1]. Human-irrelevant but reproducible; a test that needs a specific score
	// uses Seed.
	payload := fmt.Sprintf("name=%s;dataset=%s;candidate=%s", m.name, dataset, candidate)
	h := sha256.Sum256([]byte(payload))
	n := binary.BigEndian.Uint64(h[:8])
	// Map the 64-bit hash into [0,1] with fixed precision. Dividing by MaxUint64
	// keeps the result deterministic across platforms (no float rounding on the
	// integer division path).
	return float64(n) / float64(^uint64(0)), nil
}

var _ Scorer = (*MockScorer)(nil)

// ScorerFor returns the Scorer for a given EvalSuite scorer type + name.
//
//   - mock              → a deterministic MockScorer (built).
//   - llm-judge / code  → ErrScorerUnavailable in v1 (documented-future; requires
//     a live Langfuse). The controller maps this to the fail-closed/warn gate
//     behaviour, so a suite that names a real scorer offline never silently
//     promotes under gate:block.
//
// A production build wires a Langfuse-backed factory here (the one construction
// site), leaving the gate state machine untouched — the mock⇄real seam.
func ScorerFor(scorerType, name string) (Scorer, error) {
	switch scorerType {
	case ScorerTypeMock:
		return NewMockScorer(name), nil
	case ScorerTypeLLMJudge, ScorerTypeCode:
		return nil, fmt.Errorf("%w: type=%q", ErrScorerUnavailable, scorerType)
	default:
		return nil, fmt.Errorf("eval: unknown scorer type %q", scorerType)
	}
}

// clamp01 bounds x to [0,1].
func clamp01(x float64) float64 {
	switch {
	case x < 0:
		return 0
	case x > 1:
		return 1
	default:
		return x
	}
}
