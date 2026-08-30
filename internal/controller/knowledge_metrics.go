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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// KnowledgeBase ingestion robustness metrics (M16, ADR 0061 Fork 2).
var (
	// kbIngestionSafetyNetTotal counts how often the reconcileStuckIngesting safety-net had to
	// project phase:Failed onto a KnowledgeBase because its referenced ingestion run terminated
	// failed/cancelled/expired WITHOUT writing a corpus-status row (an out-of-band terminal
	// transition). The safety-net is a rescue, not the primary path — the executor's
	// recordCorpusStatus is meant to project every terminal outcome — so a NON-ZERO (or climbing)
	// value means the primary status channel is dropping terminal-failure writes. Previously the
	// rescue only logged (invisible in monitoring); this makes the silent stuck→Failed observable.
	kbIngestionSafetyNetTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "agentry_kb_ingestion_safetynet_total",
		Help: "Times the KnowledgeBase controller safety-net projected phase:Failed because an ingestion run " +
			"terminated failed out-of-band (no corpus-status row). Non-zero ⇒ the executor status channel is " +
			"dropping terminal-failure writes (M16, ADR 0061 Fork 2).",
	}, []string{"namespace"})
)

func init() {
	metrics.Registry.MustRegister(kbIngestionSafetyNetTotal)
}
