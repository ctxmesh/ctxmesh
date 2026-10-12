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

package controller

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentversion"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/pgtest"
)

// The import runs before the controller starts, inside a 128Mi limit, on clusters whose old controller
// never deleted an AgentVersion. These tests import thousands of objects of the size a real one has
// and bound the live heap while they stream.

const (
	importTestObjects = 5000
	importTestAgents  = 40
	// importHeapBound is the most live heap the import may add. Holding every object at once
	// measured 180 MiB for these 5000; streaming a page at a time measured 10-15 MiB.
	importHeapBound = 24 << 20
)

// legacyPager serves AgentVersion objects page by page, building each page on request, so the test
// itself holds none of them. Names run in list order; creation times run the other way, so an import
// that kept list order would reverse every agent's history.
type legacyPager struct {
	n, agents int
	base      time.Time
	pages     int
	onPage    func()
}

func (p *legacyPager) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	d, ok := obj.(*agentsv1beta1.AgentDeployment)
	if !ok {
		return fmt.Errorf("unexpected get of %T", obj)
	}
	d.Name, d.Namespace, d.UID = key.Name, key.Namespace, types.UID("uid-"+key.Name)
	return nil
}

func (p *legacyPager) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if p.onPage != nil {
		p.onPage()
	}
	p.pages++
	lo := &client.ListOptions{}
	lo.ApplyOptions(opts)
	start := 0
	if lo.Continue != "" {
		var err error
		if start, err = strconv.Atoi(lo.Continue); err != nil {
			return err
		}
	}
	end := min(start+int(lo.Limit), p.n)
	ul, ok := list.(*unstructured.UnstructuredList)
	if !ok {
		return fmt.Errorf("unexpected list of %T", list)
	}
	for i := start; i < end; i++ {
		ul.Items = append(ul.Items, p.object(i))
	}
	if p.onPage != nil {
		p.onPage() // the whole page is live
	}
	if end < p.n {
		ul.SetContinue(strconv.Itoa(end))
	}
	return nil
}

func (p *legacyPager) agentOf(i int) string { return fmt.Sprintf("agent-%02d", i%p.agents) }

func (p *legacyPager) versionOf(i int) string { return fmt.Sprintf("%s-v%05d", p.agentOf(i), i) }

func (p *legacyPager) createdAt(i int) time.Time {
	return p.base.Add(time.Duration(p.n-i) * time.Second)
}

// object is a legacy AgentVersion of about 12.7 KiB serialized, the size measured on a real install.
func (p *legacyPager) object(i int) unstructured.Unstructured {
	env := make([]any, 0, 60)
	for e := range 60 {
		env = append(env, map[string]any{
			"name":  fmt.Sprintf("SETTING_%02d", e),
			"value": strings.Repeat(strconv.Itoa(e%10), 180),
		})
	}
	agent := p.agentOf(i)
	obj := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agents.ctxmesh.ai/v1beta1",
		"kind":       "AgentVersion",
		"spec": map[string]any{
			"deploymentName": agent,
			"snapshot":       map[string]any{"image": fmt.Sprintf("ghcr.io/acme/agent:%d", i), "env": env},
		},
	}}
	obj.SetName(p.versionOf(i))
	obj.SetNamespace("team-a")
	obj.SetCreationTimestamp(metav1.NewTime(p.createdAt(i)))
	obj.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "agents.ctxmesh.ai/v1beta1", Kind: kindAgentDeployment, Name: agent,
		UID: types.UID("uid-" + agent), Controller: ptr.To(true),
	}})
	return obj
}

// heapWatch records the largest live heap above a baseline, sampled after a collection.
type heapWatch struct{ base, peak uint64 }

func newHeapWatch() *heapWatch {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return &heapWatch{base: m.HeapAlloc}
}

func (h *heapWatch) sample() {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if m.HeapAlloc > h.base && m.HeapAlloc-h.base > h.peak {
		h.peak = m.HeapAlloc - h.base
	}
}

// countingStore runs an import without keeping anything, so the measurement is the import's own.
type countingStore struct {
	agentversion.Store
	emitted int
	onEmit  func()
}

func (c *countingStore) ImportOnce(ctx context.Context, load agentversion.ImportLoader) (agentversion.ImportResult, error) {
	skipped, err := load(ctx, func(agentversion.Version) error {
		c.emitted++
		if c.onEmit != nil {
			c.onEmit()
		}
		return nil
	})
	return agentversion.ImportResult{Ran: true, Imported: c.emitted, Skipped: skipped}, err
}

func TestImportAgentVersions_StreamsWithinAMemoryBound(t *testing.T) {
	pager := &legacyPager{n: importTestObjects, agents: importTestAgents, base: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h := newHeapWatch()
	pager.onPage = h.sample
	store := &countingStore{}
	store.onEmit = func() {
		if store.emitted%importPageSize == importPageSize-1 {
			h.sample()
		}
	}

	res, err := ImportAgentVersions(context.Background(), pager, store)
	require.NoError(t, err)
	h.sample()
	assert.Equal(t, importTestObjects, res.Imported)
	assert.Equal(t, importTestObjects/importPageSize, pager.pages)
	t.Logf("peak live heap above baseline: %.1f MiB for %d objects (bound %d MiB)",
		float64(h.peak)/(1<<20), importTestObjects, importHeapBound>>20)
	assert.Less(t, h.peak, uint64(importHeapBound), "the import must not hold every object at once")
}

// The same import into Postgres: the bound holds with the real store, and every agent's history
// comes out in creation order although the objects arrived in the opposite order.
func TestImportAgentVersions_PostgresStreamsInOrder(t *testing.T) {
	db := pgtest.Open(t, "controller_import")
	for _, table := range []string{"agent_versions", "agent_version_import"} {
		_, err := db.Exec(`CREATE TABLE IF NOT EXISTS ` + table + ` (LIKE public.` + table + ` INCLUDING ALL)`)
		require.NoError(t, err)
	}
	_, err := db.Exec(`TRUNCATE agent_versions, agent_version_import`)
	require.NoError(t, err)
	store := agentversion.NewPostgresStore(db)

	pager := &legacyPager{n: importTestObjects, agents: importTestAgents, base: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h := newHeapWatch()
	pager.onPage = h.sample
	start := time.Now()
	res, err := ImportAgentVersions(context.Background(), pager, store)
	require.NoError(t, err)
	h.sample()
	assert.Equal(t, importTestObjects, res.Imported)
	t.Logf("postgres import: %d objects in %s, peak live heap above baseline %.1f MiB (bound %d MiB)",
		importTestObjects, time.Since(start).Round(time.Millisecond), float64(h.peak)/(1<<20), importHeapBound>>20)
	assert.Less(t, h.peak, uint64(importHeapBound))

	entries, err := store.List(context.Background(), "team-a", "uid-agent-07")
	require.NoError(t, err)
	require.Len(t, entries, importTestObjects/importTestAgents)
	for k, e := range entries {
		// Newest first is the lowest index: each later index was created a second earlier.
		require.Equal(t, pager.versionOf(7+k*importTestAgents), e.Name, "position %d", k)
	}
}
