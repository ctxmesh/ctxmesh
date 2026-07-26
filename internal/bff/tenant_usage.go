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

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
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

// NewRedisTenantUsageReader connects (read-only) to the shared state-layer Valkey at addr.
func NewRedisTenantUsageReader(addr string) TenantUsageReader {
	return &redisTenantUsage{rdb: redis.NewClient(&redis.Options{Addr: addr})}
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
