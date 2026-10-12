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
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/skill"
	"github.com/ctxmesh/ctxmesh/internal/prompt"
	"github.com/ctxmesh/ctxmesh/internal/telemetry"
)

func TestWithSpecEnv(t *testing.T) {
	t.Parallel()

	platform := []corev1.EnvVar{
		{Name: envAgentPort, Value: "8080"},
		{Name: envModelGatewayURL, Value: "platform"},
		{Name: envMemoryPort, Value: "2998"},
	}
	spec := []corev1.EnvVar{
		{Name: "A", Value: "1"},
		{Name: envMemoryPort, Value: "author"},
		{Name: envAMPRequireAttestation, Value: postureAccept},
		{Name: envAgentVersion},
		{Name: "agent_name"},
		{Name: envPromptFile, Value: "/app/prompt.txt"},
		{Name: envModelGatewayURL, Value: "author"},
		{Name: envAgentVersion},
	}

	env, ignored := withSpecEnv(platform, 2, spec, false)
	assert.Equal(t, []corev1.EnvVar{
		{Name: envAgentPort, Value: "8080"},
		{Name: envModelGatewayURL, Value: "platform"},
		{Name: "A", Value: "1"},
		{Name: envAMPRequireAttestation, Value: postureAccept},
		{Name: "agent_name"},
		{Name: envPromptFile, Value: "/app/prompt.txt"},
		{Name: envMemoryPort, Value: "2998"},
	}, env, "spec.env goes in at the index, without what the platform owns; names are case-sensitive")
	assert.Equal(t, []string{envAgentVersion, envModelGatewayURL, envMemoryPort}, ignored,
		"each dropped name once, in census order, whatever the spec order")

	_, ignored = withSpecEnv(platform, 2, spec, true)
	assert.Equal(t, []string{envAgentVersion, envAMPRequireAttestation, envModelGatewayURL, envMemoryPort}, ignored,
		"a member's attestation wiring is reserved even where the platform leaves it unset")

	env, ignored = withSpecEnv(platform, 3, nil, true)
	assert.Equal(t, platform, env)
	assert.Empty(t, ignored)
}

// envVarLiteralsOutsideAgentContainer are the variables the package sets only on other containers: the
// egress sidecar, the discovery sidecar and in-pod tool containers, and the gateway Deployment. spec.env
// never reaches those containers, so these need no class.
var envVarLiteralsOutsideAgentContainer = []string{
	envEgressCredentialNamespace, envEgressAgent, envEgressListenAddr, envEgressRoutes, envEgressBoundary,
	envEgressResponseHeaderTimeout, envEgressStreamIdleTimeout, envToolPolicyFile, envEgressRoutesFile,
	"DISCOVERY_PORT", "TOOLS_JSON_PATH", "PORT",
}

// TestEveryPlatformVariableHasAClass is the census: every environment variable this package names in an
// EnvVar literal is either on the classified list, or one it sets only on another container. A variable
// added to the agent container without a class fails here, so its precedence against spec.env is a
// decision someone made, not whatever its position in the env list happened to give it.
func TestEveryPlatformVariableHasAClass(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, v := range platformEnv {
		assert.False(t, seen[v.name], "%s is listed twice", v.name)
		seen[v.name] = true
		assert.Contains(t, []platformEnvClass{envPlatformWhenSet, envReservedOnMember, envReserved}, v.class,
			"%s has no class", v.name)
		assert.NotContains(t, envVarLiteralsOutsideAgentContainer, v.name, "%s is listed as another container's", v.name)
	}

	names, unresolved := envVarLiteralNames(t)
	require.Contains(t, names, envModelGatewayURL, "the scan must see the agent container's literals")
	require.Contains(t, names, envEgressRoutes, "and the sidecars'")
	assert.Empty(t, unresolved, "an EnvVar literal whose name the census cannot read")
	for _, n := range names {
		if slices.Contains(envVarLiteralsOutsideAgentContainer, n) {
			continue
		}
		assert.Contains(t, platformEnvIndex, n,
			"%s is set by the controller and has no class in platformEnv: decide whether spec.env may set it", n)
	}
}

