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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/authz"
)

// workspaces.go — what a read given no namespace ("All workspaces") covers.
//
// A person is bound to a workspace by a RoleBinding, which never grants a cluster-wide list, so a
// read given no namespace cannot simply ask Kubernetes for every namespace. It covers the
// namespaces the caller works in instead: the set GET /api/namespaces answers, computed once per
// request with the caller's own token. A caller who may read cluster-wide still gets one
// cluster-wide read, so nothing changes for them.
//
// The answer is 403 only when the caller may read the kind in none of their workspaces. A caller
// allowed somewhere whose namespaces hold nothing gets 200 with an empty list; whether it is 403 or
// 200 never depends on what the stores hold, so the status says nothing about other people's rows.
//
// Work per request, bounded by the namespace mirror (M namespaces) and the caller's workspaces
// (W <= M): discovery runs once (one namespace list, then for a caller who may not list namespaces
// one mirror read and at most M access reviews, maxWorkspaceProbes in flight); each kind read then
// costs one cluster-wide attempt plus at most W per-namespace reads or access reviews, and a paged
// read stops as soon as its page is full.
//
// A namespace is a workspace when the caller may list agents in it, the same test the picker uses,
// so a namespace where someone may list only some other kind is not part of "All workspaces"; they
// read it by selecting it.

// maxWorkspaceProbes bounds the access reviews in flight at once during discovery.
const maxWorkspaceProbes = 16

// workspaceCursorPrefix marks a list cursor that resumes a read across workspaces. A Kubernetes
// continue token is base64, which never contains '.', so the two never collide.
const workspaceCursorPrefix = "ws."

// errInvalidListCursor is a cursor this server did not issue for this caller.
var errInvalidListCursor = errors.New("invalid list cursor")

// workspaceSet is the namespaces a caller works in.
type workspaceSet struct {
	// namespaces is sorted by name. It is every namespace when the caller may list namespaces, and
	// otherwise the mirror's namespaces where the caller may list agents.
	namespaces []NamespaceSummary
	// known is false when the caller may not list namespaces and the mirror had nothing to offer:
	// nothing was learned, so an empty set must not be read as "no workspace".
	known bool
	// denial is the caller's refusal to list namespaces, the honest answer when !known.
	denial error
}

func (ws workspaceSet) names() []string {
	out := make([]string, 0, len(ws.namespaces))
	for _, ns := range ws.namespaces {
		out = append(out, ns.Name)
	}
	return out
}

type workspaceMemoKey struct{}

type workspaceMemo struct {
	once sync.Once
	set  workspaceSet
	err  error
}

// withWorkspaceMemo gives each request one slot for its caller's workspace set, so every read the
// request makes shares one discovery.
func withWorkspaceMemo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), workspaceMemoKey{}, &workspaceMemo{})))
	})
}

// callerWorkspaces returns the namespaces the caller works in, discovered at most once per request.
func (s *Server) callerWorkspaces(ctx context.Context, caller client.Client) (workspaceSet, error) {
	if wc, ok := caller.(workspaceClient); ok {
		caller = wc.Client
	}
	if m, ok := ctx.Value(workspaceMemoKey{}).(*workspaceMemo); ok {
		m.once.Do(func() { m.set, m.err = s.discoverWorkspaces(ctx, caller) })
		return m.set, m.err
	}
	return s.discoverWorkspaces(ctx, caller)
}

// discoverWorkspaces lists namespaces as the caller. A caller who may not gets the mirror's
// namespaces filtered to those they may use. Only a refusal falls back; a rejected token or a
// transport error is returned as it is.
func (s *Server) discoverWorkspaces(ctx context.Context, caller client.Client) (workspaceSet, error) {
	var list corev1.NamespaceList
	err := caller.List(ctx, &list)
	if err == nil {
		out := make([]NamespaceSummary, 0, len(list.Items))
		for i := range list.Items {
			ns := &list.Items[i]
			out = append(out, NamespaceSummary{
				Name:        ns.Name,
				DisplayName: ns.Annotations[annNamespaceDisplayName],
				Platform:    s.isPlatformNamespace(ns.Name),
			})
		}
		slices.SortFunc(out, func(a, b NamespaceSummary) int { return strings.Compare(a.Name, b.Name) })
		return workspaceSet{namespaces: out, known: true}, nil
	}
	if !apierrors.IsForbidden(err) {
		return workspaceSet{}, err
	}
	visible, known, ferr := s.callerVisibleNamespaces(ctx, caller)
	if ferr != nil {
		return workspaceSet{}, fmt.Errorf("namespace discovery: %w", ferr)
	}
	return workspaceSet{namespaces: visible, known: known, denial: err}, nil
}

