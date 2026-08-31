package bff

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/costrollup"
)

// newCostServer builds a caller-scoped BFF server with the given rollupStore and
// authorizer wired. Mirrors the pattern from newAlertsServer.
func newCostServer(t *testing.T, auth authz.Authorizer, store costrollup.Store) *Server {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	s.rollupStore = store
	if auth != nil {
		s.authorizer = auth
	} else {
		s.authorizer = &recordingAuthorizer{}
	}
	return s
}

// seedRollup is a helper that upserts a single tenant-scope rollup row into the store.
func seedRollup(t *testing.T, store costrollup.Store, scopeID string, day time.Time, spendUSD float64, tokens int64) {
	t.Helper()
	err := store.Upsert(context.Background(), costrollup.Rollup{
		ScopeType: "tenant",
		ScopeID:   scopeID,
		Day:       day,
		SpendUSD:  spendUSD,
		Tokens:    tokens,
	})
	require.NoError(t, err)
}

// getForecast issues GET /api/cost/forecast?<rawQuery> and returns the decoded body + status code.
func getForecast(t *testing.T, s *Server, rawQuery string) (CostForecastResponse, int) {
	t.Helper()
	url := "/api/cost/forecast"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	s.Handler().ServeHTTP(rec, req)
	var body CostForecastResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body, rec.Code
}

// getChargeback issues GET /api/cost/chargeback?<rawQuery> and returns the body + status code.
func getChargeback(t *testing.T, s *Server, rawQuery string, acceptCSV bool) (string, int) {
	t.Helper()
	url := "/api/cost/chargeback"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer caller-token")
	if acceptCSV {
		req.Header.Set("Accept", "text/csv")
	}
	s.Handler().ServeHTTP(rec, req)
	return rec.Body.String(), rec.Code
}

// ── Forecast: nil store is 501 ─────────────────────────────────────────────────

func TestCostForecast_NilStoreIs501(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	s.rollupStore = nil
	s.authorizer = &recordingAuthorizer{}

	_, code := getForecast(t, s, "tenant=acme")
	assert.Equal(t, http.StatusNotImplemented, code, "no rollup store ⇒ 501")
}

// ── Forecast: missing tenant is 400 ───────────────────────────────────────────

func TestCostForecast_MissingTenantIs400(t *testing.T) {
	store := costrollup.NewMemStore()
	s := newCostServer(t, nil, store)

	_, code := getForecast(t, s, "")
	assert.Equal(t, http.StatusBadRequest, code, "missing ?tenant= ⇒ 400")
}

// ── Forecast: persona gate (SSAR on costrollups) ──────────────────────────────

func TestCostForecast_PersonaDeniedIs403(t *testing.T) {
	store := costrollup.NewMemStore()
	s := newCostServer(t, &recordingAuthorizer{err: authz.ErrForbidden}, store)

	_, code := getForecast(t, s, "tenant=acme")
	assert.Equal(t, http.StatusForbidden, code, "no costrollups persona ⇒ 403, never a data leak")
}

func TestCostForecast_GatesOnListCostRollups(t *testing.T) {
	store := costrollup.NewMemStore()
	rec := &recordingAuthorizer{}
	s := newCostServer(t, rec, store)

	_, code := getForecast(t, s, "tenant=acme")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, authz.VerbList, rec.last.Verb)
	assert.Equal(t, resourceCostRollups, rec.last.Resource, "SSAR resource must be costrollups")
	assert.Equal(t, 1, rec.count, "exactly one persona gate, never per-row")
}

// ── Forecast: no rollups ⇒ zero MTD + zero projected ─────────────────────────

func TestCostForecast_EmptyStoreReturnsZeroes(t *testing.T) {
	store := costrollup.NewMemStore()
	s := newCostServer(t, nil, store)

	body, code := getForecast(t, s, "tenant=acme")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "acme", body.Tenant)
	assert.Equal(t, 0.0, body.MonthToDateUSD)
	assert.Equal(t, 0.0, body.ProjectedMonthEndUSD)
}

// ── Forecast: with data ⇒ correct MTD and projection ─────────────────────────

func TestCostForecast_WithDataReturnsProjection(t *testing.T) {
	store := costrollup.NewMemStore()
	// Seed 10 days of spend (MTD=100) in the current month.
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for d := range 10 {
		day := monthStart.AddDate(0, 0, d)
		if day.After(now) {
			break
		}
		seedRollup(t, store, "acme", day, float64((d+1)*10), int64((d+1)*1000))
	}
	s := newCostServer(t, nil, store)

	body, code := getForecast(t, s, "tenant=acme")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "acme", body.Tenant)
	assert.Greater(t, body.MonthToDateUSD, 0.0, "MTD should be non-zero with seed data")
	// If any days elapsed > 0, a positive MTD means a positive projection.
	if body.MonthToDateUSD > 0 {
		assert.Greater(t, body.ProjectedMonthEndUSD, 0.0, "projected should be positive when MTD > 0")
	}
}

// ── Chargeback: nil store is 501 ──────────────────────────────────────────────

func TestCostChargeback_NilStoreIs501(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	s := newCallerServer(t, &fakeCallerClientFactory{client: c})
	s.rollupStore = nil
	s.authorizer = &recordingAuthorizer{}

	_, code := getChargeback(t, s, "tenant=acme&period=2026-08", false)
	assert.Equal(t, http.StatusNotImplemented, code, "no rollup store ⇒ 501")
}