// envVarLiteralNames reads every corev1.EnvVar literal in the package's non-test sources and returns the
// names they set, resolving package constants. Names it cannot resolve are returned as positions.
func envVarLiteralNames(t *testing.T) (names, unresolved []string) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var parsed []*ast.File
	consts := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		require.NoError(t, err)
		parsed = append(parsed, file)
		for _, d := range file.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							consts[n.Name], _ = strconv.Unquote(lit.Value)
						}
					}
				}
			}
		}
	}
	isEnvVar := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && pkg.Name == "corev1" && sel.Sel.Name == "EnvVar"
	}
	record := func(lit *ast.CompositeLit) {
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Name" {
				continue
			}
			switch v := kv.Value.(type) {
			case *ast.BasicLit:
				s, _ := strconv.Unquote(v.Value)
				names = append(names, s)
				continue
			case *ast.Ident:
				if s, ok := consts[v.Name]; ok {
					names = append(names, s)
					continue
				}
			}
			unresolved = append(unresolved, fset.Position(kv.Pos()).String())
		}
	}
	for _, file := range parsed {
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if isEnvVar(lit.Type) {
				record(lit)
			}
			if arr, ok := lit.Type.(*ast.ArrayType); ok && isEnvVar(arr.Elt) {
				for _, elt := range lit.Elts {
					if inner, ok := elt.(*ast.CompositeLit); ok && inner.Type == nil {
						record(inner)
					}
				}
			}
			return true
		})
	}
	return names, unresolved
}

// digestSkills resolves every ref to a fixed digest, as the skill store would.
type digestSkills struct{}

func (digestSkills) Resolve(_ context.Context, _ string, r skill.Ref) (skill.Ref, error) {
	return skill.Ref{Name: r.Name, Version: "sha256:" + strings.Repeat("a", 64)}, nil
}

func (digestSkills) Describe(_ context.Context, _ string, names []string) (map[string]string, error) {
	out := map[string]string{}
	for _, n := range names {
		out[n] = "does " + n
	}
	return out, nil
}

// platformEnvFixture is an agent that turns on everything that sets a platform variable: a tenant with
// model caps, a budget, a guardrail, record mode, a runtime, a prompt, a skill, session and long-term
// memory, a knowledge base and trace export on the dev data plane; as a member, registry membership and a
// supervised team. spec.env is env.
type platformEnvFixture struct {
	member     bool
	proxy      bool
	perUserMem bool
}

