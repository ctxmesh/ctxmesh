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

package dataset

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

// eachStore holds the in-memory twin and the Postgres store to ONE behavioural contract (the promptversion /
// knowledge conformance pattern). The twin always runs; the Postgres store runs only when CONTROLPLANE_TEST_DSN
// points at a throwaway DB (migrated + truncated first) — CI without a DB still exercises the contract via the
// twin. The truncate order respects the FK chain (children before parents; CASCADE covers the rest).
func eachStore(t *testing.T, fn func(t *testing.T, s Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, NewMemStore()) })

	dsn := os.Getenv("CONTROLPLANE_TEST_DSN")
	if dsn == "" {
		t.Log("CONTROLPLANE_TEST_DSN unset — skipping the Postgres conformance run (the twin still ran)")
		return
	}
	db, err := controlplane.OpenDB(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`TRUNCATE datasets, dataset_cases, dataset_labels, dataset_versions, dataset_version_cases CASCADE`)
	require.NoError(t, err)
	t.Run("postgres", func(t *testing.T) { fn(t, NewPostgresStore(db)) })
}

func TestEnsureDataset_Idempotent(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		d1, err := s.EnsureDataset(ctx, "default", "support-qa")
		require.NoError(t, err)
		assert.NotEmpty(t, d1.ID)
		assert.Equal(t, "default", d1.Namespace)
		assert.Equal(t, "support-qa", d1.Name)
		assert.False(t, d1.CreatedAt.IsZero())

		// A second Ensure returns the SAME dataset (same ID) — idempotent, not a new row.
		d2, err := s.EnsureDataset(ctx, "default", "support-qa")
		require.NoError(t, err)
		assert.Equal(t, d1.ID, d2.ID)
	})
}

func TestEnsureDataset_Invalid(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		_, err := s.EnsureDataset(ctx, "", "x")
		assert.ErrorIs(t, err, controlplane.ErrInvalid)
		_, err = s.EnsureDataset(ctx, "default", "  ")
		assert.ErrorIs(t, err, controlplane.ErrInvalid)
	})
}

func TestAppendCase_Validation(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		d, err := s.EnsureDataset(ctx, "default", "cases")
		require.NoError(t, err)

		// Blank input → ErrInvalid.
		_, err = s.AppendCase(ctx, d.ID, Case{Input: "  "})
		assert.ErrorIs(t, err, controlplane.ErrInvalid)

		// Unknown dataset → ErrNotFound.
		_, err = s.AppendCase(ctx, "does-not-exist", Case{Input: "hi"})
		assert.ErrorIs(t, err, controlplane.ErrNotFound)

		// A valid case appends into the draft head and is listed.
		id, err := s.AppendCase(ctx, d.ID, Case{
			Input: "q1", Expected: "a1", SourceTraceID: "trace-1", MimeType: "text/plain",
			Tags: map[string]string{"topic": "billing"},
		})
		require.NoError(t, err)
		assert.NotEmpty(t, id)

		cases, err := s.ListCases(ctx, d.ID)
		require.NoError(t, err)
		require.Len(t, cases, 1)
		assert.Equal(t, "q1", cases[0].Input)
		assert.Equal(t, "a1", cases[0].Expected)
		assert.Equal(t, "trace-1", cases[0].SourceTraceID)
		assert.Equal(t, "billing", cases[0].Tags["topic"])
	})
}

func TestListCases_UnknownDataset(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		_, err := s.ListCases(context.Background(), "nope")
		assert.ErrorIs(t, err, controlplane.ErrNotFound)
	})
}

func TestAppendLabel_UnknownCase(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		err := s.AppendLabel(context.Background(), "no-such-case", Label{Value: "pass"})
		assert.ErrorIs(t, err, controlplane.ErrNotFound)
	})
}

func TestPinVersion_EmptyDataset(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		d, err := s.EnsureDataset(ctx, "default", "empty")
		require.NoError(t, err)
		_, err = s.PinVersion(ctx, d.ID)
		assert.ErrorIs(t, err, controlplane.ErrInvalid, "an empty pinned version can't gate anything")

		_, err = s.PinVersion(ctx, "no-such-dataset")
		assert.ErrorIs(t, err, controlplane.ErrNotFound)
	})
}

func TestResolveRef_UnpinnedAndMissing(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		d, err := s.EnsureDataset(ctx, "default", "unpinned")
		require.NoError(t, err)
		_, err = s.AppendCase(ctx, d.ID, Case{Input: "q"})
		require.NoError(t, err)

		// A bare name with NO pinned version yet → ErrInvalid (can't gate reproducibly).
		_, _, err = s.ResolveRef(ctx, "default", "unpinned")
		assert.ErrorIs(t, err, controlplane.ErrInvalid)

		// A missing dataset → ErrNotFound.
		_, _, err = s.ResolveRef(ctx, "default", "ghost")
		assert.ErrorIs(t, err, controlplane.ErrNotFound)

		// A malformed ref → ErrInvalid.
		for _, bad := range []string{"", "name@", "name@0", "name@-1", "name@x", "name@1@2", "@1"} {
			_, _, err := s.ResolveRef(ctx, "default", bad)
			assert.ErrorIs(t, err, controlplane.ErrInvalid, "ref %q must be invalid", bad)
		}

		// A pin@version that doesn't exist → ErrNotFound.
		_, err = s.PinVersion(ctx, d.ID)
		require.NoError(t, err)
		_, _, err = s.ResolveRef(ctx, "default", "unpinned@99")
		assert.ErrorIs(t, err, controlplane.ErrNotFound)
	})
}

