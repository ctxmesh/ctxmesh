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
	// Control returns the run's control verb, or "" when no marker is set (the common, no-cancel case).
	// The namespace is supplied by the CALLER from the authenticated pod identity, never from the
	// request — it is what scopes the read (M142.3, C15).
	Control(ctx context.Context, namespace, runID string) (string, error)
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

func (s *redisControlStore) Control(ctx context.Context, namespace, runID string) (string, error) {
	v, err := s.rdb.Get(ctx, controlKey(namespace, runID)).Result()
	if err == redis.Nil {
		return "", nil // no marker ⇒ no control verb (the run is not cancelled)
	}
	if err != nil {
		return "", err
	}
	return v, nil
}
