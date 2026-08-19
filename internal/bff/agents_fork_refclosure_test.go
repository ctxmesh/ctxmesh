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

// Tests for the fork ref-closure policy (m74.4, ADR 0068 §5) — the milestone's only
// genuinely-novel surface. A cross-namespace fork dangles the source-spec's same-namespace
// name-references; the closure detects them, materializes discoverable published tools
// (composing M73 connect — the flywheel compounding), and flags the rest honestly, so a fork
// never silently creates a crash-looping agent.
//
// What we cover:
//   (a) model.route absent in target → needsRebinding "model route: <name>" + the
//       fork-needs-rebinding degraded label is stamped.
//   (b) a tools[] entry that IS a discoverable published MCP server → M73 connect is COMPOSED
//       (the tool materialized into the target ns) with NO secret copied; a non-published tool
//       → needsRebinding "tool: <name>".
//   (c) a promptRef / evalSuiteRef absent in target → unresolvedRefs.
//   (d) inline prompt:/eval: blocks ride free (NOT flagged).
//   (e) a ref-closure error (a catalog read hiccup) does NOT fail the fork (still 201/created).

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/agent-engine/internal/controlplane/promptversion"
	"github.com/ctxmesh/agent-engine/internal/controlplane/publishedartifact"
	"github.com/ctxmesh/agent-engine/internal/controlplane/toolregistry"
)

// newRefClosureServer wires a BFF Server for the ref-closure tests: everything newForkServer
// wires PLUS the toolRegistryStore (the tool class + the published catalog) and a stubbed DNS
// resolver (compose-connect builds an egress NetworkPolicy from the server URL). Origins seed the
// tool store; publishedArt seeds the fork snapshot store; nsStore resolves tenant membership.
func newRefClosureServer(t *testing.T, artStore publishedartifact.Store, nsStore namespacetenant.Store, toolStore toolregistry.Store, seed ...client.Object) (*Server, client.WithWatch) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(seed...).Build()
	// Deterministic egress resolution for compose-connect's NetworkPolicy build.
	stubResolver(t, func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("192.0.2.10")}, nil })
	s := NewServer(Options{
		CallerClients:          &identityCallerFactory{backing: c},
		Scheme:                 testScheme(t),
		Auth:                   AllowAll{},
		MCPEnabled:             true,
		Adapters:               Adapters{Expand: NewExpandAdapter()},
		PublishedArtifactStore: artStore,
		NamespaceTenantStore:   nsStore,
		ToolRegistryStore:      toolStore,
		Version:                "test",
		Log:                    logr.Discard(),
	})
	s.promptStore = promptversion.NewMemStore()
	s.authorizer = &recordingAuthorizer{}
	return s, c
}

// refArtName is the origin name every ref-closure snapshot is published + forked under.
const refArtName = "assistant"

// publishRefArt seeds a public published-artifact snapshot named refArtName in ns with an
// arbitrary source-spec (canonical JSON) — the ref-closure tests need richer specs than
// forkSourceSpec (model/tools/prompt/eval).
func publishRefArt(t *testing.T, store publishedartifact.Store, ns, specJSON string) {
	t.Helper()
	_, err := store.Publish(context.Background(), publishedartifact.PublishedArtifact{
		Kind: kindAgent, OriginNamespace: ns, OriginName: refArtName,
		SpecJSON: json.RawMessage(specJSON), Visibility: visibilityPublic, ContentHash: "hash-ref",
	})
	require.NoError(t, err)
}

// -----------------------------------------------------------------------
// (a) model.route absent → needsRebinding + degraded label
// -----------------------------------------------------------------------

