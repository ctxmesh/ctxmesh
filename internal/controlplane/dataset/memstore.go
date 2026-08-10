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
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ctxmesh/agent-engine/internal/controlplane"
)

// memStore is the in-memory twin of the Postgres store — used in unit tests + the cross-impl conformance suite
// (both must behave identically), so BFF/handler tests need no live DB. Not for production. It mirrors the pg
// store's pin-freeze semantics: PinVersion COPIES the current cases + each case's latest label into a frozen
// snapshot, so later appends never mutate a pinned resolution (ADR 0062 Fork 1).
type memStore struct {
	mu sync.Mutex

	datasets  map[string]*memDataset // key: namespace + "\x00" + name
	byID      map[string]*memDataset // key: dataset ID
	caseIndex map[string]*memCase    // key: case ID → the owning case (for AppendLabel)

	seq int64            // monotonic ID source
	now func() time.Time // clock (overridable in tests for deterministic label ordering)
}

type memDataset struct {
	dataset  Dataset
	cases    []*memCase             // the draft head, append order
	versions map[int][]ResolvedCase // frozen snapshots keyed by version number
	maxVer   int
}

type memCase struct {
	c      Case
	labels []Label // append-only; the LAST element is the current label state
}

// NewMemStore returns an in-memory Store.
func NewMemStore() Store {
	return &memStore{
		datasets:  map[string]*memDataset{},
		byID:      map[string]*memDataset{},
		caseIndex: map[string]*memCase{},
		now:       time.Now,
	}
}

func memKey(ns, name string) string { return ns + "\x00" + name }

func (m *memStore) nextID(prefix string) string {
	m.seq++
	return prefix + "-" + strconv.FormatInt(m.seq, 10)
}

