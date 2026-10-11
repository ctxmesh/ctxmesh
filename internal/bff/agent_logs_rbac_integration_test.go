//go:build integration

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
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestAgentLogTailFollowsShippedPersonas: against a real API server and the shipped persona roles,
// developer and operator read an agent's live output through the BFF and are told by
// /api/capabilities that they can, while viewer is refused with 403 and told it cannot.
//
// The API server streams a pod log from the pod's node's kubelet, so a fake kubelet serves the log
// and a Node points at it. The API server does not verify a kubelet's serving certificate unless
// --kubelet-certificate-authority is set, which envtest does not set.
func TestAgentLogTailFollowsShippedPersonas(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(rbacCfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	const (
		ns        = "agent-logs-rbac"
		agent     = "aha"
		pod       = "aha-00001-deployment-abc"
		container = "user-container"
		line      = "agent aha listening on :8080"
	)
	kubelet := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containerLogs/"+ns+"/"+pod+"/"+container {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(line + "\n"))
	}))
	t.Cleanup(kubelet.Close)
	_, portStr, err := net.SplitHostPort(kubelet.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.ParseInt(portStr, 10, 32)
	require.NoError(t, err)

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "fake-kubelet"}}
	require.NoError(t, admin.Create(ctx, node))
	node.Status = corev1.NodeStatus{
		Addresses:       []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "127.0.0.1"}},
		DaemonEndpoints: corev1.NodeDaemonEndpoints{KubeletEndpoint: corev1.DaemonEndpoint{Port: int32(port)}},
	}
	require.NoError(t, admin.Status().Update(ctx, node))

	require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: ns, Labels: map[string]string{knativeServiceLabel: agent}},
		Spec: corev1.PodSpec{
			NodeName:   node.Name,
			Containers: []corev1.Container{{Name: container, Image: "echo:1"}},
		},
	}
	require.NoError(t, admin.Create(ctx, p))
	p.Status.Phase = corev1.PodRunning
	require.NoError(t, admin.Status().Update(ctx, p))

	for _, tc := range []struct {
		role    string
		canRead bool
	}{
		{"ctxmesh_developer_role.yaml", true},
		{"ctxmesh_operator_role.yaml", true},
		{"ctxmesh_viewer_role.yaml", false},
	} {
		t.Run(tc.role, func(t *testing.T) {
			role := installPersona(t, admin, tc.role)
			c, cfg := personaUserConfig(t, admin, ns, "logs-"+role, role)
			cs, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err)
			s := NewServer(Options{
				CallerClients: &fakeCallerClientFactory{client: c, podLogs: &clientsetPodLogAccessor{cs: cs}},
				Scheme:        testScheme(t),
				Auth:          AllowAll{},
				Version:       "test",
				Log:           logr.Discard(),
			})
			get := func(path string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set("Authorization", "Bearer persona-token")
				s.Handler().ServeHTTP(rec, req)
				return rec
			}

			rec := get("/api/capabilities?namespace=" + ns)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var caps CapabilitiesResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &caps))
			assert.Equal(t, tc.canRead, caps.Allowed[resLogs][verbGet], "the console's logs capability")

			rec = get("/api/agents/" + ns + "/" + agent + "/logs")
			if !tc.canRead {
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				return
			}
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), sseFrame("", "log", line), "the tail carries the pod's output")
			assert.Contains(t, rec.Body.String(), sseFrame("", "end", "log stream ended"))
			assert.NotContains(t, rec.Body.String(), "event: error")
		})
	}
}