// TestForkRefClosure_MissingModelRoute: a fork whose source-spec references a model.route that
// does NOT exist in the target ns → needsRebinding names the route AND the fork-needs-rebinding
// label is stamped (the model route is secret-adjacent — a ModelRoute wraps a provider key that
// is never copied cross-namespace).
func TestForkRefClosure_MissingModelRoute(t *testing.T) {
	ctx := context.Background()
	art := publishedartifact.NewMemStore()
	// A custom agent (image set) with a model.route that dangles in the target ns.
	spec := `{"name":"assistant","image":"ghcr.io/ctxmesh/echo:v1","model":{"route":"gpt4-prod"}}`
	publishRefArt(t, art, "publisher-ns", spec)
	s, c := newRefClosureServer(t, art, namespacetenant.NewMemStore(), toolregistry.NewMemStore())

	rec := doFork(t, s, "publisher-ns", "assistant", ForkAgentRequest{Name: "local", Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp ForkAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Contains(t, resp.NeedsRebinding, "model route: gpt4-prod")
	assert.Empty(t, resp.UnresolvedRefs)

	// The degraded label is stamped on the forked agent.
	var forked agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: forkCallerNS, Name: "local"}, &forked))
	assert.Equal(t, "true", forked.Labels[labelForkNeedsRebinding], "a needs-rebinding fork must carry the degraded label")
	// U14: the SPECIFIC dangling refs are persisted as an annotation so the detail banner itemizes them.
	assert.Contains(t, forked.Annotations[annForkUnresolvedRefs], "model route: gpt4-prod",
		"the fork must persist its dangling refs for the detail banner to itemize")
}

// TestForkRefClosure_ModelRoutePresent_NoFlag: the SAME fork when a ModelRoute of that name
// already exists in the target ns → NOT flagged, and no degraded label.
func TestForkRefClosure_ModelRoutePresent_NoFlag(t *testing.T) {
	ctx := context.Background()
	art := publishedartifact.NewMemStore()
	spec := `{"name":"assistant","image":"ghcr.io/ctxmesh/echo:v1","model":{"route":"gpt4-prod"}}`
	publishRefArt(t, art, "publisher-ns", spec)
	// Seed the ModelRoute in the caller's ns so the ref resolves.
	route := &agentsv1alpha1.ModelRoute{ObjectMeta: metav1.ObjectMeta{Name: "gpt4-prod", Namespace: forkCallerNS}}
	s, c := newRefClosureServer(t, art, namespacetenant.NewMemStore(), toolregistry.NewMemStore(), route)

	rec := doFork(t, s, "publisher-ns", "assistant", ForkAgentRequest{Name: "local", Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp ForkAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.NeedsRebinding, "a resolvable model route must not be flagged")

	var forked agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: forkCallerNS, Name: "local"}, &forked))
	assert.Empty(t, forked.Labels[labelForkNeedsRebinding], "a clean fork carries no degraded label")
}

// -----------------------------------------------------------------------
// (b) tools[] — compose M73 connect for a published tool; flag a non-published one
// -----------------------------------------------------------------------

