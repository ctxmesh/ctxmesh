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

package run

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Several runs share a trace: a workflow's nodes and a delegation's sub-runs carry their parent's, and a
// handoff starts a new root on it. A trace id must resolve to the same run every time, on both stores,
// because readers of the trace are authorized through that run: the earliest root, whatever order the
// rows were written in.
func TestGetByTraceID_ResolvesASharedTraceToItsEarliestRoot(t *testing.T) {
	for name, s := range descendantStores(t) {
		t.Run(name, func(t *testing.T) {
			mk := func(id, parent string, at time.Time) {
				t.Helper()
				r := New(id, "team", "agent-"+id, nil, "", at)
				r.TraceID, r.ParentRunID = "tr-shared", parent
				require.NoError(t, s.Create(r))
			}
			// Written first and earliest, but a sub-run.
			mk("sub", "root-early", t0.Add(-time.Hour))
			// Two roots, the later one written first.
			mk("root-late", "", t0.Add(time.Minute))
			mk("root-early", "", t0)
			// A same-time root that sorts after root-early by id.
			mk("root-tie", "", t0)

			for range 20 {
				got, err := s.GetByTraceID("tr-shared")
				require.NoError(t, err)
				require.NotNil(t, got)
				assert.Equal(t, "root-early", got.ID)
			}

			none, err := s.GetByTraceID("tr-absent")
			require.NoError(t, err)
			assert.Nil(t, none)
		})
	}
}
