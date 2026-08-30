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
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/ctxmesh/agentry/api/v1alpha1"
)

// TenantUsageReader reads a tenant's LIVE quota consumption from the shared state-layer Valkey (M49, the
// M47-review P0: a quota surface must answer "who's about to be throttled?", not just show the caps). It
// reads the SAME keys the launcher's tenant enforcer writes (cmd/launcher/tenant_quota.go). Read-only; an
// interface so the BFF tests use a fake without a live Valkey.
type TenantUsageReader interface {
	Usage(ctx context.Context, tenantID string) (TenantUsage, error)
}

// TenantUsage is a tenant's current-period consumption.
type TenantUsage struct {
	SpendUSD float64 `json:"spendUSD"` // this month's accrued model spend
	RPM      int64   `json:"rpm"`      // requests in the current minute window
	InFlight int64   `json:"inFlight"` // current concurrent in-flight requests
}

type redisTenantUsage struct{ rdb *redis.Client }

// usageOpTimeout bounds a single dial/read against the state-layer Valkey. This is a UI-facing
// observability read: a slow/unreachable state-layer must degrade FAST (the handler 500s → the console
// hides the usage line) rather than stalling the request on go-redis's multi-second defaults. Mirrors the
// launcher's bounded memory/dedupe clients (cmd/launcher/memory.go, async.go).
const usageOpTimeout = 2 * time.Second

// NewRedisTenantUsageReader connects (read-only) to the shared state-layer Valkey at addr. No password by
// design: the in-cluster Valkey is unauthenticated on purpose (ADR 0049 — in-cluster `requirepass` was DECLINED
// because the Knative no-`valueFrom`-in-ksvc constraint forces the launcher's password to be broadly readable,
// making it security theater). This reader matches the launcher's unauthed tenant-store writer
// (cmd/launcher/tenant_quota.go). Password auth is the BYO-external-Valkey posture (`devDataPlane.enabled=false`);
// real per-tenant isolation of the in-cluster store is the roadmapped memory-proxy + Valkey-ACLs (ADR 0049).
func NewRedisTenantUsageReader(addr string) TenantUsageReader {
	return &redisTenantUsage{rdb: redis.NewClient(&redis.Options{
		Addr:        addr,
		DialTimeout: usageOpTimeout,
		ReadTimeout: usageOpTimeout,
	})}
}

func (r *redisTenantUsage) Usage(ctx context.Context, tenantID string) (TenantUsage, error) {
	now := time.Now().UTC()
	var u TenantUsage
	if v, err := r.rdb.Get(ctx, "tenant:"+tenantID+":spend:"+now.Format("2006-01")).Float64(); err == nil {
		u.SpendUSD = v
	} else if err != redis.Nil {
		return u, err
	}
	if v, err := r.rdb.Get(ctx, "tenant:"+tenantID+":rpm:"+strconv.FormatInt(now.Unix()/60, 10)).Int64(); err == nil {
		u.RPM = v
	} else if err != redis.Nil {
		return u, err
	}
	if v, err := r.rdb.Get(ctx, "tenant:"+tenantID+":inflight").Int64(); err == nil {
		u.InFlight = v
	} else if err != redis.Nil {
		return u, err
	}
	return u, nil
}

// handleTenantUsage serves GET /api/tenants/{name}/usage — the tenant's live consumption from the shared
// Valkey. Caller-scoped (the caller must be able to GET the tenant); 501 when no usage reader is wired.
func (s *Server) handleTenantUsage(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	// Authorize via the caller-scoped Get (a viewer's 403 / a 404 surfaced honestly).
	var t agentsv1alpha1.Tenant
	if err := caller.Get(r.Context(), client.ObjectKey{Name: name}, &t); err != nil {
		s.writeGetError(w, err, "tenant")
		return
	}
	if s.tenantUsage == nil {
		writeError(w, http.StatusNotImplemented, "tenant usage is not available (no state-layer configured)")
		return
	}
	u, err := s.tenantUsage.Usage(r.Context(), name)
	if err != nil {
		s.log.Error(err, "read tenant usage failed", "tenant", name)
		writeError(w, http.StatusInternalServerError, "failed to read tenant usage")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// TenantUsageItem pairs a tenant name with its live usage (the batched list row).
type TenantUsageItem struct {
	Name string `json:"name"`
	TenantUsage
}

// TenantUsageListResponse is the batched usage payload (GET /api/tenants/usage).
type TenantUsageListResponse struct {
	Items []TenantUsageItem `json:"items"`
}

// handleTenantUsageList serves GET /api/tenants/usage — the LIVE usage for every
// tenant the caller can list, in ONE round-trip (m54.5), so the tenants list can
// render a near-cap indicator per row without a per-row fan-out. Caller-scoped: the
// set is exactly the tenants the caller's own List returns (ADR 0011). 501 when no
// usage reader is wired; a single tenant's read error is skipped (best-effort — one
// unreadable tenant must not blank the whole list).
func (s *Server) handleTenantUsageList(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.callerClient(w, r)
	if !ok {
		return
	}
	if s.tenantUsage == nil {
		writeError(w, http.StatusNotImplemented, "tenant usage is not available (no state-layer configured)")
		return
	}
	var list agentsv1alpha1.TenantList
	if err := caller.List(r.Context(), &list); err != nil {
		if status, msg, isRBAC := classifyReadError(err); isRBAC {
			writeError(w, status, msg)
			return
		}
		s.log.Error(err, "list Tenants for usage failed")
		writeError(w, http.StatusInternalServerError, "failed to list tenants")
		return
	}
	items := make([]TenantUsageItem, 0, len(list.Items))
	for i := range list.Items {
		name := list.Items[i].Name
		u, err := s.tenantUsage.Usage(r.Context(), name)
		if err != nil {
			// Best-effort: skip a tenant whose usage is momentarily unreadable rather
			// than failing the whole list (the row simply shows no indicator).
			s.log.Error(err, "read tenant usage failed (skipped in batch)", "tenant", name)
			continue
		}
		items = append(items, TenantUsageItem{Name: name, TenantUsage: u})
	}
	writeJSON(w, http.StatusOK, TenantUsageListResponse{Items: items})
}
