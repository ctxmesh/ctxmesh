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

// Package audit envtest-backed integration tests (build tag 'integration',
// run via `make test-integration`). They prove the M11.4 🧪: a CRD mutation
// (create + update + delete) emits an audit record, and the platform-persona
// ClusterRoles install and are valid.
package audit

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	sigsyaml "sigs.k8s.io/yaml"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
)

var (
	testCfg    *rest.Config
	testScheme *k8sruntime.Scheme
	k8sClient  client.Client
	testEnv    *envtest.Environment
)

// TestMain bootstraps envtest for the audit integration tests. It loads the
// agent CRD bases so the manager cache can build informers for every audited
// CRD (mirrors the controller suite pattern; ADR 0003 stdlib-only pyramid).
func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.WriteTo(os.Stderr), zap.UseDevMode(true)))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
		},
		ErrorIfCRDPathMissing: true,
	}
	if dir := firstEnvTestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic("failed to start envtest environment: " + err.Error())
	}
	testCfg = cfg

	testScheme = k8sruntime.NewScheme()
	if err = scheme.AddToScheme(testScheme); err != nil {
		panic("failed to add client-go scheme: " + err.Error())
	}
	if err = agentsv1alpha1.AddToScheme(testScheme); err != nil {
		panic("failed to add agents/v1alpha1 scheme: " + err.Error())
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		panic("failed to create envtest client: " + err.Error())
	}

	code := m.Run()

	if stopErr := testEnv.Stop(); stopErr != nil {
		logf.Log.Error(stopErr, "envtest environment stopped with error")
	}
	os.Exit(code)
}

// firstEnvTestBinaryDir returns the first subdirectory inside bin/k8s so tests
// run from an IDE (without KUBEBUILDER_ASSETS set) can still find the binaries
// downloaded by 'make setup-envtest'.
func firstEnvTestBinaryDir() string {
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

// recordingSink is a concurrency-safe Sink capturing entries emitted on the
// informer event loop (a different goroutine than the test).
type recordingSink struct {
	mu      sync.Mutex
	entries []AuditEntry
}

func (r *recordingSink) Record(entry AuditEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry)
}

// find returns the first entry matching verb+name, and whether one was found.
func (r *recordingSink) find(verb Verb, name string) (AuditEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.entries {
		if e.Verb == verb && e.Name == name {
			return e, true
		}
	}
	return AuditEntry{}, false
}

// TestAudit_MutationEmitsRecord is the M11.4 🧪: create + update + delete of an
// agent CRD each produce an audit record with the right verb/kind/name.
func TestAudit_MutationEmitsRecord(t *testing.T) {
	sink := &recordingSink{}

	mgr, err := manager.New(testCfg, manager.Options{
		Scheme:  testScheme,
		Metrics: server.Options{BindAddress: "0"},
	})
	require.NoError(t, err)

	require.NoError(t, NewAuditor(mgr.GetCache(), mgr.GetScheme(), sink).SetupWithManager(mgr))

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if startErr := mgr.Start(ctx); startErr != nil {
			logf.Log.Error(startErr, "manager stopped with error")
		}
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	// Wait for the cache (and thus the audit informers) to be ready before
	// mutating, so the create is observed as a live Add — not a warmup relist.
	require.True(t, mgr.GetCache().WaitForCacheSync(ctx), "cache did not sync")

	const name = "audit-probe"
	deploy := &agentsv1alpha1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: agentsv1alpha1.AgentDeploymentSpec{
			Image:          "ghcr.io/ctxmesh/example-agent:latest",
			ExecutionModel: "serving",
			Port:           8080,
		},
	}

	// ── create ────────────────────────────────────────────────────────────────
	require.NoError(t, k8sClient.Create(ctx, deploy))
	waitForEntry(t, sink, VerbCreate, name)

	// ── update ────────────────────────────────────────────────────────────────
	require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(deploy), deploy))
	deploy.Spec.Port = 9090
	require.NoError(t, k8sClient.Update(ctx, deploy))
	waitForEntry(t, sink, VerbUpdate, name)

	// ── delete (the most security-relevant action; informer DeleteFunc) ─────────
	require.NoError(t, k8sClient.Delete(ctx, deploy))
	deleteEntry := waitForEntry(t, sink, VerbDelete, name)

	// Entry shape: kind is stamped, name matches, subject is best-effort
	// non-empty (managedFields field-manager of the create/update apply).
	assert.Equal(t, "AgentDeployment", deleteEntry.Kind)
	assert.Equal(t, "default", deleteEntry.Namespace)
	assert.NotEmpty(t, deleteEntry.Subject, "subject should be best-effort populated")
	assert.False(t, deleteEntry.Timestamp.IsZero(), "timestamp should be set")

	// M63 (ADR 0056 §3): the entry now carries the mutated object's resourceVersion — the
	// PostgresSink folds it into the deterministic dedup key so cross-replica duplicate
	// observations of the SAME mutation collapse to one audit_log row.
	updateEntry, ok := sink.find(VerbUpdate, name)
	require.True(t, ok, "the update entry was recorded")
	assert.NotEmpty(t, updateEntry.ResourceVersion, "audit entries carry the resourceVersion (M63 dedup key)")
}