func (f platformEnvFixture) build(t *testing.T, env []corev1.EnvVar) (*AgentDeploymentReconciler, *agentsv1beta1.AgentDeployment) {
	t.Helper()
	const ns = "team-a"
	pointer, err := json.Marshal(prompt.ResolvedPointer{Name: "greeting", Repo: "r", Ref: "main", Path: "p.txt"})
	require.NoError(t, err)
	labels := map[string]string{}
	if f.member {
		labels["team"] = "mesh"
	}
	scope := "session"
	if f.member {
		scope = memoryScopeShared
	}
	deploy := &agentsv1beta1.AgentDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "everything", Namespace: ns, Labels: labels,
			Annotations: map[string]string{prompt.ResolvedPromptAnnotation: string(pointer)},
		},
		Spec: agentsv1beta1.AgentDeploymentSpec{
			Image:              "ghcr.io/ctxmesh/example-agent:latest",
			Role:               "worker",
			AllowedCallers:     []string{"peer"},
			Budget:             &agentsv1beta1.BudgetSpec{PerConversationUSD: "1", PerAgentUSD: "5"},
			GuardrailPolicyRef: "policy",
			Record:             true,
			Runtime: &agentsv1beta1.RuntimeSpec{
				OutputSchema: &k8sruntime.RawExtension{Raw: []byte(`{"type":"object"}`)},
			},
			PromptRef:      "greeting",
			SkillRefs:      []string{"summarise@stable"},
			SessionMemory:  &agentsv1beta1.SessionMemorySpec{Scope: scope, PerUser: f.perUserMem},
			LongTermMemory: &agentsv1beta1.LongTermMemorySpec{Enabled: true, PerUser: true, EmbeddingRoute: "embed"},
			KnowledgeBases: []agentsv1beta1.KnowledgeBaseRef{{Name: "kb"}},
			Env:            env,
		},
	}
	objs := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{tenantLabel: "alpha"}}},
		&agentsv1beta1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: "alpha"},
			Spec: agentsv1beta1.TenantSpec{Model: &agentsv1beta1.TenantModelQuota{
				BudgetUSD: "100.00", RPM: 600, MaxConcurrent: 4,
			}},
		},
		&agentsv1beta1.GuardrailPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "policy", Namespace: ns},
			Spec: agentsv1beta1.GuardrailPolicySpec{
				FailMode:        "closed",
				PatternDenylist: []agentsv1beta1.PatternRule{{Name: "a", Pattern: "secret", Action: "block"}},
			},
		},
		&agentsv1beta1.KnowledgeBase{
			ObjectMeta: metav1.ObjectMeta{Name: "kb", Namespace: ns},
			Spec:       agentsv1beta1.KnowledgeBaseSpec{EmbeddingRoute: "embed"},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: telemetry.LangfuseSecretName, Namespace: ns},
			Data: map[string][]byte{
				telemetry.LangfuseKeyEndpoint: []byte("http://langfuse"),
				telemetry.LangfuseKeyPublic:   []byte("pk"),
				telemetry.LangfuseKeySecret:   []byte("sk"),
			},
		},
	}
	if f.member {
		objs = append(objs,
			&agentsv1beta1.AgentRegistry{
				ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: ns},
				Spec: agentsv1beta1.AgentRegistrySpec{
					RegistryId:     "mesh-id",
					MemberSelector: metav1.LabelSelector{MatchLabels: map[string]string{"team": "mesh"}},
				},
			},
			&agentsv1beta1.AgentTeam{
				ObjectMeta: metav1.ObjectMeta{Name: "crew", Namespace: ns},
				Spec: agentsv1beta1.AgentTeamSpec{
					RegistryRef: "mesh",
					Supervisor:  agentsv1beta1.AgentTeamSupervisor{AgentRef: "everything"},
				},
			},
		)
	}
	r := identityReconciler(t, deploy, objs...)
	r.DevDataPlane = true
	r.Skills = digestSkills{}
	r.OBOEgress = OBOEgressConfig{
		SidecarImage:           "test.local/egress-sidecar:test",
		TokenServiceURL:        "http://token-service",
		CapabilityPublicKeyB64: testCapabilityKey,
		CapabilityAudience:     "platform-audience",
	}
	if f.proxy {
		r.StatelayerProxyURL = "http://statelayer-proxy"
	}
	return r, deploy
}

var platformEnvFixtures = map[string]platformEnvFixture{
	"member supervisor, state-layer proxy": {member: true, proxy: true},
	"per-user memory, state-layer proxy":   {perUserMem: true, proxy: true},
	"no state-layer proxy":                 {},
}

// platformEnvNotYetSet are classified variables no path sets today, so no fixture can show them.
var platformEnvNotYetSet = []string{
	envSkillDir, // only once skill bodies are mounted
}

// envNames is the user container's env names, in order, duplicates kept.
func envNames(pod podTemplate) []string {
	out := make([]string, 0, len(pod.containers[0].Env))
	for _, e := range pod.containers[0].Env {
		out = append(out, e.Name)
	}
	return out
}

// The census, run against the real pod: with nothing in spec.env, every variable the platform puts on the
// agent container is classified, none is there twice, and every classified variable is set by some
// fixture (so the list does not keep names nothing sets).
func TestEveryVariableOnThePodIsClassified(t *testing.T) {
	t.Parallel()

	set := map[string]bool{}
	for name, f := range platformEnvFixtures {
		r, deploy := f.build(t, nil)
		pod, err := r.buildPodTemplate(context.Background(), deploy)
		require.NoError(t, err, name)
		names := envNames(pod)
		for _, n := range names {
			assert.Contains(t, platformEnvIndex, n, "%s: %s is on the pod and has no class", name, n)
			assert.Equal(t, 1, countNames(names, n), "%s: %s is on the pod more than once", name, n)
			set[n] = true
		}
	}
	for _, v := range platformEnv {
		if slices.Contains(platformEnvNotYetSet, v.name) {
			assert.False(t, set[v.name], "%s is set now: remove it from platformEnvNotYetSet", v.name)
			continue
		}
		assert.True(t, set[v.name], "no fixture sets %s: extend the fixture, or remove the variable from platformEnv", v.name)
	}
}

