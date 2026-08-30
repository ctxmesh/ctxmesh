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

// Tenant corpus storage metrics (ADR 0061 governance #7, M68 m68.12).
// Registered once into the controller-runtime registry so they appear at /metrics.
// The gauge tracks aggregate corpus bytes per tenant — a soft-cap signal.
// Hard storage-quota enforcement is deferred (m52 Theme M).
var (
	// tenantCorpusBytesGauge tracks the aggregate corpus bytes (sum of
	// KnowledgeBase.status.sizeBytes) per tenant. A value at or above
	// corpusBytesSoftCap should alert operators that the tenant is approaching
	// or has exceeded its soft storage ceiling.
	tenantCorpusBytesGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "agentry_tenant_corpus_bytes",
		Help: "Total corpus bytes (sum of KnowledgeBase.status.sizeBytes) across all member namespaces for a tenant. " +
			"Exceeding storage.corpusBytesSoftCap triggers a StorageSoftCapExceeded warning condition (SOFT — never blocks ingestion). " +
			"Hard enforcement is deferred (m52 Theme M).",
	}, []string{"tenant"})
)

func init() {
	metrics.Registry.MustRegister(tenantCorpusBytesGauge)
}
