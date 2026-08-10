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

// Package onlinescore is the control-plane store for per-agent-version online score aggregates
// (ADR 0062 Fork 2, M69 — the improvement loop's production/online half). An online score is a
// 3-component vector (operational + feedback + judge) stored UN-COLLAPSED keyed by
// (namespace, agentName, agentVersion, windowStart) so regression detection can inspect each
// component independently. This package is the operational component only (error rate,
// tool-failure rate, latency p95 — free, deterministic, from the run store + traces).
// The feedback + judge components are added by the online-scoring worker (m69.5).
package onlinescore

import (
	"context"
	"time"
)

// OperationalStats holds the free, deterministic metrics derived from runs and traces.
type OperationalStats struct {
	Total         int
	ErrorCount    int
	ToolFailCount int
	LatencyP95Ms  float64
}

// FeedbackStats accumulates user-provided feedback signals for a window.
type FeedbackStats struct {
	Count  int
	SumVal float64
}

// JudgeStats accumulates LLM-judge scores for a window.
type JudgeStats struct {
	Count  int
	SumVal float64
}

// Aggregate is the per-(namespace, agentName, agentVersion, windowStart) online score record.
// WindowStart is always truncated to the hour boundary.
type Aggregate struct {
	ID           string
	Namespace    string
	AgentName    string
	AgentVersion string
	WindowStart  time.Time
	Operational  OperationalStats
	Feedback     FeedbackStats
	Judge        JudgeStats
	UpdatedAt    time.Time
}

// Store persists and retrieves Aggregate records. All implementations must truncate WindowStart
// to the hour boundary before storage and return controlplane.ErrNotFound for missing rows.
type Store interface {
	// UpsertAggregate writes (or updates) the aggregate keyed by
	// (Namespace, AgentName, AgentVersion, WindowStart truncated to the hour).
	UpsertAggregate(ctx context.Context, a Aggregate) error

	// GetAggregate retrieves the aggregate for the given key. WindowStart is truncated to
	// the hour before lookup. Returns controlplane.ErrNotFound when no row exists.
	GetAggregate(ctx context.Context, namespace, agentName, agentVersion string, windowStart time.Time) (*Aggregate, error)

	// ListAggregates returns aggregates for (namespace, agentName) sorted by WindowStart DESC.
	// limit <= 0 returns all aggregates.
	ListAggregates(ctx context.Context, namespace, agentName string, limit int) ([]Aggregate, error)
}
