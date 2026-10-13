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
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/ctxmesh/ctxmesh/internal/controlplane/alertstore"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/auditlog"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/promptversion"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/skill"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
	"github.com/ctxmesh/ctxmesh/internal/run"
)

// workspaceFixture is one object of every console-listed kind in a namespace, as the API server
// will accept it.
func workspaceFixture(ns string) []*unstructured.Unstructured {
	obj := func(kind, name string, labels map[string]any, spec map[string]any) *unstructured.Unstructured {
		meta := map[string]any{"name": name, "namespace": ns}
		if labels != nil {
			meta["labels"] = labels
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "agents.ctxmesh.ai/v1beta1", "kind": kind, "metadata": meta, "spec": spec,
		}}
	}
	return []*unstructured.Unstructured{
		obj("AgentDeployment", "agent", nil, map[string]any{"image": "ghcr.io/example/agent:1"}),
		obj("ModelRoute", "route", map[string]any{labelManagedBy: managedByConnect}, map[string]any{
			"providers": []any{map[string]any{"provider": "openai", "model": "gpt", "priority": int64(1), "secretBindingRef": "binding"}},
		}),
		obj("SecretBinding", "binding", nil, map[string]any{"secretRef": map[string]any{"name": "s", "key": "k"}}),
		obj("AgentRegistry", "registry", nil, map[string]any{"registryId": "reg", "memberSelector": map[string]any{}}),
		obj("MCPToolBinding", "tool-binding", nil, map[string]any{
			"agentRef": "agent", "mode": "remote", "registryRef": "tools", "toolName": "lookup",
			"server": map[string]any{"url": "http://tools.svc/mcp"},
		}),
		obj("AgentScalingPolicy", "scaling", nil, map[string]any{"agentRef": "agent", "max": int64(2), "trigger": "request-rate"}),
		obj("EvalSuite", "suite", nil, map[string]any{
			"dataset": map[string]any{"ref": "d"}, "threshold": "0.5",
			"scorers": []any{map[string]any{"name": "s", "type": "mock"}},
		}),
		obj("KnowledgeBase", "kb", nil, map[string]any{"embeddingRoute": "route", "source": map[string]any{"type": "upload"}}),
		obj("GuardrailPolicy", "guard", nil, map[string]any{}),
		obj("Workflow", "flow", nil, map[string]any{
			"registryRef": "registry", "steps": []any{map[string]any{"name": "one", "agentRef": "agent"}},
		}),
		obj("AgentTeam", "team", nil, map[string]any{
			"registryRef": "registry", "supervisor": map[string]any{"agentRef": "agent"},
			"roster": []any{map[string]any{"name": "one", "agentRef": "agent"}},
		}),
	}
}

// responseNamespaces collects every namespace a list response names, at any depth.
func responseNamespaces(t *testing.T, body []byte) []string {
	t.Helper()
	var doc any
	require.NoError(t, json.Unmarshal(body, &doc))
	seen := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if s, ok := child.(string); ok && (k == "namespace" || k == "agentNs") && s != "" {
					seen[s] = true
				}
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(doc)
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	slices.Sort(out)
	return out
}

