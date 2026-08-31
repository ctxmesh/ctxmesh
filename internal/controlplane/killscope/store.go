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

// Package killscope is the AUTHORITATIVE record of active emergency stops (M146,
// [ADR 0126](../../../decisions/0126-scoped-kill-switch.md)).
//
// The state-layer marker that interrupts an in-flight model call is deliberately an ACCELERATOR and
// deliberately FAILS OPEN (ADR 0063 §D3): a control-plane blip must never spuriously kill live work.
// That posture is right for cancelling one run and backwards for an emergency stop, which is needed
// exactly when the platform is degraded — a fail-open kill quietly un-kills itself during the incident
// that motivated it.
//
// So the two layers are split (ADR 0126 §2). This store is the half that FAILS CLOSED: the worker reads
// it to decide whether to claim a queued run, and the run-create edge reads it to decide whether to
// accept a run at all. Neither consults the state layer, so an unreachable Valkey cannot resurrect a
// killed scope. The marker is written alongside and only makes the stop land faster.
package killscope

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Level is the blast radius of a stop, from narrowest to widest. The zero value is invalid — a scope
// with no level would silently match nothing, which for a safety control is the wrong direction to fail.
type Level string

const (
	LevelAgent     Level = "agent"
	LevelNamespace Level = "namespace"
	LevelTenant    Level = "tenant"
	LevelFleet     Level = "fleet"
)

// ErrInvalidScope is returned for a scope whose level and identifiers disagree — an agent kill with no
// agent name, a fleet kill that also names a namespace. Rejected rather than normalised: a safety
// control must mean exactly what the operator wrote.
var ErrInvalidScope = errors.New("killscope: invalid scope")

// Scope identifies what is stopped. Exactly the fields its Level requires must be set.
type Scope struct {
	Level     Level
	Namespace string // agent, namespace
	Agent     string // agent
	Tenant    string // tenant
}

// Validate rejects a scope whose identifiers do not match its level.
func (s Scope) Validate() error {
	switch s.Level {
	case LevelAgent:
		if s.Namespace == "" || s.Agent == "" {
			return fmt.Errorf("%w: agent scope needs namespace and agent", ErrInvalidScope)
		}
		if s.Tenant != "" {
			return fmt.Errorf("%w: agent scope must not name a tenant", ErrInvalidScope)
		}
	case LevelNamespace:
		if s.Namespace == "" || s.Agent != "" || s.Tenant != "" {
			return fmt.Errorf("%w: namespace scope needs a namespace and nothing else", ErrInvalidScope)
		}
	case LevelTenant:
		if s.Tenant == "" || s.Namespace != "" || s.Agent != "" {
			return fmt.Errorf("%w: tenant scope needs a tenant and nothing else", ErrInvalidScope)
		}
	case LevelFleet:
		if s.Namespace != "" || s.Agent != "" || s.Tenant != "" {
			return fmt.Errorf("%w: fleet scope names nothing", ErrInvalidScope)
		}
	default:
		return fmt.Errorf("%w: unknown level %q", ErrInvalidScope, s.Level)
	}
	return nil
}

// Key is the stable identity of a scope — one active kill per scope, so re-killing an already-killed
// scope is idempotent rather than creating a second row someone must remember to lift twice.
func (s Scope) Key() string {
	switch s.Level {
	case LevelAgent:
		return "agent:" + s.Namespace + ":" + s.Agent
	case LevelNamespace:
		return "namespace:" + s.Namespace
	case LevelTenant:
		return "tenant:" + s.Tenant
	default:
		return "fleet"
	}
}

// MarkerKey is the state-layer key the accelerator marker is written to for this scope. It MUST match
// internal/statelayer.scopeKeys byte for byte — the two sides of a cross-package contract, kept
// greppable from both ends (the same discipline the run-control key already uses).
func (s Scope) MarkerKey() string {
	switch s.Level {
	case LevelAgent:
		return "agent:" + s.Namespace + ":" + s.Agent + ":control"
	case LevelNamespace:
		return "ns:" + s.Namespace + ":control"
	case LevelTenant:
		return "tenant:" + s.Tenant + ":control"
	default:
		return "fleet:control"
	}
}

// Kill is an active stop plus the provenance a destructive control must carry.
type Kill struct {
	Scope Scope
	// Reason is the operator's free text. Required — an unexplained fleet stop found at 3am is nearly
	// as bad as no stop at all.
	Reason string
	// Principal is who pulled it, recorded for the audit trail and shown in the console banner.
	Principal string
}

// Store records active kills. Reads are on the run-claim and run-create hot paths, so implementations
// are expected to be cheap (the common case is an empty set) and the caller caches briefly.
type Store interface {
	// Kill records a stop. Idempotent per Scope.Key(): re-killing an active scope refreshes the reason
	// and principal rather than stacking rows.
	Kill(ctx context.Context, k Kill) error
	// Unkill lifts a stop. Returns false when the scope was not killed, so the caller can report an
	// honest no-op instead of a success that did nothing.
	Unkill(ctx context.Context, s Scope) (bool, error)
	// Active lists every live kill. Callers expand it into a ClaimFilter; the set is normally empty.
	Active(ctx context.Context) ([]Kill, error)
}

// ParseLevel maps wire text to a Level, rejecting anything unknown rather than defaulting — a typo'd
// level must not silently become a fleet stop, nor silently match nothing.
func ParseLevel(s string) (Level, error) {
	switch Level(strings.ToLower(strings.TrimSpace(s))) {
	case LevelAgent:
		return LevelAgent, nil
	case LevelNamespace:
		return LevelNamespace, nil
	case LevelTenant:
		return LevelTenant, nil
	case LevelFleet:
		return LevelFleet, nil
	default:
		return "", fmt.Errorf("%w: unknown level %q", ErrInvalidScope, s)
	}
}
