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
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ctxmesh/agent-engine/internal/controlplane/authz"
	"github.com/ctxmesh/agent-engine/internal/controlplane/costrollup"
)

// resourceCostRollups is the virtual resource the cost-forecast and chargeback
// endpoints authorize against. A caller whose ClusterRole grants `list costrollups`
// (or the platform admin ClusterRole) sees the rollup-backed surfaces. Not a CRD —
// the persona's RBAC on this name IS the cost-read policy (same pattern as auditlogs).
const resourceCostRollups = "costrollups"

// CostForecastResponse is the GET /api/cost/forecast wire DTO (M70, ADR 0063 D3).
// All USD values are rounded to 6 decimal places on the wire for consistent precision.
type CostForecastResponse struct {
	Tenant               string  `json:"tenant"`
	MonthToDateUSD       float64 `json:"monthToDateUSD"`
	ProjectedMonthEndUSD float64 `json:"projectedMonthEndUSD"`
	AsOf                 string  `json:"asOf"` // RFC3339
}

// handleCostForecast serves GET /api/cost/forecast?tenant= (M70, ADR 0063 D3).
//
// Caller-scoped SSAR on `costrollups` (persona gate — a denial is 403, never a
// leaked or empty response). Reads the durable cost-rollup ledger for the current
// month, calls costrollup.LinearForecast, and returns the MTD + projected month-end.
//
// 501 when the rollup store is nil (CONTROLPLANE_DSN not set).
// 400 when ?tenant= is missing or empty.
func (s *Server) handleCostForecast(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.rollupStore == nil {
		writeError(w, http.StatusNotImplemented,
			"the cost forecast requires the control-plane store (CONTROLPLANE_DSN); it is not enabled")
		return
	}

	tenant := strings.TrimSpace(r.URL.Query().Get("tenant"))
	if tenant == "" {
		writeError(w, http.StatusBadRequest, "missing required query param: tenant")
		return
	}

	// Persona gate (never per-row): one SSAR on `costrollups`. Cluster-wide
	// (empty namespace) — cost-rollup data is tenant-scoped, not namespace-scoped.
	if err := s.authorizeStore(r.Context(), caller, authz.VerbList, resourceCostRollups, "", ""); err != nil {
		s.writeAuthzError(w, err, "read the cost forecast")
		return
	}

	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	rollups, err := s.rollupStore.Range(r.Context(), "tenant", tenant, monthStart, now)
	if err != nil {
		s.log.Error(err, "cost forecast: rollup range read failed", "tenant", tenant)
		writeError(w, http.StatusInternalServerError, "failed to read the cost rollup")
		return
	}

	var mtd float64
	if len(rollups) > 0 {
		// Range returns day-ASC; the last row is the current MTD cumulative spend.
		mtd = rollups[len(rollups)-1].SpendUSD
	}

	projected, _ := costrollup.LinearForecast(rollups, now)
	// ok=false (empty / now==monthStart) → projected remains 0 — honest.

	writeJSON(w, http.StatusOK, CostForecastResponse{
		Tenant:               tenant,
		MonthToDateUSD:       mtd,
		ProjectedMonthEndUSD: projected,
		AsOf:                 now.Format(time.RFC3339),
	})
}

// handleCostChargeback serves GET /api/cost/chargeback?tenant=&period=YYYY-MM (M70, ADR 0063 D3).
//
// Caller-scoped SSAR on `costrollups` (same persona gate as forecast). Returns the
// per-day rollup for the requested calendar month. When Accept: text/csv or
// ?format=csv is requested, the response is a CSV file; otherwise JSON.
//
// 501 when the rollup store is nil.
// 400 when ?tenant= or ?period= is missing/invalid.
func (s *Server) handleCostChargeback(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.rollupStore == nil {
		writeError(w, http.StatusNotImplemented,
			"the cost chargeback requires the control-plane store (CONTROLPLANE_DSN); it is not enabled")
		return
	}

	tenant := strings.TrimSpace(r.URL.Query().Get("tenant"))
	if tenant == "" {
		writeError(w, http.StatusBadRequest, "missing required query param: tenant")
		return
	}

	period := strings.TrimSpace(r.URL.Query().Get("period"))
	if period == "" {
		writeError(w, http.StatusBadRequest, "missing required query param: period (format YYYY-MM)")
		return
	}
	periodStart, err := time.Parse("2006-01", period)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid period format — want YYYY-MM, e.g. 2026-08")
		return
	}
	periodStart = periodStart.UTC()
	// Period end = last second of the last day of the month.
	periodEnd := time.Date(periodStart.Year(), periodStart.Month()+1, 1, 0, 0, 0, 0, time.UTC).
		Add(-time.Second)

	// Persona gate (never per-row): one SSAR on `costrollups`.
	if err := s.authorizeStore(r.Context(), caller, authz.VerbList, resourceCostRollups, "", ""); err != nil {
		s.writeAuthzError(w, err, "read the cost chargeback")
		return
	}

	rollups, err := s.rollupStore.Range(r.Context(), "tenant", tenant, periodStart, periodEnd)
	if err != nil {
		s.log.Error(err, "cost chargeback: rollup range read failed", "tenant", tenant, "period", period)
		writeError(w, http.StatusInternalServerError, "failed to read the cost rollup")
		return
	}

	// CSV when Accept: text/csv or ?format=csv.
	wantCSV := strings.EqualFold(r.URL.Query().Get("format"), "csv") ||
		strings.Contains(r.Header.Get("Accept"), "text/csv")

	if wantCSV {
		writeChargebackCSV(w, rollups)
		return
	}
	writeJSON(w, http.StatusOK, chargebackJSONResponse(rollups))
}

// ChargebackRow is one row in the GET /api/cost/chargeback JSON response.
type ChargebackRow struct {
	ScopeType string  `json:"scope_type"`
	ScopeID   string  `json:"scope_id"`
	Day       string  `json:"day"` // RFC3339 date (midnight UTC)
	SpendUSD  float64 `json:"spend_usd"`
	Tokens    int64   `json:"tokens"`
}

// ChargebackResponse is the GET /api/cost/chargeback JSON body.
type ChargebackResponse struct {
	Items []ChargebackRow `json:"items"`
}

func chargebackJSONResponse(rollups []costrollup.Rollup) ChargebackResponse {
	items := make([]ChargebackRow, 0, len(rollups))
	for i := range rollups {
		r := &rollups[i]
		items = append(items, ChargebackRow{
			ScopeType: r.ScopeType,
			ScopeID:   r.ScopeID,
			Day:       r.Day.UTC().Format(time.RFC3339),
			SpendUSD:  r.SpendUSD,
			Tokens:    r.Tokens,
		})
	}
	return ChargebackResponse{Items: items}
}

// writeChargebackCSV writes rollups as a CSV to w with columns:
//
//	scope_type,scope_id,day,spend_usd,tokens
func writeChargebackCSV(w http.ResponseWriter, rollups []costrollup.Rollup) {
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=chargeback.csv")
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"scope_type", "scope_id", "day", "spend_usd", "tokens"})
	for i := range rollups {
		r := &rollups[i]
		_ = cw.Write([]string{
			r.ScopeType,
			r.ScopeID,
			r.Day.UTC().Format("2006-01-02"),
			fmt.Sprintf("%.6f", r.SpendUSD),
			fmt.Sprintf("%d", r.Tokens),
		})
	}
	cw.Flush()
}