// TestPinImmutability is the load-bearing invariant (ADR 0062 Fork 1): a pinned version resolves IMMUTABLY — the
// same case set + the same per-case label state every time, even as the draft head keeps growing with new cases
// and new labels. This is the property the eval-gate depends on to compare "0.82 on rev A vs 0.79 on rev B".
func TestPinImmutability(t *testing.T) {
	eachStore(t, func(t *testing.T, s Store) {
		ctx := context.Background()
		d, err := s.EnsureDataset(ctx, "default", "immutable")
		require.NoError(t, err)

		// Two cases + a label on case A, then pin v1.
		caseA, err := s.AppendCase(ctx, d.ID, Case{Input: "qA", Expected: "eA"})
		require.NoError(t, err)
		caseB, err := s.AppendCase(ctx, d.ID, Case{Input: "qB", Expected: "eB"})
		require.NoError(t, err)
		require.NoError(t, s.AppendLabel(ctx, caseA, Label{Value: "pass", Author: "alice"}))

		v1, err := s.PinVersion(ctx, d.ID)
		require.NoError(t, err)
		assert.Equal(t, 1, v1)

		// Snapshot v1 right after the pin.
		v1cases, err := s.ResolveVersion(ctx, "default", "immutable", 1)
		require.NoError(t, err)
		require.Len(t, v1cases, 2)
		byCaseID := func(cs []ResolvedCase) map[string]ResolvedCase {
			out := map[string]ResolvedCase{}
			for _, c := range cs {
				out[c.CaseID] = c
			}
			return out
		}
		v1m := byCaseID(v1cases)
		require.Contains(t, v1m, caseA)
		require.Contains(t, v1m, caseB)
		assert.True(t, v1m[caseA].HasLabel)
		assert.Equal(t, "pass", v1m[caseA].LabelValue)
		assert.Equal(t, "alice", v1m[caseA].LabelAuthor)
		assert.False(t, v1m[caseB].HasLabel, "case B was unlabeled at pin time")

		// NOW mutate the draft head AFTER the pin: append MORE labels to case A (a re-judgment), a label to case
		// B, and a whole new case C.
		require.NoError(t, s.AppendLabel(ctx, caseA, Label{Value: "fail", Author: "bob"}))
		require.NoError(t, s.AppendLabel(ctx, caseB, Label{Value: "pass", Author: "carol"}))
		caseC, err := s.AppendCase(ctx, d.ID, Case{Input: "qC", Expected: "eC"})
		require.NoError(t, err)

		// v1 STILL resolves to the ORIGINAL cases + the ORIGINAL label state — the appends did not move it.
		v1again, err := s.ResolveVersion(ctx, "default", "immutable", 1)
		require.NoError(t, err)
		require.Len(t, v1again, 2, "v1 must NOT include case C added after the pin")
		v1m2 := byCaseID(v1again)
		assert.Equal(t, "pass", v1m2[caseA].LabelValue, "v1's frozen label for case A must remain 'pass', not the later 'fail'")
		assert.Equal(t, "alice", v1m2[caseA].LabelAuthor)
		assert.False(t, v1m2[caseB].HasLabel, "case B's later label must NOT appear in the frozen v1")
		assert.NotContains(t, v1m2, caseC)

		// A bare ref returns the latest pinned (still v1) until a v2 pin.
		bareCases, bareVer, err := s.ResolveRef(ctx, "default", "immutable")
		require.NoError(t, err)
		assert.Equal(t, 1, bareVer)
		assert.Len(t, bareCases, 2)

		// A pinned ref name@1 is stable and equals ResolveVersion(1).
		refCases, refVer, err := s.ResolveRef(ctx, "default", "immutable@1")
		require.NoError(t, err)
		assert.Equal(t, 1, refVer)
		assert.Len(t, refCases, 2)

		// Pin v2: now it captures case C + case A's/B's LATEST labels.
		v2, err := s.PinVersion(ctx, d.ID)
		require.NoError(t, err)
		assert.Equal(t, 2, v2)

		v2cases, err := s.ResolveVersion(ctx, "default", "immutable", 2)
		require.NoError(t, err)
		require.Len(t, v2cases, 3, "v2 includes case C")
		v2m := byCaseID(v2cases)
		assert.Equal(t, "fail", v2m[caseA].LabelValue, "v2 freezes case A's latest label 'fail'")
		assert.Equal(t, "bob", v2m[caseA].LabelAuthor)
		assert.True(t, v2m[caseB].HasLabel)
		assert.Equal(t, "pass", v2m[caseB].LabelValue)
		require.Contains(t, v2m, caseC)

		// v1 is STILL v1 — pinning v2 did not disturb it.
		v1final, err := s.ResolveVersion(ctx, "default", "immutable", 1)
		require.NoError(t, err)
		require.Len(t, v1final, 2)
		assert.Equal(t, "pass", byCaseID(v1final)[caseA].LabelValue)

		// A bare ref now follows the latest pinned → v2.
		_, bareVer2, err := s.ResolveRef(ctx, "default", "immutable")
		require.NoError(t, err)
		assert.Equal(t, 2, bareVer2)
	})
}
