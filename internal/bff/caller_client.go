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
	"io"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CallerClientFactory builds a Kubernetes client scoped to the CALLER'S
// identity for a single request (ADR 0011: the BFF is an auth-transparent
// proxy). Every user-facing CRD read/write goes through a client this factory
// produces, so the Kubernetes API server enforces the caller's own RBAC (the
// M11 developer/operator/viewer personas). The BFF never re-implements RBAC —
// it only routes the caller's token; K8s is the single authorization source.
//
// The factory is a seam so it is unit-testable with a fake (assert the token
// that flows in, hand back a fake client.Client) and so the Playground (m12.7)
// can reuse the exact same caller-scoping for its invoke path.
type CallerClientFactory interface {
	// ForRequest extracts the caller's bearer token from the request and returns
	// a client.Client whose every operation runs as that caller. A missing or
	// empty token yields errUnauthenticated (surfaced as 401) BEFORE any K8s call
	// — an anonymous caller never reaches the API server through the BFF.
	ForRequest(r *http.Request) (client.Client, error)

	// PodLogsForRequest returns a PodLogAccessor scoped to the CALLER'S token, for
	// the SSE log-tail (GET /api/agents/{ns}/{name}/logs). The controller-runtime
	// client cannot stream a pod's log subresource, so the log tail needs a typed
	// core client built from the SAME per-request caller config — NEVER the BFF SA
	// (whose RBAC has rules:[] and cannot read pods/log; only the caller can). A
	// missing/empty token yields errUnauthenticated (401) before any K8s call, the
	// same gate as ForRequest.
	PodLogsForRequest(r *http.Request) (PodLogAccessor, error)
}

// PodLogAccessor is the narrow, caller-scoped seam the SSE log-tail needs: list
// the pods backing an agent (by Knative service label) and open a streaming read
// of one pod's log subresource. It is satisfied in production by a client-go
// clientset built from the caller's bearer token, and by a fake in tests — so the
// handler proves the stream + the caller-scoped 403 path without a real cluster.
// Both operations run as the caller: the K8s API server's `pods/log` RBAC decides
// whether the caller may read logs (a denial is a Forbidden the handler surfaces,
// never the BFF SA reaching past the caller).
type PodLogAccessor interface {
	// ListPods lists pods in the namespace matching labelSelector. It returns the
	// raw PodList so the handler can pick the active pod and honor its phase.
	ListPods(ctx context.Context, namespace, labelSelector string) (*corev1.PodList, error)
	// StreamPodLog opens a streaming read of one pod's log subresource. The caller
	// closes the returned reader. follow=true streams until the context is
	// cancelled or the pod ends; tailLines (when >0) bounds the initial backlog.
	StreamPodLog(ctx context.Context, namespace, pod, container string, follow bool, tailLines *int64) (io.ReadCloser, error)
}

// callerClient builds the per-request caller-scoped client for a handler and
// maps a factory failure to the right HTTP status, writing the error response so
// the handler can simply return. On success it returns the client and ok=true.
//
//   - a missing/empty token (errUnauthenticated) → 401 BEFORE any K8s call;
//   - any other construction failure → 500 (server misconfig, logged).
//
// This is the single choke point through which every user-facing CRD op obtains
// its client, so no handler can accidentally reach for a static SA client.
func (s *Server) callerClient(w http.ResponseWriter, r *http.Request) (client.Client, bool) {
	c, err := s.callerClients.ForRequest(r)
	if err != nil {
		if err == errUnauthenticated {
			writeError(w, http.StatusUnauthorized, err.Error())
			return nil, false
		}
		s.log.Error(err, "build caller-scoped client failed")
		writeError(w, http.StatusInternalServerError, "failed to build caller client")
		return nil, false
	}
	return c, true
}

// classifyReadError maps a caller-scoped READ failure to an honest HTTP status
// when it is an authz/authn rejection from the K8s API server, so a viewer/
// unauthorized caller sees a real 403/401 instead of an empty list swallowed as
// success. isRBAC is false for anything else (transient/API errors), which the
// handler then treats as a 500. Read denials must never masquerade as "no data".
func classifyReadError(err error) (status int, msg string, isRBAC bool) {
	switch {
	case apierrors.IsForbidden(err):
		return http.StatusForbidden, "forbidden: not allowed to read the requested resources", true
	case apierrors.IsUnauthorized(err):
		return http.StatusUnauthorized, "unauthorized: token rejected by the API server", true
	default:
		return 0, "", false
	}
}

