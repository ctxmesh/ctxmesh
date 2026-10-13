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

// Package deliveryclaim is the idempotency record an async AMP consumer keeps per message id in Valkey.
//
// A delivery first claims its message id. The claim is held while the agent processes the message, so a
// concurrent duplicate is not processed twice; it is replaced by a done marker once the agent succeeded,
// so a later redelivery is a duplicate; and it is released when the agent failed, so the broker's retry
// is processed instead of being taken for a duplicate. A claim expires on its own, which is what lets a
// message through again after a consumer died holding it.
//
// Both the launcher's direct Valkey path and the state-layer proxy keep the record, so the encoding lives
// here once.
package deliveryclaim

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// State is what a claim found.
type State string

const (
	// Claimed means this delivery holds the claim: process the message, then Complete or Release it.
	Claimed State = "claimed"
	// Done means a delivery of this message id succeeded: this one is a duplicate.
	Done State = "done"
	// InFlight means another delivery holds the claim: this one must be retried later, not acknowledged,
	// or a claim left by a consumer that died would lose the message.
	InFlight State = "inflight"
)

const (
	// doneMarker is the value of a processed id. It is the value the seen-set wrote before claims existed,
	// so an id recorded by an older consumer still reads as processed.
	doneMarker = "1"
	// claimPrefix marks a held claim; the rest of the value is the holder's token.
	claimPrefix = "claim:"
)

// claimScript sets the claim unless the key exists, and returns the existing value when it does, in one
// atomic step: a GET then a SET would let two deliveries both claim.
var claimScript = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if cur then return cur end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
return false
`)

// releaseScript deletes the claim only while it is still the caller's. A claim that expired and was taken
// by another delivery is that delivery's to keep or release.
var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

// Claim claims key for the delivery identified by token, for ttl. ttl must outlast the processing the
// claim guards, or a duplicate can claim the message while it is still being processed.
func Claim(ctx context.Context, rdb redis.Scripter, key, token string, ttl time.Duration) (State, error) {
	cur, err := claimScript.Run(ctx, rdb, []string{key}, claimPrefix+token, ttl.Milliseconds()).Text()
	switch {
	case errors.Is(err, redis.Nil):
		return Claimed, nil
	case err != nil:
		return "", err
	case strings.HasPrefix(cur, claimPrefix):
		return InFlight, nil
	default:
		return Done, nil
	}
}

// Complete records key as processed for ttl, replacing the claim.
func Complete(ctx context.Context, rdb redis.Cmdable, key string, ttl time.Duration) error {
	return rdb.Set(ctx, key, doneMarker, ttl).Err()
}

// Release drops the claim token holds on key, so the next delivery of the message is processed.
func Release(ctx context.Context, rdb redis.Scripter, key, token string) error {
	return releaseScript.Run(ctx, rdb, []string{key}, claimPrefix+token).Err()
}
