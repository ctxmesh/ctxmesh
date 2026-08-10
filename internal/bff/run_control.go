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

	"github.com/redis/go-redis/v9"
)

// RunControlPublisher writes a run-scoped CONTROL marker to the shared state-layer Valkey (m70.8, the
// real-kill cancel channel). The BFF is the trusted control-plane writer: it holds STATELAYER_ADDR and
// SETs `run:{id}:control = <verb>` so the launcher gateway — reading it back through the pod-authed
// state-layer proxy — can abort the agent's in-flight model call. This is the ACCELERATOR on top of the
// durable status flip: the status transition is the authoritative cancel; the marker just makes the kill
// land at call-boundary granularity instead of waiting for the worker to notice a terminal run. An
// interface so the cancel handler can be unit-tested with a fake (no live Valkey).
type RunControlPublisher interface {
	// Publish SETs the control verb for a run with a bounded TTL. A failure is returned for the caller
	// to LOG (never fatal to the cancel response — the status flip already happened).
	Publish(ctx context.Context, runID, verb string) error
}

// controlVerbCancel is the ONLY control verb v1 handles (real-kill). Future verbs (nudge / take-over)
// are deliberately NOT implemented — the marker is a verb channel and an unknown verb is ignored by the
// launcher, so adding a verb here never breaks an older launcher.
const controlVerbCancel = "cancel"

// controlMarkerTTL bounds the control marker's lifetime. It matches runCapabilityTTL (invoke.go): the
// agent's model calls run under a capability that lives ~5 minutes, so a marker that outlives that window
// can no longer accelerate anything — it is harmless but must not linger forever. The durable run status
// is the authoritative cancel; this TTL only bounds the accelerator key.
const controlMarkerTTL = runCapabilityTTL

// controlPublishTimeout bounds the single Valkey SET so a slow/unreachable state-layer cannot stall the
// cancel response — the publish is best-effort (the status flip is already committed). Mirrors the
// tenant-usage reader's bounded op timeout (usageOpTimeout).
const controlPublishTimeout = usageOpTimeout

// runControlKey is the EXACT marker key the launcher's control-store reads back through the proxy:
// `run:{runID}:control`. Kept in ONE place so the write site and the proxy read site cannot drift.
func runControlKey(runID string) string { return "run:" + runID + ":control" }

type redisRunControlPublisher struct{ rdb *redis.Client }

// NewRedisRunControlPublisher connects to the shared state-layer Valkey at addr and writes control
// markers. No password by design — the in-cluster Valkey is unauthenticated (ADR 0049); this mirrors the
// BFF's tenant-usage reader (NewRedisTenantUsageReader), which reads the same store. addr comes from
// STATELAYER_ADDR (the trusted control-plane already holds it for the usage reader).
func NewRedisRunControlPublisher(addr string) RunControlPublisher {
	return &redisRunControlPublisher{rdb: redis.NewClient(&redis.Options{
		Addr:        addr,
		DialTimeout: controlPublishTimeout,
		ReadTimeout: controlPublishTimeout,
	})}
}

func (p *redisRunControlPublisher) Publish(ctx context.Context, runID, verb string) error {
	ctx, cancel := context.WithTimeout(ctx, controlPublishTimeout)
	defer cancel()
	return p.rdb.Set(ctx, runControlKey(runID), verb, controlMarkerTTL).Err()
}

// publishCancelMarker writes the cancel verb for a run AFTER a successful status flip, best-effort. A nil
// publisher (STATELAYER_ADDR unset — a memory-only / dev deployment) or a Valkey failure degrades to
// today's soft cancel (the durable status flip already happened): the marker is only the accelerator, so
// its absence never breaks the cancel — it just means the agent pod notices via the status, not the abort.
func (s *Server) publishCancelMarker(ctx context.Context, runID string) {
	if s.runControl == nil {
		s.log.V(1).Info("run cancel: no control publisher (STATELAYER_ADDR unset) — soft cancel only", "run", runID)
		return
	}
	if err := s.runControl.Publish(ctx, runID, controlVerbCancel); err != nil {
		// Best-effort: the status flip is the authoritative cancel. Log and move on — never fail the
		// cancel response on a marker-publish blip.
		s.log.Error(err, "run cancel: publishing the control marker failed (soft cancel still applied)", "run", runID)
	}
}

// interface assertion.
var _ RunControlPublisher = (*redisRunControlPublisher)(nil)