func countNames(names []string, n string) int {
	c := 0
	for _, x := range names {
		if x == n {
			c++
		}
	}
	return c
}

// Every classified variable, set in spec.env at once: the pod carries each name once; a variable the
// platform sets for the pod keeps the platform's value; a reserved one is absent where the platform
// leaves it unset; any other keeps the author's value; and the condition names exactly what was dropped,
// in census order.
func TestEveryPlatformVariableInSpecEnvFollowsItsClass(t *testing.T) {
	t.Parallel()

	const authored = "authored"
	spec := make([]corev1.EnvVar, 0, 1+len(platformEnv))
	spec = append(spec, corev1.EnvVar{Name: "KEPT", Value: "1"})
	for _, v := range platformEnv {
		spec = append(spec, corev1.EnvVar{Name: v.name, Value: authored})
	}

	for name, f := range platformEnvFixtures {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r, clean := f.build(t, nil)
			cleanPod, err := r.buildPodTemplate(context.Background(), clean)
			require.NoError(t, err)
			platform := map[string]string{}
			for _, e := range cleanPod.containers[0].Env {
				platform[e.Name] = e.Value
			}

			r, deploy := f.build(t, spec)
			pod, err := r.buildPodTemplate(context.Background(), deploy)
			require.NoError(t, err)
			names := envNames(pod)
			got := map[string]string{}
			for _, e := range pod.containers[0].Env {
				got[e.Name] = e.Value
			}
			for _, n := range names {
				require.Equal(t, 1, countNames(names, n), "%s is on the pod more than once", n)
			}
			assert.Equal(t, "1", got["KEPT"])

			var dropped []string
			for _, v := range platformEnv {
				platformValue, platformSets := platform[v.name]
				switch {
				case platformSets:
					dropped = append(dropped, v.name)
					if v.name == envAgentVersion {
						assert.NotEqual(t, authored, got[v.name], "AGENT_VERSION is the spec's own version")
						continue
					}
					assert.Equal(t, platformValue, got[v.name], "%s: the platform sets it, so its value is used", v.name)
				case reservedEnv(v.name, f.member):
					dropped = append(dropped, v.name)
					assert.NotContains(t, names, v.name, "%s is reserved: spec.env cannot set it", v.name)
				default:
					assert.Equal(t, authored, got[v.name], "%s: the platform does not set it here, so spec.env's is used", v.name)
				}
			}

			var stored agentsv1beta1.AgentDeployment
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(deploy), &stored))
			cond := apimeta.FindStatusCondition(stored.Status.Conditions, conditionPlatformEnvIgnored)
			require.NotNil(t, cond, "the drop is reported")
			assert.Equal(t, metav1.ConditionTrue, cond.Status)
			assert.Equal(t, reasonPlatformEnvSet, cond.Reason)
			assert.Contains(t, cond.Message, "spec.env sets "+strings.Join(dropped, ", ")+", which")
			assert.NotEqual(t, cleanPod.digest, pod.digest, "the drop moves the revision name")
		})
	}
}

// The condition's earlier name is removed, whether or not spec.env still sets anything.
func TestTheIdentityEnvIgnoredConditionIsRetired(t *testing.T) {
	t.Parallel()

	for _, env := range [][]corev1.EnvVar{nil, {{Name: envAgentPort, Value: "9"}}} {
		deploy := &agentsv1beta1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "team-a"},
			Spec:       agentsv1beta1.AgentDeploymentSpec{Image: "ghcr.io/ctxmesh/example-agent:latest", Env: env},
			Status: agentsv1beta1.AgentDeploymentStatus{Conditions: []metav1.Condition{{
				Type: conditionIdentityEnvIgnored, Status: metav1.ConditionTrue, Reason: "IdentityEnvSet",
				LastTransitionTime: metav1.Now(),
			}}},
		}
		r := identityReconciler(t, deploy, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}})
		_, err := r.buildPodTemplate(context.Background(), deploy)
		require.NoError(t, err)
		var stored agentsv1beta1.AgentDeployment
		require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(deploy), &stored))
		assert.Nil(t, apimeta.FindStatusCondition(stored.Status.Conditions, conditionIdentityEnvIgnored))
		assert.Equal(t, env != nil, apimeta.FindStatusCondition(stored.Status.Conditions, conditionPlatformEnvIgnored) != nil)
	}
}
