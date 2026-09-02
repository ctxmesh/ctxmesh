package bff

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// redisProofSpender is the cross-replica proof-replay set (M149 m149.4).
//
// The proof verifier's default seen-set is an in-process map: correct for one replica and
// silently wrong for several, because a proof spent on replica A stays unseen by replica B
// for the whole freshness window. ADR 0124 accepted that when the BFF ran single-pod; M148
// made multi-replica the production posture, so a captured proof could be replayed against
// any replica that had not seen it — and which replica a request reaches is precisely what
// a load balancer decides.
//
// It rides the SAME state-layer Valkey as the run-capability bind store beside it, and uses
// the same primitive: SETNX is atomic across processes, so exactly one caller claims a
// given proof id no matter which replica it reached. A read-then-write would lose the very
// race this exists to prevent.
type redisProofSpender struct{ rdb *redis.Client }

// NewRedisProofSpender builds a shared proof-replay set over the state-layer Valkey,
// mirroring NewRedisRuncapBindStore. Empty password ⇒ unauthenticated (dev).
func NewRedisProofSpender(addr, username, password string) runcap.ProofSpender {
	return &redisProofSpender{rdb: redis.NewClient(&redis.Options{
		Addr: addr, Username: username, Password: password,
		DialTimeout: usageOpTimeout, ReadTimeout: usageOpTimeout, WriteTimeout: usageOpTimeout,
	})}
}

func proofSpendKey(jti string) string { return "runcap:pop:jti:" + jti }

// Spend claims a proof id, or reports it already used.
//
// FAILS CLOSED. If the state layer cannot answer, this returns an error rather than
// allowing the proof — an unreachable replay set must not silently degrade into no replay
// protection at all. That is the same posture the budget quota and async dedup take
// (the Fable audit's SEC-1 analysis): the paths whose whole job is to refuse are the paths
// that must refuse when they cannot check.
func (s *redisProofSpender) Spend(ctx context.Context, jti string, ttl time.Duration) error {
	ok, err := s.rdb.SetNX(ctx, proofSpendKey(jti), "1", ttl).Result()
	if err != nil {
		return fmt.Errorf("proof replay set unavailable (failing closed): %w", err)
	}
	if !ok {
		return runcap.ErrProofReplayed
	}
	return nil
}
