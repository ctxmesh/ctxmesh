package bff

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/ctxmesh/agent-engine/api/v1alpha1"
	"github.com/ctxmesh/agent-engine/internal/prompt"
)

// stampResolvedPrompt (m40.3): the BFF denormalizes the prompt git pointer onto the AgentDeployment so
// the controller reconciles self-contained. Best-effort — unresolved → unstamped (never a create error).
func TestStampResolvedPrompt(t *testing.T) {
	ctx := context.Background()
	src := agentsv1alpha1.GitPromptSource{Repo: "https://git/x.git", Ref: "v1", Path: "p/s.txt"}

	objsFor := func(promptRef string, alongside *agentsv1alpha1.PromptVersion) []decodedObject {
		agent := &agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
			Spec:       agentsv1alpha1.AgentDeploymentSpec{PromptRef: promptRef},
		}
		objs := []decodedObject{{obj: agent, kind: agentDeploymentOwnerKind}}
		if alongside != nil {
			objs = append(objs, decodedObject{obj: alongside, kind: "PromptVersion"})
		}
		return objs
	}
	stampedPointer := func(t *testing.T, objs []decodedObject) (prompt.ResolvedPointer, bool) {
		raw := objs[0].obj.GetAnnotations()[prompt.ResolvedPromptAnnotation]
		if raw == "" {
			return prompt.ResolvedPointer{}, false
		}
		var p prompt.ResolvedPointer
		require.NoError(t, json.Unmarshal([]byte(raw), &p))
		return p, true
	}
	reader := func(pvs ...*agentsv1alpha1.PromptVersion) AgentReader {
		b := fake.NewClientBuilder().WithScheme(testScheme(t))
		for _, pv := range pvs {
			b = b.WithObjects(pv)
		}
		return b.Build()
	}
	mkPV := func(name string) *agentsv1alpha1.PromptVersion {
		return &agentsv1alpha1.PromptVersion{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       agentsv1alpha1.PromptVersionSpec{Git: src},
		}
	}

	t.Run("created alongside (prompt: block) → stamped without a cluster read", func(t *testing.T) {
		objs := objsFor("p1", mkPV("p1"))
		require.Nil(t, stampResolvedPrompt(ctx, reader(), objs, "default")) // empty cluster — resolved from alongside
		p, ok := stampedPointer(t, objs)
		require.True(t, ok)
		assert.Equal(t, "p1", p.Name)
		assert.Equal(t, "v1", p.Ref)
		assert.Equal(t, src.Repo, p.Repo)
	})

	t.Run("existing PromptVersion in the cluster → stamped", func(t *testing.T) {
		objs := objsFor("p2", nil)
		require.Nil(t, stampResolvedPrompt(ctx, reader(mkPV("p2")), objs, "default"))
		p, ok := stampedPointer(t, objs)
		require.True(t, ok)
		assert.Equal(t, "p2", p.Name)
	})

	t.Run("alongside PromptVersion wins over a same-named cluster one", func(t *testing.T) {
		// The alongside copy (being created in this same request) is authoritative over an existing one.
		along := mkPV("dup")
		along.Spec.Git.Ref = "along-ref"
		cluster := mkPV("dup")
		cluster.Spec.Git.Ref = "cluster-ref"
		objs := objsFor("dup", along)
		require.Nil(t, stampResolvedPrompt(ctx, reader(cluster), objs, "default"))
		p, ok := stampedPointer(t, objs)
		require.True(t, ok)
		assert.Equal(t, "along-ref", p.Ref)
	})

	t.Run("unresolved promptRef → 400 error, NOT stamped (so the edit path can fail loud)", func(t *testing.T) {
		objs := objsFor("ghost", nil)
		err := stampResolvedPrompt(ctx, reader(), objs, "default")
		require.NotNil(t, err)
		assert.Equal(t, 400, err.status)
		_, ok := stampedPointer(t, objs)
		assert.False(t, ok)
	})

	t.Run("no promptRef → nil, not stamped", func(t *testing.T) {
		objs := objsFor("", nil)
		require.Nil(t, stampResolvedPrompt(ctx, reader(), objs, "default"))
		_, ok := stampedPointer(t, objs)
		assert.False(t, ok)
	})
}