// ── Chargeback: missing/bad params ───────────────────────────────────────────

func TestCostChargeback_MissingTenantIs400(t *testing.T) {
	store := costrollup.NewMemStore()
	s := newCostServer(t, nil, store)

	_, code := getChargeback(t, s, "period=2026-08", false)
	assert.Equal(t, http.StatusBadRequest, code, "missing ?tenant= ⇒ 400")
}

func TestCostChargeback_MissingPeriodIs400(t *testing.T) {
	store := costrollup.NewMemStore()
	s := newCostServer(t, nil, store)

	_, code := getChargeback(t, s, "tenant=acme", false)
	assert.Equal(t, http.StatusBadRequest, code, "missing ?period= ⇒ 400")
}

func TestCostChargeback_BadPeriodFormatIs400(t *testing.T) {
	store := costrollup.NewMemStore()
	s := newCostServer(t, nil, store)

	_, code := getChargeback(t, s, "tenant=acme&period=not-a-date", false)
	assert.Equal(t, http.StatusBadRequest, code, "invalid period format ⇒ 400")
}

// ── Chargeback: persona gate ──────────────────────────────────────────────────

func TestCostChargeback_PersonaDeniedIs403(t *testing.T) {
	store := costrollup.NewMemStore()
	s := newCostServer(t, &recordingAuthorizer{err: authz.ErrForbidden}, store)

	_, code := getChargeback(t, s, "tenant=acme&period=2026-08", false)
	assert.Equal(t, http.StatusForbidden, code, "no costrollups persona ⇒ 403")
}

func TestCostChargeback_GatesOnListCostRollups(t *testing.T) {
	store := costrollup.NewMemStore()
	rec := &recordingAuthorizer{}
	s := newCostServer(t, rec, store)

	_, code := getChargeback(t, s, "tenant=acme&period=2026-08", false)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, authz.VerbList, rec.last.Verb)
	assert.Equal(t, resourceCostRollups, rec.last.Resource, "SSAR resource must be costrollups")
	assert.Equal(t, 1, rec.count, "exactly one persona gate, never per-row")
}

// ── Chargeback: JSON response ─────────────────────────────────────────────────

func TestCostChargeback_JSONResponse(t *testing.T) {
	store := costrollup.NewMemStore()
	// Seed two rows in August 2026.
	seedRollup(t, store, "acme", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 10.0, 1000)
	seedRollup(t, store, "acme", time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC), 50.0, 5000)
	// Seed a different tenant — should NOT appear.
	seedRollup(t, store, "other", time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC), 99.0, 9999)

	s := newCostServer(t, nil, store)

	body, code := getChargeback(t, s, "tenant=acme&period=2026-08", false)
	require.Equal(t, http.StatusOK, code)

	var resp ChargebackResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	require.Len(t, resp.Items, 2, "only acme rows for August")
	assert.Equal(t, "tenant", resp.Items[0].ScopeType)
	assert.Equal(t, "acme", resp.Items[0].ScopeID)
	assert.Equal(t, 10.0, resp.Items[0].SpendUSD)
	assert.Equal(t, int64(1000), resp.Items[0].Tokens)
}

// ── Chargeback: CSV via Accept header ────────────────────────────────────────

func TestCostChargeback_CSVViaAcceptHeader(t *testing.T) {
	store := costrollup.NewMemStore()
	seedRollup(t, store, "acme", time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), 25.5, 2500)

	s := newCostServer(t, nil, store)

	body, code := getChargeback(t, s, "tenant=acme&period=2026-08", true)
	require.Equal(t, http.StatusOK, code)
	assert.True(t, strings.HasPrefix(body, "scope_type,scope_id,day,spend_usd,tokens"),
		"first line must be CSV header; got: %q", body)
	assert.Contains(t, body, "acme", "CSV must contain the scope_id")
	assert.Contains(t, body, "25.500000", "CSV must contain spend_usd formatted to 6 dp")
	assert.Contains(t, body, "2500", "CSV must contain tokens")
}

// ── Chargeback: CSV via ?format=csv query param ───────────────────────────────

func TestCostChargeback_CSVViaQueryParam(t *testing.T) {
	store := costrollup.NewMemStore()
	seedRollup(t, store, "acme", time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC), 25.5, 2500)

	s := newCostServer(t, nil, store)

	body, code := getChargeback(t, s, "tenant=acme&period=2026-08&format=csv", false)
	require.Equal(t, http.StatusOK, code)
	assert.True(t, strings.HasPrefix(body, "scope_type,scope_id,day,spend_usd,tokens"),
		"first line must be CSV header; got: %q", body)
}

// ── Chargeback: empty period ⇒ 200 with empty items ──────────────────────────

func TestCostChargeback_EmptyPeriodReturnsEmptyItems(t *testing.T) {
	store := costrollup.NewMemStore()
	// Seed data for a DIFFERENT month (July 2026) — August query must yield empty.
	seedRollup(t, store, "acme", time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC), 10.0, 1000)

	s := newCostServer(t, nil, store)

	body, code := getChargeback(t, s, "tenant=acme&period=2026-08", false)
	require.Equal(t, http.StatusOK, code)

	var resp ChargebackResponse
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	assert.Empty(t, resp.Items, "no data in August ⇒ empty items, not an error")
}
