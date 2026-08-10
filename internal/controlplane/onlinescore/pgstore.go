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

package onlinescore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

// pgStore is the Postgres-backed Store. The schema (online_score_aggregates — migration 0008) is applied
// by the control-plane goose migrations (controlplane.Migrate), not here; the store assumes the table exists.
type pgStore struct {
	db *sql.DB
}

// NewPostgresStore returns a Store over the given control-plane DB handle. Migrations are the caller's job
// (controlplane.OpenDB / controlplane.Migrate), matching the operator-owns-its-schema model.
func NewPostgresStore(db *sql.DB) Store { return &pgStore{db: db} }

// UpsertAggregate inserts or updates the aggregate row keyed by (namespace, agent_name, agent_version, window_start).
// window_start is truncated to the hour before storage.
func (s *pgStore) UpsertAggregate(ctx context.Context, a Aggregate) error {
	if a.Namespace == "" || a.AgentName == "" {
		return fmt.Errorf("onlinescore: %w: namespace and agentName are required", controlplane.ErrInvalid)
	}
	a.WindowStart = a.WindowStart.Truncate(time.Hour)
	_, err := s.db.ExecContext(
		ctx, `
		INSERT INTO online_score_aggregates
			(namespace, agent_name, agent_version, window_start,
			 total, error_count, tool_fail_count, latency_p95_ms,
			 feedback_count, feedback_sum,
			 judge_count, judge_sum,
			 updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, now())
		ON CONFLICT (namespace, agent_name, agent_version, window_start) DO UPDATE SET
			total           = EXCLUDED.total,
			error_count     = EXCLUDED.error_count,
			tool_fail_count = EXCLUDED.tool_fail_count,
			latency_p95_ms  = EXCLUDED.latency_p95_ms,
			feedback_count  = EXCLUDED.feedback_count,
			feedback_sum    = EXCLUDED.feedback_sum,
			judge_count     = EXCLUDED.judge_count,
			judge_sum       = EXCLUDED.judge_sum,
			updated_at      = now()`,
		a.Namespace, a.AgentName, a.AgentVersion, a.WindowStart,
		a.Operational.Total, a.Operational.ErrorCount, a.Operational.ToolFailCount, a.Operational.LatencyP95Ms,
		a.Feedback.Count, a.Feedback.SumVal,
		a.Judge.Count, a.Judge.SumVal,
	)
	if err != nil {
		return fmt.Errorf("onlinescore: upsert aggregate: %w", err)
	}
	return nil
}

// GetAggregate retrieves the aggregate for the given key. window_start is truncated to the hour before lookup.
// Returns controlplane.ErrNotFound when no row exists.
func (s *pgStore) GetAggregate(ctx context.Context, namespace, agentName, agentVersion string, windowStart time.Time) (*Aggregate, error) {
	windowStart = windowStart.Truncate(time.Hour)
	var a Aggregate
	err := s.db.QueryRowContext(ctx, `
		SELECT id, namespace, agent_name, agent_version, window_start,
			total, error_count, tool_fail_count, latency_p95_ms,
			feedback_count, feedback_sum,
			judge_count, judge_sum,
			updated_at
		FROM online_score_aggregates
		WHERE namespace = $1 AND agent_name = $2 AND agent_version = $3 AND window_start = $4`,
		namespace, agentName, agentVersion, windowStart).
		Scan(
			&a.ID, &a.Namespace, &a.AgentName, &a.AgentVersion, &a.WindowStart,
			&a.Operational.Total, &a.Operational.ErrorCount, &a.Operational.ToolFailCount, &a.Operational.LatencyP95Ms,
			&a.Feedback.Count, &a.Feedback.SumVal,
			&a.Judge.Count, &a.Judge.SumVal,
			&a.UpdatedAt,
		)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("onlinescore: %w", controlplane.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("onlinescore: get aggregate: %w", err)
	}
	a.WindowStart = a.WindowStart.UTC()
	a.UpdatedAt = a.UpdatedAt.UTC()
	return &a, nil
}

// ListAggregates returns aggregates for (namespace, agentName) sorted by window_start DESC.
// When limit <= 0, all aggregates are returned (bounded internally to 100000 to protect the DB).
func (s *pgStore) ListAggregates(ctx context.Context, namespace, agentName string, limit int) ([]Aggregate, error) {
	effectiveLimit := limit
	if effectiveLimit <= 0 {
		effectiveLimit = 100000
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, namespace, agent_name, agent_version, window_start,
			total, error_count, tool_fail_count, latency_p95_ms,
			feedback_count, feedback_sum,
			judge_count, judge_sum,
			updated_at
		FROM online_score_aggregates
		WHERE namespace = $1 AND agent_name = $2
		ORDER BY window_start DESC
		LIMIT $3`,
		namespace, agentName, effectiveLimit)
	if err != nil {
		return nil, fmt.Errorf("onlinescore: list aggregates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Aggregate, 0)
	for rows.Next() {
		var a Aggregate
		if err := rows.Scan(
			&a.ID, &a.Namespace, &a.AgentName, &a.AgentVersion, &a.WindowStart,
			&a.Operational.Total, &a.Operational.ErrorCount, &a.Operational.ToolFailCount, &a.Operational.LatencyP95Ms,
			&a.Feedback.Count, &a.Feedback.SumVal,
			&a.Judge.Count, &a.Judge.SumVal,
			&a.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("onlinescore: list aggregates scan: %w", err)
		}
		a.WindowStart = a.WindowStart.UTC()
		a.UpdatedAt = a.UpdatedAt.UTC()
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("onlinescore: list aggregates rows: %w", err)
	}
	return out, nil
}