// probeEach runs probe for every name with at most maxWorkspaceProbes in flight and returns the
// verdicts in input order.
func probeEach(ctx context.Context, names []string, probe func(context.Context, string) (bool, error)) ([]bool, []error) {
	allowed := make([]bool, len(names))
	errs := make([]error, len(names))
	sem := make(chan struct{}, maxWorkspaceProbes)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			allowed[i], errs[i] = probe(ctx, name)
		}()
	}
	wg.Wait()
	return allowed, errs
}

// listableNamespaces is the authorization half of a store-backed read given no namespace: all is
// true when the caller may list resource cluster-wide; otherwise namespaces holds the caller's
// workspaces where they may list it, never empty. A caller allowed nowhere gets the cluster-wide
// denial. It reads no store, so a refused caller costs no store work.
func (s *Server) listableNamespaces(ctx context.Context, caller client.Client, resource string) (all bool, namespaces []string, err error) {
	denial := s.authorizeStore(ctx, caller, authz.VerbList, resource, "", "")
	if denial == nil {
		return true, nil, nil
	}
	if !errors.Is(denial, authz.ErrForbidden) {
		return false, nil, denial
	}
	set, err := s.callerWorkspaces(ctx, caller)
	if err != nil {
		return false, nil, err
	}
	candidates := set.names()
	allowed, errs := probeEach(ctx, candidates, func(ctx context.Context, ns string) (bool, error) {
		err := s.authorizeStore(ctx, caller, authz.VerbList, resource, ns, "")
		if errors.Is(err, authz.ErrForbidden) {
			return false, nil
		}
		return err == nil, err
	})
	for i, ns := range candidates {
		if errs[i] != nil {
			return false, nil, errs[i]
		}
		if allowed[i] {
			namespaces = append(namespaces, ns)
		}
	}
	if len(namespaces) == 0 {
		return false, nil, denial
	}
	return false, namespaces, nil
}

// authorizeStoreList authorizes a paged store list as the caller and, for a list given no
// namespace, narrows opts to the namespaces the caller may list. It writes the refusal and reports
// false when the list must not run.
func (s *Server) authorizeStoreList(w http.ResponseWriter, r *http.Request, caller client.Client, resource string, opts *controlplane.ListOptions, action string) bool {
	if opts.Namespace != "" {
		if err := s.authorizeStore(r.Context(), caller, authz.VerbList, resource, opts.Namespace, ""); err != nil {
			s.writeAuthzError(w, err, action)
			return false
		}
		return true
	}
	all, namespaces, err := s.listableNamespaces(r.Context(), caller, resource)
	if err != nil {
		s.writeAuthzError(w, err, action)
		return false
	}
	if !all {
		opts.Namespaces = namespaces
	}
	return true
}

// workspaceClient is the caller's own client whose List, given no namespace, covers the caller's
// workspaces when a cluster-wide list is refused. Everything else passes through. Use it only for
// namespaced kinds.
type workspaceClient struct {
	client.Client
	s *Server
}

// acrossWorkspaces wraps the caller's client so a namespace-less List reads "All workspaces".
func (s *Server) acrossWorkspaces(caller client.Client) client.Client {
	if _, ok := caller.(workspaceClient); ok {
		return caller
	}
	return workspaceClient{Client: caller, s: s}
}

type workspaceCursor struct {
	Namespace string `json:"n"`
	Continue  string `json:"c,omitempty"`
}

func encodeWorkspaceCursor(c workspaceCursor) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return workspaceCursorPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeWorkspaceCursor(s string) (workspaceCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, workspaceCursorPrefix))
	if err != nil {
		return workspaceCursor{}, errInvalidListCursor
	}
	var c workspaceCursor
	if err := json.Unmarshal(raw, &c); err != nil || c.Namespace == "" {
		return workspaceCursor{}, errInvalidListCursor
	}
	return c, nil
}