// TestForkRefClosure_PublishedTool_ComposesConnect: a managed agent forked with a tool that IS
// offered by a DISCOVERABLE published MCP server → the closure composes M73 connect to
// materialize that server into the target ns (the tool becomes resolvable), the tool is NOT
// flagged, and — the crux — NO secret is copied.
func TestForkRefClosure_PublishedTool_ComposesConnect(t *testing.T) {
	ctx := context.Background()
	// A published, world-readable MCP server in a foreign ns that offers the "search" tool.
	origin := oauthOrigin("publisher-ns", "vendor-mcp", visibilityPublic)
	origin.Labels[labelMCPCredentialSource] = credSourceNone
	toolStore := toolregistry.NewMemStore()
	_, err := toolStore.Upsert(ctx, crdToolRegistryToStore(origin))
	require.NoError(t, err)

	art := publishedartifact.NewMemStore()
	spec := `{"name":"assistant","runtime":"managed","tools":["search"]}`
	publishRefArt(t, art, "author-ns", spec)
	s, back := newRefClosureServer(t, art, namespacetenant.NewMemStore(), toolStore)

	rec := doFork(t, s, "author-ns", "assistant", ForkAgentRequest{Name: "local", Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp ForkAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.NeedsRebinding, "a published tool must be auto-materialized, not flagged; got %v", resp.NeedsRebinding)

	// The published server was materialized into the caller's ns (compose-connect ran).
	copyRec, gErr := toolStore.Get(ctx, forkCallerNS, mcpServerName("vendor-mcp"))
	require.NoError(t, gErr, "the published tool's server must be materialized into the target ns")
	copyCRD := storeToolRegistryToCRD(copyRec)
	require.Len(t, copyCRD.Spec.Tools, 1)
	assert.Equal(t, "search", copyCRD.Spec.Tools[0].Name)

	// THE CRUX: no credential material crossed the boundary — NO Secret was created by the
	// compose-connect (the publisher's token never crosses namespaces).
	var secList corev1.SecretList
	require.NoError(t, back.List(ctx, &secList))
	assert.Empty(t, secList.Items, "compose-connect must copy NO secret — no credential crosses the namespace boundary")
	assert.Empty(t, copyCRD.Annotations[annMCPSecret], "the materialized copy references NO Secret")
}

// TestForkRefClosure_PublishedTool_ResolvedRefs_U9: when a published tool is auto-materialized via
// compose-connect, the fork response must carry the tool name in ResolvedRefs (U9, m76.3) — so the
// UI can celebrate "N tools connected automatically". NeedsRebinding must be empty for the same tool.
func TestForkRefClosure_PublishedTool_ResolvedRefs_U9(t *testing.T) {
	ctx := context.Background()
	origin := oauthOrigin("publisher-ns", "vendor-mcp", visibilityPublic)
	origin.Labels[labelMCPCredentialSource] = credSourceNone
	toolStore := toolregistry.NewMemStore()
	_, err := toolStore.Upsert(ctx, crdToolRegistryToStore(origin))
	require.NoError(t, err)

	art := publishedartifact.NewMemStore()
	spec := `{"name":"assistant","runtime":"managed","tools":["search"]}`
	publishRefArt(t, art, "author-ns", spec)
	s, _ := newRefClosureServer(t, art, namespacetenant.NewMemStore(), toolStore)

	rec := doFork(t, s, "author-ns", "assistant", ForkAgentRequest{Name: "local", Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp ForkAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// U9: the auto-materialized "search" tool must appear in ResolvedRefs.
	assert.Contains(t, resp.ResolvedRefs, "search",
		"a published tool auto-materialized via compose-connect must appear in resolvedRefs (U9)")
	// And NOT in NeedsRebinding — it was resolved, not flagged.
	assert.NotContains(t, resp.NeedsRebinding, "tool: search",
		"an auto-materialized tool must not be in needsRebinding")
	assert.NotNil(t, resp.ResolvedRefs, "resolvedRefs must be [] not null")
}

// TestForkRefClosure_NonPublishedTool_Flags: a managed agent forked with a tool that is NOT
// offered by any discoverable published server → needsRebinding "tool: <name>" (an honest flag
// beats a wrong materialize) + the degraded label.
func TestForkRefClosure_NonPublishedTool_Flags(t *testing.T) {
	ctx := context.Background()
	art := publishedartifact.NewMemStore()
	spec := `{"name":"assistant","runtime":"managed","tools":["mystery-tool"]}`
	publishRefArt(t, art, "author-ns", spec)
	// Empty tool store → no published server offers "mystery-tool".
	s, c := newRefClosureServer(t, art, namespacetenant.NewMemStore(), toolregistry.NewMemStore())

	rec := doFork(t, s, "author-ns", "assistant", ForkAgentRequest{Name: "local", Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp ForkAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Contains(t, resp.NeedsRebinding, "tool: mystery-tool")

	var forked agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: forkCallerNS, Name: "local"}, &forked))
	assert.Equal(t, "true", forked.Labels[labelForkNeedsRebinding])
}

// TestForkRefClosure_AmbiguousTool_Flags: a tool offered by MORE THAN ONE discoverable published
// server is ambiguous — the mapping is not cleanly determinable, so it is flagged rather than
// materializing an arbitrary one (an honest flag beats a wrong materialize).
func TestForkRefClosure_AmbiguousTool_Flags(t *testing.T) {
	ctx := context.Background()
	toolStore := toolregistry.NewMemStore()
	for _, name := range []string{"vendor-a", "vendor-b"} {
		o := oauthOrigin("publisher-ns", name, visibilityPublic)
		o.Labels[labelMCPCredentialSource] = credSourceNone
		// Both offer a tool named "search".
		o.Spec.Tools = []agentsv1alpha1.ToolEntry{{Name: "search"}}
		_, err := toolStore.Upsert(ctx, crdToolRegistryToStore(o))
		require.NoError(t, err)
	}
	art := publishedartifact.NewMemStore()
	spec := `{"name":"assistant","runtime":"managed","tools":["search"]}`
	publishRefArt(t, art, "author-ns", spec)
	s, _ := newRefClosureServer(t, art, namespacetenant.NewMemStore(), toolStore)

	rec := doFork(t, s, "author-ns", "assistant", ForkAgentRequest{Name: "local", Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp ForkAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Contains(t, resp.NeedsRebinding, "tool: search", "an ambiguous tool must be flagged, not arbitrarily materialized")
}

// -----------------------------------------------------------------------
// (c) promptRef / evalSuiteRef by-name → unresolvedRefs
// -----------------------------------------------------------------------

// TestForkRefClosure_MissingPromptRef: a fork whose source-spec references a by-name PromptVersion
// absent in the target ns → unresolvedRefs "prompt: <name>" (NOT needsRebinding — a by-name ref is
// carded for auto-closure, ADR 0068 §5). No degraded label (unresolvedRefs alone is not degraded).
func TestForkRefClosure_MissingPromptRef(t *testing.T) {
	ctx := context.Background()
	art := publishedartifact.NewMemStore()
	spec := `{"name":"assistant","image":"ghcr.io/ctxmesh/echo:v1","promptRef":"shared-prompt"}`
	publishRefArt(t, art, "publisher-ns", spec)
	s, c := newRefClosureServer(t, art, namespacetenant.NewMemStore(), toolregistry.NewMemStore())

	rec := doFork(t, s, "publisher-ns", "assistant", ForkAgentRequest{Name: "local", Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp ForkAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Contains(t, resp.UnresolvedRefs, "prompt: shared-prompt")
	assert.Empty(t, resp.NeedsRebinding)

	var forked agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: forkCallerNS, Name: "local"}, &forked))
	assert.Empty(t, forked.Labels[labelForkNeedsRebinding], "unresolvedRefs alone does not degrade the agent")
}

// TestForkRefClosure_PromptRefPresent_NoFlag: the same fork when the PromptVersion exists in the
// target store → NOT flagged.
func TestForkRefClosure_PromptRefPresent_NoFlag(t *testing.T) {
	ctx := context.Background()
	art := publishedartifact.NewMemStore()
	spec := `{"name":"assistant","image":"ghcr.io/ctxmesh/echo:v1","promptRef":"shared-prompt"}`
	publishRefArt(t, art, "publisher-ns", spec)
	s, _ := newRefClosureServer(t, art, namespacetenant.NewMemStore(), toolregistry.NewMemStore())
	// Seed the PromptVersion in the caller's ns store so the ref resolves.
	_, err := s.promptStore.Create(ctx, promptversion.PromptVersion{Namespace: forkCallerNS, Name: "shared-prompt"})
	require.NoError(t, err)

	rec := doFork(t, s, "publisher-ns", "assistant", ForkAgentRequest{Name: "local", Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp ForkAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.UnresolvedRefs, "a resolvable promptRef must not be flagged")
}

// TestForkRefClosure_MissingEvalSuiteRef: a source-spec that references a by-name EvalSuite (the
// evalSuiteRef class, ADR 0068 §5) absent in the target ns → unresolvedRefs "evalSuite: <name>".
//
// NB (schema reality, verified): the CURRENT expand schema has no source-spec field that carries a
// by-name eval reference through a full fork — a bare `eval:{suite}` is REJECTED by expand
// (eval.dataset/scorers/threshold are required when the block is present → it is always an INLINE
// block that rides free), and a top-level `evalSuiteRef` is not in expand's knownFields (rejected).
// So the evalSuiteRef class is a forward-looking policy: the closure handles it (future-proof), but
// it cannot be exercised through the create path today. This test drives closeForkRefs DIRECTLY
// with an evalSuiteRef-bearing spec to prove the class resolves + flags correctly.
func TestForkRefClosure_MissingEvalSuiteRef(t *testing.T) {
	s, _ := newRefClosureServer(t, publishedartifact.NewMemStore(), namespacetenant.NewMemStore(), toolregistry.NewMemStore())
	r := bearerReq("alice")
	caller, err := s.callerClients.ForRequest(r)
	require.NoError(t, err)

	spec := `{"name":"assistant","image":"i","evalSuiteRef":"quality-gate"}`
	needsRebinding, unresolvedRefs, _ := s.closeForkRefs(r, caller, spec, forkCallerNS, "local")
	assert.Empty(t, needsRebinding)
	assert.Contains(t, unresolvedRefs, "evalSuite: quality-gate")
}

// TestForkRefClosure_EvalSuiteRefPresent_NoFlag: the same evalSuiteRef when the EvalSuite exists in
// the target ns (a caller-scoped CRD GET resolves it) → NOT flagged.
func TestForkRefClosure_EvalSuiteRefPresent_NoFlag(t *testing.T) {
	es := &agentsv1alpha1.EvalSuite{ObjectMeta: metav1.ObjectMeta{Name: "quality-gate", Namespace: forkCallerNS}}
	s, _ := newRefClosureServer(t, publishedartifact.NewMemStore(), namespacetenant.NewMemStore(), toolregistry.NewMemStore(), es)
	r := bearerReq("alice")
	caller, err := s.callerClients.ForRequest(r)
	require.NoError(t, err)

	spec := `{"name":"assistant","image":"i","evalSuiteRef":"quality-gate"}`
	_, unresolvedRefs, _ := s.closeForkRefs(r, caller, spec, forkCallerNS, "local")
	assert.Empty(t, unresolvedRefs, "a resolvable evalSuiteRef must not be flagged")
}

// bearerReq builds a minimal request carrying a bearer token, for direct (non-HTTP-round-trip)
// closeForkRefs unit tests that need a caller client + a request context.
func bearerReq(token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

// -----------------------------------------------------------------------
// (d) inline prompt:/eval: blocks ride free (NOT flagged)
// -----------------------------------------------------------------------

// TestForkRefClosure_InlineBlocksRideFree: a fork whose source-spec carries INLINE prompt: (git
// pointer) and eval: (dataset/scorers/threshold) blocks — these create their OWN target-ns
// objects through the create path, so they must NOT be flagged as dangling refs.
func TestForkRefClosure_InlineBlocksRideFree(t *testing.T) {
	art := publishedartifact.NewMemStore()
	// Inline prompt (git block) + inline eval (full block) — both portable.
	spec := `{"name":"assistant","image":"ghcr.io/ctxmesh/echo:v1",` +
		`"prompt":{"name":"p1","git":{"repo":"https://github.com/x/y","ref":"main","path":"p.md"}},` +
		`"eval":{"suite":"s1","dataset":"ds","threshold":"0.8","scorers":[{"name":"acc","type":"exact"}]}}`
	publishRefArt(t, art, "publisher-ns", spec)
	s, c := newRefClosureServer(t, art, namespacetenant.NewMemStore(), toolregistry.NewMemStore())

	rec := doFork(t, s, "publisher-ns", "assistant", ForkAgentRequest{Name: "local", Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp ForkAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Empty(t, resp.NeedsRebinding, "inline blocks ride free — nothing to rebind")
	assert.Empty(t, resp.UnresolvedRefs, "an inline eval block is portable — not an unresolved evalSuiteRef")

	// No degraded label (a fully-portable fork is healthy).
	var forked agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Namespace: forkCallerNS, Name: "local"}, &forked))
	assert.Empty(t, forked.Labels[labelForkNeedsRebinding])
}

// -----------------------------------------------------------------------
// (e) a ref-closure error does not fail the fork
// -----------------------------------------------------------------------

// errCatalogStore wraps a toolregistry.Store and forces ListCatalog to error, simulating a
// catalog read hiccup during the ref-closure. Every other method delegates (so the create path's
// toolRegistryIndex + compose-connect surfaces are unaffected shape-wise).
type errCatalogStore struct {
	toolregistry.Store
}

func (e errCatalogStore) ListCatalog(context.Context, string, []string) ([]toolregistry.ToolRegistry, error) {
	return nil, assertErrCatalog
}

var assertErrCatalog = errCatalogHiccup{}

type errCatalogHiccup struct{}

func (errCatalogHiccup) Error() string { return "simulated catalog read hiccup" }

// TestForkRefClosure_CatalogError_DoesNotFailFork: the agent is created BEFORE the ref-closure, so
// a ref-closure error (here a catalog read hiccup during the tool class) must NOT fail the fork —
// it degrades to flagging the unresolved tool. The fork still returns 201/created.
func TestForkRefClosure_CatalogError_DoesNotFailFork(t *testing.T) {
	ctx := context.Background()
	art := publishedartifact.NewMemStore()
	spec := `{"name":"assistant","runtime":"managed","tools":["search"]}`
	publishRefArt(t, art, "author-ns", spec)
	toolStore := errCatalogStore{Store: toolregistry.NewMemStore()}
	s, c := newRefClosureServer(t, art, namespacetenant.NewMemStore(), toolStore)

	rec := doFork(t, s, "author-ns", "assistant", ForkAgentRequest{Name: "local", Namespace: forkCallerNS})
	require.Equal(t, http.StatusCreated, rec.Code, "a ref-closure error must NOT fail the fork; body: %s", rec.Body.String())

	var resp ForkAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// Catalog unreadable ⇒ the tool cannot be materialized ⇒ it degrades to a flag (never a 500).
	assert.Contains(t, resp.NeedsRebinding, "tool: search")

	// The agent still exists (the create ran before the closure).
	var forked agentsv1alpha1.AgentDeployment
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: forkCallerNS, Name: "local"}, &forked))
	assert.Equal(t, "true", forked.Labels[labelForkNeedsRebinding])
}

// -----------------------------------------------------------------------
// direct unit tests on the ref-closure primitives (no HTTP)
// -----------------------------------------------------------------------

// TestDanglingEvalSuiteRef_InlineVsByName exercises the inline-vs-by-name discrimination directly.
func TestDanglingEvalSuiteRef_InlineVsByName(t *testing.T) {
	byName := forkRefSpec{}
	byName.Eval = &struct {
		Suite     string          `json:"suite"`
		Dataset   string          `json:"dataset"`
		Scorers   json.RawMessage `json:"scorers"`
		Threshold string          `json:"threshold"`
	}{Suite: "quality-gate"}
	assert.Equal(t, "quality-gate", danglingEvalSuiteRef(&byName), "a bare eval.suite is a by-name ref")

	inline := forkRefSpec{}
	inline.Eval = &struct {
		Suite     string          `json:"suite"`
		Dataset   string          `json:"dataset"`
		Scorers   json.RawMessage `json:"scorers"`
		Threshold string          `json:"threshold"`
	}{Suite: "s1", Dataset: "ds", Threshold: "0.8"}
	assert.Empty(t, danglingEvalSuiteRef(&inline), "an inline eval block is portable — not a dangling ref")

	explicit := forkRefSpec{EvalSuiteRef: "explicit-suite"}
	assert.Equal(t, "explicit-suite", danglingEvalSuiteRef(&explicit))
}

// TestTargetHasPromptVersion_NilStore: a nil prompt store cannot confirm the ref → treated as
// unresolved (surfaced honestly), never a panic.
func TestTargetHasPromptVersion_NilStore(t *testing.T) {
	s := &Server{log: logr.Discard()}
	assert.False(t, s.targetHasPromptVersion(context.Background(), forkCallerNS, "p1"),
		"a nil store cannot confirm the PromptVersion — treat as unresolved")
}