// waitForEntry polls the sink until an entry for verb+name appears, failing the
// test if it does not within the deadline. Returns the matched entry.
func waitForEntry(t *testing.T, sink *recordingSink, verb Verb, name string) AuditEntry {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if e, ok := sink.find(verb, name); ok {
			return e
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no %s audit entry for %q within deadline", verb, name)
	return AuditEntry{}
}

// TestPersonaClusterRoles_InstallAndValid proves the shipped persona
// ClusterRole YAMLs decode, install into the API server, and cover the agent
// CRDs with the expected verbs.
func TestPersonaClusterRoles_InstallAndValid(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		file      string
		roleName  string
		wantVerbs []string
		// wantAuditlogs asserts the OPERATOR-ONLY audit-read grant (ADR 0056 §4): only the operator
		// persona's ClusterRole carries `auditlogs`, so GET /api/audit's SSAR gate hides the trail
		// from developer/viewer. A regression that leaks it to another persona fails here.
		wantAuditlogs bool
	}{
		{
			file:          "ctxmesh_operator_role.yaml",
			roleName:      "operator",
			wantVerbs:     []string{"*"},
			wantAuditlogs: true,
		},
		{
			file:      "ctxmesh_developer_role.yaml",
			roleName:  "developer",
			wantVerbs: []string{"create", "delete", "get", "list", "patch", "update", "watch"},
		},
		{
			file:      "ctxmesh_viewer_role.yaml",
			roleName:  "viewer",
			wantVerbs: []string{"get", "list", "watch"},
		},
	}

	// Every agent CRD must be covered by each persona's resource rule — a
	// missing one is an authz gap.
	// memorybindings removed: MemoryBinding retired (ADR 0101), no longer a CRD.
	wantResources := []string{
		"agentdeployments", "agentversions", "modelroutes", "secretbindings",
		"mcptoolbindings", "toolregistries", "agentregistries",
		"agentteams", "agentscalingpolicies", "evalsuites", "promptversions",
	}

	for _, tc := range tests {
		t.Run(tc.roleName, func(t *testing.T) {
			role := loadClusterRole(t, tc.file)
			require.Equal(t, tc.roleName, role.Name)

			// Install into the API server — proves the manifest is valid RBAC.
			require.NoError(t, k8sClient.Create(ctx, role))
			t.Cleanup(func() { _ = k8sClient.Delete(ctx, role) })

			var got rbacv1.ClusterRole
			require.NoError(t, k8sClient.Get(ctx,
				client.ObjectKey{Name: tc.roleName}, &got))

			// The primary (resources) rule is the first rule: assert it covers
			// every agent CRD with the persona's verbs.
			require.NotEmpty(t, got.Rules)
			primary := got.Rules[0]
			assert.Equal(t, []string{"agents.ctxmesh.ai"}, primary.APIGroups)
			assert.ElementsMatch(t, tc.wantVerbs, primary.Verbs,
				"verbs for %s", tc.roleName)
			for _, res := range wantResources {
				assert.Contains(t, primary.Resources, res,
					"%s must cover CRD %q (authz gap otherwise)", tc.roleName, res)
			}
			// M63: auditlogs is operator-ONLY — present on operator, absent from developer/viewer.
			assert.Equal(t, tc.wantAuditlogs, slices.Contains(primary.Resources, "auditlogs"),
				"%s auditlogs coverage (operator-only, ADR 0056 §4)", tc.roleName)
		})
	}
}

// loadClusterRole reads and decodes a persona ClusterRole YAML from config/rbac.
func loadClusterRole(t *testing.T, file string) *rbacv1.ClusterRole {
	t.Helper()
	path := filepath.Join("..", "..", "config", "rbac", file)
	data, err := os.ReadFile(path)
	require.NoError(t, err, "reading %s", path)

	var role rbacv1.ClusterRole
	require.NoError(t, sigsyaml.Unmarshal(data, &role), "decoding %s", path)
	// Strip resourceVersion/uid so Create is a fresh install.
	role.ObjectMeta = metav1.ObjectMeta{
		Name:   role.Name,
		Labels: role.Labels,
	}
	return &role
}
