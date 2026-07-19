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
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ctxmesh/agent-engine/internal/kedatypes"
)

// The durable run-worker (ADR 0034, m32.2) is a platform singleton — NOT a per-agent workload — so
// its ScaledObject is built from this fixed config, not reconciled from a CRD. It scales the worker
// Deployment on the depth of the queued-run backlog in the durable store.
const (
	runWorkerScaledObjectName = "run-worker"
	// runWorkerDSNEnv is the env var on the worker Deployment holding the Postgres DSN; KEDA's
	// postgresql scaler reads the connection from it (connectionFromEnv), so there is no second copy
	// of the DSN in a TriggerAuthentication/Secret — the worker and the scaler share one source.
	runWorkerDSNEnv = "RUN_STORE_DSN"
	// runWorkerQueueQuery is the queue-depth query KEDA polls: the count of runs awaiting a worker.
	runWorkerQueueQuery = "SELECT count(*) FROM runs WHERE status = 'queued'"
	// KEDA scaler type + a stable trigger name.
	kedaPostgreSQLScalerType = "postgresql"
	runWorkerTriggerName     = "queued-runs"
)

// RunWorkerScaleConfig parameterises the run-worker ScaledObject.
type RunWorkerScaleConfig struct {
	// Namespace + TargetName locate the run-worker Deployment to scale.
	Namespace  string
	TargetName string
	// MinReplicas may be 0: a durable run outlives its worker, so scaling to zero is safe — a new
	// replica claims any queued/reclaimable run when work arrives. MaxReplicas caps fan-out.
	MinReplicas int32
	MaxReplicas int32
	// QueueThreshold is the desired queued-runs-per-replica (KEDA's targetQueryValue).
	QueueThreshold int32
	// CooldownSecs is the scale-down cooldown after the queue drains.
	CooldownSecs int32
}

func (c RunWorkerScaleConfig) withDefaults() RunWorkerScaleConfig {
	if c.TargetName == "" {
		c.TargetName = runWorkerScaledObjectName
	}
	if c.MaxReplicas <= 0 {
		c.MaxReplicas = 10
	}
	if c.QueueThreshold <= 0 {
		c.QueueThreshold = 5
	}
	if c.CooldownSecs <= 0 {
		c.CooldownSecs = 60
	}
	return c
}

// BuildRunWorkerScaledObject builds the KEDA ScaledObject autoscaling the durable run-worker
// Deployment on the queued-run backlog (ADR 0034, m32.2). It is the single source of truth for the
// wiring — the shipped manifest mirrors it and an envtest asserts KEDA accepts it.
func BuildRunWorkerScaledObject(cfg RunWorkerScaleConfig) *kedatypes.ScaledObject {
	cfg = cfg.withDefaults()
	minR, maxR, cooldown := cfg.MinReplicas, cfg.MaxReplicas, cfg.CooldownSecs
	return &kedatypes.ScaledObject{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "keda.sh/v1alpha1",
			Kind:       "ScaledObject",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      runWorkerScaledObjectName,
			Namespace: cfg.Namespace,
		},
		Spec: kedatypes.ScaledObjectSpec{
			ScaleTargetRef: &kedatypes.ScaleTarget{
				Name:       cfg.TargetName,
				Kind:       "Deployment",
				APIVersion: "apps/v1",
			},
			MinReplicaCount: &minR,
			MaxReplicaCount: &maxR,
			CooldownPeriod:  &cooldown,
			Triggers: []kedatypes.ScaleTriggers{{
				Type: kedaPostgreSQLScalerType,
				Name: runWorkerTriggerName,
				Metadata: map[string]string{
					"connectionFromEnv":          runWorkerDSNEnv,
					"query":                      runWorkerQueueQuery,
					"targetQueryValue":           strconv.Itoa(int(cfg.QueueThreshold)),
					"activationTargetQueryValue": "0",
				},
			}},
		},
	}
}
