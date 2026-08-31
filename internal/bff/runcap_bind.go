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
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ctxmesh/agentry/internal/runcap"
)

// Binding a run capability to its holder's key (M142.5, ADR 0124).
//
// The BFF mints a capability before the pod that will use it has generated a key, so it cannot bind at
// mint time. Instead the launcher EXCHANGES the bearer capability for a bound one, presenting its public
// key. Everything after that requires proof-of-possession, so a copy of the token is worthless.
//
// The exchange is SINGLE-USE, and that is the whole security argument. Anyone holding the bearer token
// could bind it to their own key — so the first bind wins and every later one is refused. The legitimate
// launcher binds on receipt, in-cluster, in milliseconds; an attacker who obtains the token afterwards
// finds it already bound to a key they do not have. The exposure window shrinks from "the capability's
// whole life" to "before its owner first used it".
//
// The record lives in the shared state layer with a TTL, not in Postgres and not in replica memory:
//   - replica memory would make "already bound" answer differently on each BFF, which is not a boundary;
//   - Postgres would mean a schema migration for a fact that stops mattering in five minutes.
//
// A TTL slightly beyond the capability's own lifetime is exactly right: once the capability expires the
// binding is moot, so remembering it longer only grows the store.

// runcapBindTTL outlives runCapabilityTTL by a margin for clock skew, then lets the record vanish.
const runcapBindTTL = runCapabilityTTL + time.Minute

// RuncapBindStore records which key a run's capability was bound to. Set is atomic and single-use:
// it returns ok=false when the run is already bound, which is what makes the first bind authoritative.
type RuncapBindStore interface {
	// Bind records jkt for runID iff no binding exists. ok=false ⇒ already bound (by whom is returned,
	// so a legitimate re-bind of the SAME key is idempotent rather than an error).
	Bind(ctx context.Context, runID, jkt string) (existing string, ok bool, err error)
}

// redisRuncapBindStore is the production store over the shared state-layer Valkey.
type redisRuncapBindStore struct{ rdb *redis.Client }

// NewRedisRuncapBindStore builds a bind store over the state-layer Valkey, mirroring
// NewRedisRunControlPublisher. Empty password ⇒ unauthenticated (dev).
func NewRedisRuncapBindStore(addr, username, password string) RuncapBindStore {
	return &redisRuncapBindStore{rdb: redis.NewClient(&redis.Options{
		Addr: addr, Username: username, Password: password,
		DialTimeout: usageOpTimeout, ReadTimeout: usageOpTimeout, WriteTimeout: usageOpTimeout,
	})}
}

func runcapBindKey(runID string) string { return "runcap:bind:" + runID }

func (s *redisRuncapBindStore) Bind(ctx context.Context, runID, jkt string) (string, bool, error) {
	// SETNX is the single-use primitive: exactly one caller wins, whichever replica it reached.
	ok, err := s.rdb.SetNX(ctx, runcapBindKey(runID), jkt, runcapBindTTL).Result()
	if err != nil {
		return "", false, err
	}
	if ok {
		return jkt, true, nil
	}
	existing, err := s.rdb.Get(ctx, runcapBindKey(runID)).Result()
	if err != nil {
		return "", false, err
	}
	return existing, false, nil
}

// BindRuncapRequest presents the caller's public-key thumbprint.
type BindRuncapRequest struct {
	JKT string `json:"jkt"`
}

// BindRuncapResponse returns the sender-constrained capability to use from now on.
type BindRuncapResponse struct {
	Capability string `json:"capability"`
}

// registerRuncapBindRoute wires the exchange onto the unauthenticated api mux — it authenticates on the
// relayed capability itself, like the other internal edges. Absent without a signer or a bind store,
// so an install that cannot record bindings does not offer an exchange it could not make single-use.
func (s *Server) registerRuncapBindRoute(api *http.ServeMux) {
	if s.capabilitySigner != nil && s.runcapBind != nil {
		api.HandleFunc("POST /api/internal/runcap/bind", s.handleBindRuncap)
	}
}

// handleBindRuncap exchanges a bearer run capability for one bound to the caller's key.
//
// It deliberately does NOT require a proof: the caller cannot produce one yet, because nothing is bound.
// That is the one edge where possession is still authority, which is precisely why the binding is
// single-use — the window is one exchange, not the capability's lifetime.
func (s *Server) handleBindRuncap(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.Header.Get(runcap.HeaderName))
	if token == "" {
		writeError(w, http.StatusUnauthorized, "missing run capability")
		return
	}
	capab, err := s.capabilitySigner.Verifier().Verify(token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid run capability")
		return
	}
	if capab.KeyThumbprint != "" {
		// Already sender-constrained: re-binding would be a downgrade path (bind again with a new key).
		writeError(w, http.StatusConflict, "this capability is already bound to a key")
		return
	}

	var req BindRuncapRequest
	if decErr := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); decErr != nil {
		writeError(w, http.StatusBadRequest, "invalid bind request body")
		return
	}
	jkt := strings.TrimSpace(req.JKT)
	if jkt == "" {
		writeError(w, http.StatusBadRequest, "jkt is required — it is what the capability binds to")
		return
	}

	existing, ok, err := s.runcapBind.Bind(r.Context(), capab.RunID, jkt)
	if err != nil {
		s.log.Error(err, "runcap bind: the bind store is unavailable", "run", capab.RunID)
		writeError(w, http.StatusBadGateway, "the capability bind store is unavailable")
		return
	}
	if !ok && existing != jkt {
		// Someone else bound this run first. Refuse rather than issue a second constrained capability —
		// two live bindings would mean two holders, which is the property this exists to prevent.
		s.log.Info("runcap bind REFUSED: this run's capability is already bound to another key",
			"run", capab.RunID, "agent", capab.Agent)
		writeError(w, http.StatusConflict, "this run's capability is already bound to another key")
		return
	}
	// existing == jkt ⇒ the same holder retrying (a restart, a retry after a timeout). Idempotent.

	remaining := time.Until(capab.ExpiresAt)
	if remaining <= 0 {
		writeError(w, http.StatusUnauthorized, "the run capability has expired")
		return
	}
	bound, err := s.capabilitySigner.Mint(runcap.MintRequest{
		User: capab.User, Agent: capab.Agent, Boundary: capab.Boundary, RunID: capab.RunID,
		// The bound capability inherits the ORIGINAL's remaining life; binding must not extend a
		// capability's reach in time, only narrow who can spend it.
		TTL:           remaining,
		KeyThumbprint: jkt,
	})
	if err != nil {
		s.log.Error(err, "runcap bind: minting the bound capability failed", "run", capab.RunID)
		writeError(w, http.StatusInternalServerError, "could not mint a bound capability")
		return
	}
	writeJSON(w, http.StatusOK, BindRuncapResponse{Capability: bound})
}
