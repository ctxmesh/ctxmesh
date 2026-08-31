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

package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
)

// TestScheduledRefreshDecision covers the pure scheduling predicate (M140.4) — the load-bearing logic, tested
// deterministically without a cluster (tier 0).
func TestScheduledRefreshDecision(t *testing.T) {
	iv := func(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }
	ago := func(d time.Duration) *metav1.Time { tm := metav1.NewTime(time.Now().Add(-d)); return &tm }
	now := time.Now()

	t.Run("no interval → off", func(t *testing.T) {
		create, _ := scheduledRefreshDecision(&agentsv1beta1.KnowledgeBase{}, now)
		require.False(t, create)
	})

	t.Run("never ingested → due now", func(t *testing.T) {
		kb := &agentsv1beta1.KnowledgeBase{Spec: agentsv1beta1.KnowledgeBaseSpec{RefreshInterval: iv(5 * time.Minute)}}
		create, _ := scheduledRefreshDecision(kb, now)
		require.True(t, create, "a declared cadence with no prior ingest fires now")
	})

	t.Run("elapsed > interval → due", func(t *testing.T) {
		kb := &agentsv1beta1.KnowledgeBase{Spec: agentsv1beta1.KnowledgeBaseSpec{RefreshInterval: iv(time.Minute)}}
		kb.Status.LastIngestedAt = ago(2 * time.Minute)
		create, _ := scheduledRefreshDecision(kb, now)
		require.True(t, create)
	})

	t.Run("elapsed < interval → requeue, not due", func(t *testing.T) {
		kb := &agentsv1beta1.KnowledgeBase{Spec: agentsv1beta1.KnowledgeBaseSpec{RefreshInterval: iv(time.Minute)}}
		kb.Status.LastIngestedAt = ago(10 * time.Second)
		create, requeue := scheduledRefreshDecision(kb, now)
		require.False(t, create)
		require.Greater(t, requeue, time.Duration(0))
	})

	t.Run("in-flight (Ingesting) → skip", func(t *testing.T) {
		kb := &agentsv1beta1.KnowledgeBase{Spec: agentsv1beta1.KnowledgeBaseSpec{RefreshInterval: iv(time.Minute)}}
		kb.Status.Phase = kbPhaseIngesting
		kb.Status.LastIngestedAt = ago(2 * time.Minute)
		create, _ := scheduledRefreshDecision(kb, now)
		require.False(t, create, "never create while an ingest is in flight")
	})

	t.Run("tiny interval clamped to the 1m floor", func(t *testing.T) {
		kb := &agentsv1beta1.KnowledgeBase{Spec: agentsv1beta1.KnowledgeBaseSpec{RefreshInterval: iv(time.Second)}}
		kb.Status.LastIngestedAt = ago(30 * time.Second) // < the 1m floor
		create, _ := scheduledRefreshDecision(kb, now)
		require.False(t, create, "1s clamps to 1m, so 30s-ago is not yet due")
	})

	t.Run("backoff keys off the last ATTEMPT, not just success", func(t *testing.T) {
		kb := &agentsv1beta1.KnowledgeBase{Spec: agentsv1beta1.KnowledgeBaseSpec{RefreshInterval: iv(time.Minute)}}
		kb.Status.LastIngestedAt = ago(10 * time.Minute)        // last SUCCESS long ago
		kb.Status.LastScheduledIngestAt = ago(30 * time.Second) // but attempted 30s ago
		create, _ := scheduledRefreshDecision(kb, now)
		require.False(t, create, "a failing source retries once per interval, not hotter")
	})
}
