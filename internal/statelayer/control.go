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

package statelayer

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// ControlStore reads a run's CONTROL marker from the credentialed Valkey (m70.8, the real-kill cancel
// channel). The trusted BFF WRITES `run:{namespace}:{runID}:control=<verb>` on cancel
// (internal/bff/run_control.go);
// this store READS it back for the pod-authed /control endpoint so the launcher gateway can abort the
// agent's in-flight model call. Read-only here — the proxy never writes control markers (the BFF is the
// sole writer). An interface so the handler tests use a fake without a live Valkey.
type ControlStore interface {
	// Control returns the FIRST control verb set on any scope covering this run — the run itself, its
	// agent, its namespace, its tenant, or the fleet — or "" when none is set (the common case). Every
	// field of the scope is supplied by the CALLER from the authenticated pod identity, never from the
	// request: that is what makes a cross-namespace or cross-tenant read structurally impossible rather
	// than merely refused (M142.3 C15, extended to the whole hierarchy in M146/ADR 0126).
	Control(ctx context.Context, scope ControlScope) (string, error)
}

// ControlScope is the set of scopes a single run sits under. The proxy derives every field from the
// verified pod token — namespace and agent from the agent-<name> SA, tenant from the namespace — so a
// caller can never widen its own scope by naming someone else's.
//
// Agent and Tenant are "" when the proxy could not derive them (no tenant resolver, an untenanted
// namespace). A missing scope is SKIPPED, not guessed: this is the fail-OPEN accelerator layer (ADR 0126
// §2), and the fail-CLOSED layers that actually enforce a kill read the control plane instead.
type ControlScope struct {
	Namespace string
	Agent     string
	Tenant    string
	RunID     string
}

// scopeKeys returns the marker keys covering this run, CHEAPEST-FIRST — the most specific scope first so
// a per-run cancel still wins the ordering it always had, then progressively wider blast radii.
//
// The keys are read in ONE round trip (MGET) and scanned in this order, so adding scopes costs no extra
// latency on the hot path — the launcher polls this every controlPollInterval while a call is in flight.
func scopeKeys(sc ControlScope) []string {
	keys := []string{controlKey(sc.Namespace, sc.RunID)}
	if sc.Agent != "" {
		keys = append(keys, "agent:"+sc.Namespace+":"+sc.Agent+":control")
	}
	keys = append(keys, "ns:"+sc.Namespace+":control")
	if sc.Tenant != "" {
		keys = append(keys, "tenant:"+sc.Tenant+":control")
	}
	return append(keys, "fleet:control")
}

// controlKey is the EXACT marker key the BFF writes (internal/bff/run_control.go runControlKey):
// `run:{namespace}:{runID}:control`. Duplicated here (a tiny, stable format) so the read side of the
// cross-PACKAGE contract is explicit and greppable; the two must never drift.
func controlKey(namespace, runID string) string {
	return "run:" + namespace + ":" + runID + ":control"
}

// redisControlStore is the production ControlStore over the proxy's credentialed Valkey.
type redisControlStore struct{ rdb *redis.Client }

// NewRedisControlStore builds a ControlStore over the state-layer Valkey (the proxy's credentialed
// connection), mirroring NewRedisQuotaStore. Empty password ⇒ unauthenticated (dev / a Valkey with no
// requirepass).
func NewRedisControlStore(addr, username, password string) ControlStore {
	return &redisControlStore{rdb: redis.NewClient(&redis.Options{
		Addr:         addr,
		Username:     username,
		Password:     password,
		DialTimeout:  memoryOpTimeout,
		ReadTimeout:  memoryOpTimeout,
		WriteTimeout: memoryOpTimeout,
	})}
}

func (s *redisControlStore) Control(ctx context.Context, scope ControlScope) (string, error) {
	keys := scopeKeys(scope)
	// ONE round trip for the whole hierarchy. MGET returns a slice positionally aligned with keys, so
	// scanning it in order preserves cheapest-first precedence without N sequential GETs on a path the
	// launcher polls once a second per in-flight call.
	vals, err := s.rdb.MGet(ctx, keys...).Result()
	if err == redis.Nil {
		return "", nil // no marker on any scope ⇒ no control verb
	}
	if err != nil {
		return "", err
	}
	for _, v := range vals {
		// A missing key is nil; a set key is a string. Anything else is a malformed value we ignore
		// rather than surface as a verb — an unparseable marker must not read as a kill.
		if str, ok := v.(string); ok && str != "" {
			return str, nil
		}
	}
	return "", nil
}