// List reads cluster-wide when the caller may. Refused, it reads each of the caller's workspaces in
// name order, skipping any where the read is refused, and fills a page of opts' Limit across them;
// the continue token it returns then names the namespace to resume in. Any page, first or resumed,
// is refused only when the caller may read the kind neither cluster-wide nor in any workspace.
func (c workspaceClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	lo := (&client.ListOptions{}).ApplyOptions(opts)
	resuming := strings.HasPrefix(lo.Continue, workspaceCursorPrefix)
	if lo.Namespace != "" {
		// An "All workspaces" cursor means nothing inside one namespace; the API server would
		// reject it as malformed, which would surface as a 500.
		if resuming {
			return errInvalidListCursor
		}
		return c.Client.List(ctx, list, opts...)
	}
	var denial error
	if !resuming {
		err := c.Client.List(ctx, list, opts...)
		if err == nil || !apierrors.IsForbidden(err) || lo.Continue != "" {
			return err
		}
		denial = err
	}

	set, err := c.s.callerWorkspaces(ctx, c.Client)
	if err != nil {
		return err
	}
	if !set.known && !resuming {
		return denial
	}
	names := set.names()
	from := workspaceCursor{}
	if resuming {
		if from, err = decodeWorkspaceCursor(lo.Continue); err != nil {
			return err
		}
	}
	allowed, err := c.listFrom(ctx, list, lo, names, from)
	if err != nil || allowed {
		return err
	}
	if resuming {
		// Every workspace from the cursor on refused. A denial must never read as an empty page,
		// so the caller gets 200 only if they may read somewhere the walk did not reach.
		before, _ := slices.BinarySearch(names, from.Namespace)
		return c.refusedEverywhere(ctx, list, lo, names[:before])
	}
	return denial
}

// refusedEverywhere answers a resumed page that read nothing: the cluster-wide refusal the first
// page would have returned, unless the caller may read the kind cluster-wide or in one of
// unvisited. Each probe reads at most one item into a copy, so list is left as it is.
func (c workspaceClient) refusedEverywhere(ctx context.Context, list client.ObjectList, lo *client.ListOptions, unvisited []string) error {
	probe := func(ns string) error {
		o := *lo
		o.Namespace, o.Continue, o.Limit = ns, "", 1
		_, _, err := c.listNamespace(ctx, list, &o)
		return err
	}
	denial := probe("")
	if !apierrors.IsForbidden(denial) {
		return denial
	}
	for _, ns := range unvisited {
		if err := probe(ns); !apierrors.IsForbidden(err) {
			return err
		}
	}
	return denial
}

// listFrom fills list from names, starting at from: at its namespace, or at the next one when it
// has left the caller's set since the last page. Every namespace read is still the caller's own,
// so a hand-made cursor reaches nothing a fresh list would not. It reports whether any namespace
// allowed the read.
func (c workspaceClient) listFrom(ctx context.Context, list client.ObjectList, lo *client.ListOptions, names []string, from workspaceCursor) (bool, error) {
	start, cont := 0, ""
	if from.Namespace != "" {
		start, _ = slices.BinarySearch(names, from.Namespace)
		if start < len(names) && names[start] == from.Namespace {
			cont = from.Continue
		}
	}
	var (
		items   []runtime.Object
		resume  *workspaceCursor
		allowed bool
	)
	full := func() bool { return lo.Limit > 0 && int64(len(items)) >= lo.Limit }
	for i := start; i < len(names) && resume == nil && !full(); i++ {
		nsOpts := *lo
		nsOpts.Namespace, nsOpts.Continue = names[i], cont
		if lo.Limit > 0 {
			nsOpts.Limit = lo.Limit - int64(len(items))
		}
		cont = ""
		got, next, err := c.listNamespace(ctx, list, &nsOpts)
		if apierrors.IsForbidden(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		allowed = true
		items = append(items, got...)
		switch {
		case next != "":
			resume = &workspaceCursor{Namespace: names[i], Continue: next}
		case full() && i+1 < len(names):
			resume = &workspaceCursor{Namespace: names[i+1]}
		}
	}
	nextCursor := ""
	if resume != nil {
		var err error
		if nextCursor, err = encodeWorkspaceCursor(*resume); err != nil {
			return false, err
		}
	}
	if err := meta.SetList(list, items); err != nil {
		return false, err
	}
	list.SetContinue(nextCursor)
	return allowed, nil
}

// listNamespace reads one page of one namespace into a fresh list of list's type.
func (c workspaceClient) listNamespace(ctx context.Context, list client.ObjectList, opts *client.ListOptions) ([]runtime.Object, string, error) {
	page, ok := list.DeepCopyObject().(client.ObjectList)
	if !ok {
		return nil, "", fmt.Errorf("list %T cannot be copied", list)
	}
	if err := meta.SetList(page, nil); err != nil {
		return nil, "", err
	}
	if err := c.Client.List(ctx, page, opts); err != nil {
		return nil, "", err
	}
	items, err := meta.ExtractList(page)
	if err != nil {
		return nil, "", err
	}
	return items, page.GetContinue(), nil
}