// TestEveryListForAPersonaBoundToOneNamespace: against a real API server, a person bound by
// RoleBinding the way the chart binds one reads every console list in their workspace and under
// "All workspaces", sees only their namespace there, and is refused another namespace. A person
// whose workspace is empty gets empty lists, a person bound nowhere gets 403, and a cluster-wide
// person still sees every namespace.
func TestEveryListForAPersonaBoundToOneNamespace(t *testing.T) {
	ctx := context.Background()
	admin, err := client.New(rbacCfg, client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	const mine, theirs, lonely = "lists-mine", "lists-theirs", "lists-lonely"
	mirror := namespacetenant.NewMemStore()
	registries := toolregistry.NewMemStore()
	prompts := promptversion.NewMemStore()
	alerts := alertstore.NewMemStore()
	audit := auditlog.NewMemStore()
	runs := run.NewMemStore()
	skills := skill.NewMemStore()
	now := time.Now().UTC()
	for _, ns := range []string{mine, theirs, lonely} {
		require.NoError(t, admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
		require.NoError(t, mirror.RecordNamespace(ctx, ns))
	}
	for _, ns := range []string{mine, theirs} {
		for _, o := range workspaceFixture(ns) {
			require.NoError(t, admin.Create(ctx, o), "%s/%s", o.GetKind(), ns)
		}
		_, err := registries.Upsert(ctx, toolregistry.ToolRegistry{
			Namespace: ns, Name: "tools",
			Tools:  []toolregistry.ToolEntry{{Name: "lookup", URL: "http://tools.svc/mcp"}},
			Labels: map[string]string{labelManagedBy: managedByMCP, labelMCPVisibility: visibilityTeam},
		})
		require.NoError(t, err)
		_, err = prompts.Upsert(ctx, promptversion.PromptVersion{Namespace: ns, Name: "prompt", Repo: "r", Ref: "1", Path: "p"})
		require.NoError(t, err)
		_, err = alerts.Append(ctx, alertstore.Alert{Namespace: ns, PolicyName: "p", Condition: "c", FiredAt: now})
		require.NoError(t, err)
		require.NoError(t, audit.Append(ctx, auditlog.Entry{
			OccurredAt: now, Source: "bff", Actor: "someone", ActorKind: "user", Action: "create",
			ResourceKind: "AgentDeployment", ResourceName: "agent", Namespace: ns,
		}))
		require.NoError(t, runs.Create(run.New("run-"+ns, ns, "agent", nil, "", now)))
		require.NoError(t, skills.UpsertSkill(ctx, skill.Skill{Namespace: ns, Name: "summarise"}))
	}

	viewerRole := installPersona(t, admin, "ctxmesh_viewer_role.yaml")
	developerRole := installPersona(t, admin, "ctxmesh_developer_role.yaml")
	operatorRole := installPersona(t, admin, "ctxmesh_operator_role.yaml")
	viewer := personaUser(t, admin, mine, "lists-viewer", viewerRole)
	developer := personaUser(t, admin, mine, "lists-developer", developerRole)
	operator := personaUser(t, admin, mine, "lists-operator", operatorRole)
	alone := personaUser(t, admin, lonely, "lists-alone", developerRole)
	nobody := personaUser(t, admin, mine, "lists-nobody")
	require.NoError(t, admin.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "lists-everywhere"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: developerRole},
		Subjects:   []rbacv1.Subject{{Kind: "User", Name: "lists-everywhere", APIGroup: rbacv1.GroupName}},
	}))
	everywhereUser, err := rbacEnv.AddUser(envtest.User{Name: "lists-everywhere", Groups: []string{"system:authenticated"}}, nil)
	require.NoError(t, err)
	everywhere, err := client.New(everywhereUser.Config(), client.Options{Scheme: testScheme(t)})
	require.NoError(t, err)

	send := func(c client.Client, method, path, body string) *httptest.ResponseRecorder {
		s := NewServer(Options{
			CallerClients:        newFakeFactory(c),
			Scheme:               testScheme(t),
			Auth:                 AllowAll{},
			MCPEnabled:           true,
			ProviderConnect:      true,
			ProviderHTTP:         &http.Client{},
			NamespaceTenantStore: mirror,
			ToolRegistryStore:    registries,
			PromptStore:          prompts,
			AlertStore:           alerts,
			AuditStore:           audit,
			RunStore:             runs,
			SkillStore:           skills,
			Version:              "test",
			Log:                  logr.Discard(),
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer persona-token")
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	serve := func(c client.Client) func(path string) *httptest.ResponseRecorder {
		return func(path string) *httptest.ResponseRecorder { return send(c, http.MethodGet, path, "") }
	}
	withNS := func(path, ns string) string {
		if ns == "" {
			return path
		}
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		return path + sep + "namespace=" + ns
	}

	developerLists := []string{
		"/api/agents", "/api/topology", "/api/modelroutes", "/api/secretbindings", "/api/agentregistries",
		"/api/mcptoolbindings", "/api/agentscalingpolicies", "/api/evalsuites", "/api/knowledgebases",
		"/api/guardrailpolicies", "/api/workflows", "/api/teams", "/api/providers", "/api/runs",
		"/api/tools", "/api/mcpservers", "/api/toolregistries", "/api/promptversions", "/api/alerts",
		"/api/skills",
	}
	operatorLists := []string{"/api/audit"}

	check := func(t *testing.T, get func(string) *httptest.ResponseRecorder, path string) {
		t.Helper()
		for _, scope := range []string{mine, ""} {
			rec := get(withNS(path, scope))
			require.Equal(t, http.StatusOK, rec.Code, "%s in %q: %s", path, scope, rec.Body.String())
			assert.Equal(t, []string{mine}, responseNamespaces(t, rec.Body.Bytes()), "%s in %q lists only their namespace", path, scope)
		}
		rec := get(withNS(path, theirs))
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s in another namespace: %s", path, rec.Body.String())
	}
	for _, path := range developerLists {
		t.Run("developer "+path, func(t *testing.T) { check(t, serve(developer), path) })
	}
	for _, path := range operatorLists {
		t.Run("operator "+path, func(t *testing.T) { check(t, serve(operator), path) })
	}

	t.Run("census counts only their namespace", func(t *testing.T) {
		for _, c := range []struct {
			who  client.Client
			want int
		}{{developer, 1}, {everywhere, 2}} {
			rec := serve(c.who)("/api/agents/census")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var body CensusResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.GreaterOrEqual(t, body.Total, c.want)
			if c.who == developer {
				assert.Equal(t, c.want, body.Total)
			}
		}
	})

	t.Run("an empty workspace lists nothing, successfully", func(t *testing.T) {
		get := serve(alone)
		for _, path := range developerLists {
			for _, scope := range []string{lonely, ""} {
				rec := get(withNS(path, scope))
				require.Equal(t, http.StatusOK, rec.Code, "%s in %q: %s", path, scope, rec.Body.String())
				assert.Empty(t, responseNamespaces(t, rec.Body.Bytes()), "%s in %q", path, scope)
			}
		}
	})

	t.Run("a person bound nowhere is refused", func(t *testing.T) {
		get := serve(nobody)
		for _, path := range append(slices.Clone(developerLists), operatorLists...) {
			rec := get(path)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", path, rec.Body.String())
		}
	})

	t.Run("a cluster-wide person still sees every namespace", func(t *testing.T) {
		get := serve(everywhere)
		for _, path := range developerLists {
			rec := get(path)
			require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
			got := responseNamespaces(t, rec.Body.Bytes())
			assert.Subset(t, got, []string{mine, theirs}, path)
		}
	})

	t.Run("skills: a viewer reads them, a developer writes them, neither reaches another namespace", func(t *testing.T) {
		check(t, serve(viewer), "/api/skills")
		create := func(who client.Client, ns, name string) *httptest.ResponseRecorder {
			return send(who, http.MethodPost, "/api/skills", `{"namespace":"`+ns+`","name":"`+name+`"}`)
		}
		rec := create(viewer, mine, "from-viewer")
		assert.Equal(t, http.StatusForbidden, rec.Code, "a viewer only reads: %s", rec.Body.String())
		rec = create(developer, mine, "from-developer")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		_, found, err := skills.GetSkill(ctx, mine, "from-developer")
		require.NoError(t, err)
		assert.True(t, found, "the developer's skill was stored")
		rec = create(developer, theirs, "from-developer")
		assert.Equal(t, http.StatusForbidden, rec.Code, "bound to %s, refused %s: %s", mine, theirs, rec.Body.String())
		_, found, err = skills.GetSkill(ctx, theirs, "from-developer")
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("the approval queue needs a namespace and is refused another", func(t *testing.T) {
		get := serve(developer)
		assert.Equal(t, http.StatusOK, get("/api/approvals?namespace="+mine).Code)
		assert.Equal(t, http.StatusForbidden, get("/api/approvals?namespace="+theirs).Code)
	})
}