// bearerToken pulls the token out of an "Authorization: Bearer <token>" header,
// trimming surrounding whitespace. It returns "" when the header is absent, not
// a bearer credential, or empty after the prefix — the single place the wire
// format is parsed, shared by BearerAuthenticator and the caller-client factory
// so the edge (presence) and the token flow agree byte-for-byte.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// restCallerClientFactory builds a per-request client.Client from the caller's
// bearer token, layered on the in-cluster rest.Config's transport (API-server
// host + cluster CA/TLS). It is the production CallerClientFactory.
type restCallerClientFactory struct {
	// base is the in-cluster rest.Config (host, CA, TLS). It is NEVER mutated —
	// every request gets a COPY with only the credential swapped, so concurrent
	// requests can never see each other's token.
	base *rest.Config
	// scheme registers the platform CRDs so the typed client can encode/decode
	// AgentDeployment/EvalSuite/PromptVersion/etc.
	scheme *runtime.Scheme
	// newClient constructs the client from a config; overridable in tests. In
	// production it is sigs.k8s.io/controller-runtime/pkg/client.New.
	newClient func(cfg *rest.Config, opts client.Options) (client.Client, error)
}

// NewCallerClientFactory returns the production CallerClientFactory. base is the
// in-cluster rest.Config (from ctrl.GetConfig); scheme carries the platform CRDs.
// The base config is copied per request — the caller passes the shared config and
// this factory never mutates it.
func NewCallerClientFactory(base *rest.Config, scheme *runtime.Scheme) CallerClientFactory {
	return &restCallerClientFactory{
		base:      base,
		scheme:    scheme,
		newClient: client.New,
	}
}

// ForRequest implements CallerClientFactory.
func (f *restCallerClientFactory) ForRequest(r *http.Request) (client.Client, error) {
	token := bearerToken(r)
	if token == "" {
		// No credential to scope by → reject before touching the API server.
		return nil, errUnauthenticated
	}
	return f.forToken(token)
}

// PodLogsForRequest implements CallerClientFactory: it builds a caller-scoped
// clientset from the SAME per-request config as ForRequest and wraps it in the
// PodLogAccessor seam. The typed core client is what streams the pod-log
// subresource (the controller-runtime client cannot), and it runs as the caller
// — a caller without `pods/log` RBAC gets a Forbidden, never the BFF SA reaching
// past them.
func (f *restCallerClientFactory) PodLogsForRequest(r *http.Request) (PodLogAccessor, error) {
	token := bearerToken(r)
	if token == "" {
		return nil, errUnauthenticated
	}
	cs, err := kubernetes.NewForConfig(f.callerConfig(token))
	if err != nil {
		return nil, err
	}
	return &clientsetPodLogAccessor{cs: cs}, nil
}

// callerConfig copies the in-cluster config and swaps in ONLY the caller's bearer
// token, dropping every other credential source. It is the single place the
// per-request caller credential is assembled, shared by the CRD client and the
// pod-log clientset so both authenticate byte-identically as the caller.
//
// Clearing BearerTokenFile is load-bearing: client-go prefers the file when both
// are set and re-reads the mounted ServiceAccount token, which would silently
// send the BFF's OWN identity to the API server (a confused-deputy regression).
// The copy (rest.CopyConfig) guarantees the shared base config is never mutated,
// so this is safe under concurrency.
func (f *restCallerClientFactory) callerConfig(token string) *rest.Config {
	cfg := rest.CopyConfig(f.base)
	cfg.BearerToken = token
	cfg.BearerTokenFile = ""
	// Drop any other in-cluster credential material so ONLY the caller's bearer
	// token authenticates the request — no client cert, no exec/auth-provider
	// plugin can override it.
	cfg.CertData = nil
	cfg.CertFile = ""
	cfg.KeyData = nil
	cfg.KeyFile = ""
	cfg.Username = ""
	cfg.Password = ""
	cfg.AuthProvider = nil
	cfg.ExecProvider = nil
	return cfg
}

// forToken builds the caller-scoped controller-runtime client from a raw bearer
// token via callerConfig, so the CRD reads/writes run as the caller (ADR 0011).
func (f *restCallerClientFactory) forToken(token string) (client.Client, error) {
	return f.newClient(f.callerConfig(token), client.Options{Scheme: f.scheme})
}

// clientsetPodLogAccessor is the production PodLogAccessor backed by a caller-
// scoped client-go clientset. Both methods run as the caller — the API server's
// `pods` / `pods/log` RBAC governs them.
type clientsetPodLogAccessor struct {
	cs kubernetes.Interface
}

func (a *clientsetPodLogAccessor) ListPods(ctx context.Context, namespace, labelSelector string) (*corev1.PodList, error) {
	return a.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
}

func (a *clientsetPodLogAccessor) StreamPodLog(ctx context.Context, namespace, pod, container string, follow bool, tailLines *int64) (io.ReadCloser, error) {
	opts := &corev1.PodLogOptions{
		Follow:    follow,
		TailLines: tailLines,
	}
	if container != "" {
		opts.Container = container
	}
	return a.cs.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
}
