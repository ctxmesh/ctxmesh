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

// Package bff is the M12 Backend-for-Frontend for the agent-engine operator UI
// (ADR 0010). It is a server-side layer in the Go control plane: it reuses the
// controllers' client-go to read/write the agent CRDs, sits behind the M11
// control-plane auth, and serves the static Vite SPA build. Credentials
// (Kubernetes, and later Langfuse/Prometheus) stay server-side — the browser
// never receives them.
//
// This file defines the UI-shaped DTOs the SPA consumes. They are deliberately
// a thin, flat projection of the CRDs — never the raw Kubernetes objects — so
// the API contract with the SPA is stable and small.
package bff

import (
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
)

// Agent lifecycle phases the BFF projects onto the UI DTO, derived from the
// AgentDeployment "Ready" condition.
const (
	phaseReady    = "Ready"
	phaseNotReady = "NotReady"
	phasePending  = "Pending"
)

// HealthResponse is returned by GET /api/health. It doubles as a version probe
// for the SPA (the dashboard renders it to prove the BFF seam is live).
type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// AgentSummary is the UI projection of a single AgentDeployment. It exposes only
// what the dashboard/config-builder need; the rich detail views (m12.5+) fetch
// more via dedicated endpoints. Keeping this flat decouples the SPA from the CRD
// schema churn.
type AgentSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Image     string `json:"image"`
	Phase     string `json:"phase"`
	Ready     bool   `json:"ready"`
}

// AgentListResponse is returned by GET /api/agents.
type AgentListResponse struct {
	Agents []AgentSummary `json:"agents"`
}

// newAgentSummary projects an AgentDeployment onto the UI DTO. The Ready flag
// and Phase are derived from the standard "Ready" condition (which mirrors the
// underlying Knative Service, per the CRD status contract). agents is never nil
// on the wire — the list endpoint returns [] for "no agents".
func newAgentSummary(ad *agentsv1alpha1.AgentDeployment) AgentSummary {
	ready := false
	phase := phasePending
	if c := apimeta.FindStatusCondition(ad.Status.Conditions, "Ready"); c != nil {
		ready = c.Status == metav1.ConditionTrue
		switch c.Status {
		case metav1.ConditionTrue:
			phase = phaseReady
		case metav1.ConditionFalse:
			phase = phaseNotReady
		default:
			phase = phasePending
		}
	}
	return AgentSummary{
		Name:      ad.Name,
		Namespace: ad.Namespace,
		Image:     ad.Spec.Image,
		Phase:     phase,
		Ready:     ready,
	}
}