func (m *memStore) EnsureDataset(_ context.Context, namespace, name string) (*Dataset, error) {
	namespace, name = strings.TrimSpace(namespace), strings.TrimSpace(name)
	if namespace == "" || name == "" {
		return nil, fmt.Errorf("dataset: %w: namespace and name are required", controlplane.ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	k := memKey(namespace, name)
	if d, ok := m.datasets[k]; ok {
		out := d.dataset
		return &out, nil
	}
	now := m.now().UTC()
	d := &memDataset{
		dataset:  Dataset{ID: m.nextID("ds"), Namespace: namespace, Name: name, CreatedAt: now, UpdatedAt: now},
		versions: map[int][]ResolvedCase{},
	}
	m.datasets[k] = d
	m.byID[d.dataset.ID] = d
	out := d.dataset
	return &out, nil
}

func (m *memStore) AppendCase(_ context.Context, datasetID string, c Case) (string, error) {
	if strings.TrimSpace(c.Input) == "" {
		return "", fmt.Errorf("dataset: %w: case input is required", controlplane.ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	d, ok := m.byID[datasetID]
	if !ok {
		return "", fmt.Errorf("dataset: %w: dataset %q", controlplane.ErrNotFound, datasetID)
	}
	mc := &memCase{c: Case{
		ID:            m.nextID("case"),
		DatasetID:     datasetID,
		Input:         c.Input,
		Expected:      c.Expected,
		SourceTraceID: c.SourceTraceID,
		MimeType:      c.MimeType,
		Tags:          cloneTags(c.Tags),
		CreatedAt:     m.now().UTC(),
	}}
	d.cases = append(d.cases, mc)
	m.caseIndex[mc.c.ID] = mc
	return mc.c.ID, nil
}

func (m *memStore) AppendLabel(_ context.Context, caseID string, l Label) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	mc, ok := m.caseIndex[caseID]
	if !ok {
		return fmt.Errorf("dataset: %w: case %q", controlplane.ErrNotFound, caseID)
	}
	// APPEND-ONLY: never mutate an existing element; the newest append is the current state.
	mc.labels = append(mc.labels, Label{
		ID:         m.nextID("label"),
		CaseID:     caseID,
		Value:      l.Value,
		Correction: l.Correction,
		Note:       l.Note,
		Author:     l.Author,
		CreatedAt:  m.now().UTC(),
	})
	return nil
}

func (m *memStore) PinVersion(_ context.Context, datasetID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	d, ok := m.byID[datasetID]
	if !ok {
		return 0, fmt.Errorf("dataset: %w: dataset %q", controlplane.ErrNotFound, datasetID)
	}
	if len(d.cases) == 0 {
		return 0, fmt.Errorf("dataset: %w: cannot pin a dataset with no cases", controlplane.ErrInvalid)
	}
	// FREEZE a COPY of every case + its latest label state. Copying (not referencing) is what makes later
	// case/label appends unable to move an already-pinned resolution.
	snapshot := make([]ResolvedCase, 0, len(d.cases))
	for _, mc := range d.cases {
		rc := ResolvedCase{
			CaseID:        mc.c.ID,
			Input:         mc.c.Input,
			Expected:      mc.c.Expected,
			SourceTraceID: mc.c.SourceTraceID,
			MimeType:      mc.c.MimeType,
			Tags:          cloneTags(mc.c.Tags),
		}
		if n := len(mc.labels); n > 0 {
			last := mc.labels[n-1]
			rc.HasLabel = true
			rc.LabelValue = last.Value
			rc.LabelCorrection = last.Correction
			rc.LabelNote = last.Note
			rc.LabelAuthor = last.Author
		}
		snapshot = append(snapshot, rc)
	}
	d.maxVer++
	d.versions[d.maxVer] = snapshot
	return d.maxVer, nil
}

func (m *memStore) ResolveVersion(_ context.Context, namespace, name string, version int) ([]ResolvedCase, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	d, ok := m.datasets[memKey(strings.TrimSpace(namespace), strings.TrimSpace(name))]
	if !ok {
		return nil, fmt.Errorf("dataset: %w: dataset %q", controlplane.ErrNotFound, name)
	}
	return m.resolveLocked(d, version)
}

func (m *memStore) ResolveRef(_ context.Context, namespace, ref string) ([]ResolvedCase, int, error) {
	name, version, pinned, err := parseRef(ref)
	if err != nil {
		return nil, 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	d, ok := m.datasets[memKey(strings.TrimSpace(namespace), name)]
	if !ok {
		return nil, 0, fmt.Errorf("dataset: %w: dataset %q", controlplane.ErrNotFound, name)
	}
	if !pinned {
		if d.maxVer == 0 {
			return nil, 0, fmt.Errorf("dataset: %w: dataset %q has no pinned version", controlplane.ErrInvalid, name)
		}
		version = d.maxVer
	}
	cases, err := m.resolveLocked(d, version)
	if err != nil {
		return nil, 0, err
	}
	return cases, version, nil
}

// resolveLocked returns a deep copy of the frozen snapshot for a version (caller holds m.mu). A missing version
// → controlplane.ErrNotFound.
func (m *memStore) resolveLocked(d *memDataset, version int) ([]ResolvedCase, error) {
	frozen, ok := d.versions[version]
	if !ok {
		return nil, fmt.Errorf("dataset: %w: version %d", controlplane.ErrNotFound, version)
	}
	out := make([]ResolvedCase, len(frozen))
	for i, rc := range frozen {
		out[i] = rc
		out[i].Tags = cloneTags(rc.Tags)
	}
	return out, nil
}

func (m *memStore) ListCases(_ context.Context, datasetID string) ([]Case, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	d, ok := m.byID[datasetID]
	if !ok {
		return nil, fmt.Errorf("dataset: %w: dataset %q", controlplane.ErrNotFound, datasetID)
	}
	out := make([]Case, 0, len(d.cases))
	for _, mc := range d.cases {
		c := mc.c
		c.Tags = cloneTags(c.Tags)
		out = append(out, c)
	}
	return out, nil
}

func (m *memStore) ListDatasets(_ context.Context, namespace string) ([]Dataset, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil, fmt.Errorf("dataset: %w: namespace is required", controlplane.ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Dataset, 0)
	for _, d := range m.datasets {
		if d.dataset.Namespace == namespace {
			ds := d.dataset
			out = append(out, ds)
		}
	}
	// Stable oldest-first order.
	slices.SortFunc(out, func(a, b Dataset) int {
		if a.CreatedAt.Before(b.CreatedAt) {
			return -1
		}
		if a.CreatedAt.After(b.CreatedAt) {
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

func (m *memStore) LatestLabel(_ context.Context, caseID string) (*Label, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	mc, ok := m.caseIndex[caseID]
	if !ok {
		return nil, fmt.Errorf("dataset: %w: case %q", controlplane.ErrNotFound, caseID)
	}
	if len(mc.labels) == 0 {
		return nil, nil
	}
	l := mc.labels[len(mc.labels)-1]
	return &l, nil
}

// cloneTags copies a tag map, normalizing empty to nil (parity with the pg store's jsonb round-trip).
func cloneTags(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}
