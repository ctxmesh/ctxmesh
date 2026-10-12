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

// Package agentversion is the control-plane store for agent version history (ADR 0042).
//
// A version is the snapshot of an AgentDeployment's spec that one served revision was built from,
// named "<agent>-<spec-hash-8>". The controller records one whenever the spec hash changes; a
// rollback reverts the spec to a recorded snapshot; the regression detector compares a version
// against the one recorded before it; the console lists and diffs them.
//
// History belongs to one incarnation of an agent (its UID): a deleted and recreated agent of the
// same name starts empty. Order is insertion order (Seq). Recording a version that already exists
// keeps its original Seq, so an agent that goes A, B, A has A before B.
package agentversion

import (
	"context"
	"fmt"
	"time"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane"
)

// Source says who recorded a version.
type Source string

const (
	// SourceController is a version the AgentDeployment reconciler recorded.
	SourceController Source = "controller"
	// SourceImport is a version copied from an AgentVersion object when an older install upgraded.
	SourceImport Source = "import"
)

// Version is one recorded snapshot.
type Version struct {
	Namespace string
	Agent     string
	AgentUID  string
	Name      string
	// Seq orders an agent's versions; assigned by the store.
	Seq int64
	// Snapshot is the versioned part of the spec (governance fields a rollback keeps are omitted
	// by the caller before recording).
	Snapshot  agentsv1beta1.AgentDeploymentSpec
	Source    Source
	CreatedAt time.Time
}

// Entry names one recorded version without its snapshot.
type Entry struct {
	Name      string
	Seq       int64
	Source    Source
	CreatedAt time.Time
}

// Owner is an agent incarnation that has recorded a version of a given name.
type Owner struct {
	Agent    string
	AgentUID string
}

// ImportResult reports a one-time import.
type ImportResult struct {
	// Ran is false when an earlier import already completed; nothing was loaded or written.
	Ran      bool
	Imported int
	Skipped  int
}

// Store persists version history. Two implementations, Postgres and an in-memory twin, pass one
// conformance suite.
type Store interface {
	// Record inserts v unless (Namespace, AgentUID, Name) is already recorded; inserted reports
	// which. An existing version is never modified, and keeps its Seq.
	Record(ctx context.Context, v Version) (inserted bool, err error)

	// Get returns one version of an agent incarnation.
	Get(ctx context.Context, namespace, agentUID, name string) (Version, bool, error)

	// Owners returns every agent incarnation in the namespace with a version of this name.
	Owners(ctx context.Context, namespace, name string) ([]Owner, error)

	// List returns an agent incarnation's versions, newest first.
	List(ctx context.Context, namespace, agentUID string) ([]Entry, error)

	// Prior returns the version recorded immediately before name. ok is false when name is not
	// recorded or is the oldest.
	Prior(ctx context.Context, namespace, agentUID, name string) (prior string, ok bool, err error)

	// Prune deletes the rows of earlier incarnations of the named agent, and those of agentUID
	// that are neither among its keep newest nor named in pinned. It returns the rows deleted.
	Prune(ctx context.Context, namespace, agent, agentUID string, keep int, pinned []string) (int, error)

	// DeleteAgent deletes every row of the named agent, all incarnations.
	DeleteAgent(ctx context.Context, namespace, agent string) error

	// ImportOnce runs load and records what it returns, in order, as SourceImport, unless an
	// import has already completed. It holds a lock across replicas while it runs, and the rows
	// and the completion marker are written together: an error from load or from a write leaves
	// no marker and no rows, so the next start retries.
	ImportOnce(ctx context.Context, load func(context.Context) (versions []Version, skipped int, err error)) (ImportResult, error)
}

// validate checks the identity a caller supplies.
func validate(v Version) error {
	switch {
	case v.Namespace == "":
		return fmt.Errorf("%w: agent version: namespace is required", controlplane.ErrInvalid)
	case v.Agent == "":
		return fmt.Errorf("%w: agent version: agent is required", controlplane.ErrInvalid)
	case v.AgentUID == "":
		return fmt.Errorf("%w: agent version: agent uid is required", controlplane.ErrInvalid)
	case v.Name == "":
		return fmt.Errorf("%w: agent version: name is required", controlplane.ErrInvalid)
	}
	return nil
}

// sourceOrController defaults an empty Source.
func sourceOrController(s Source) Source {
	if s == "" {
		return SourceController
	}
	return s
}
