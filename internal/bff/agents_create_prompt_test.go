package bff

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/promptversion"
	"github.com/ctxmesh/ctxmesh/internal/prompt"
)

// stampResolvedPrompt (m40.3 + ADR 0044): the BFF denormalizes the prompt git pointer onto the
// AgentDeployment — resolved from the Postgres store (PromptVersion is retired) — so the controller
// reconciles self-contained. Best-effort on create (unresolved → unstamped, never a create error); the edit
// path propagates the error. An inline `prompt:` block is written to the store BEFORE this runs
// (createAgentFromYAML), so resolution just reads the store.
func TestStampResolvedPrompt(t *testing.T) {
	ctx := context.Background()

	objsFor := func(promptRef string) []decodedObject {
		agent := &agentsv1alpha1.AgentDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default"},
			Spec:       agentsv1alpha1.AgentDeploymentSpec{PromptRef: promptRef},
		}
		return []decodedObject{{obj: agent, kind: agentDeploymentOwnerKind}}
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
	storeWith := func(t *testing.T, pvs ...promptversion.PromptVersion) promptversion.Store {
		s := promptversion.NewMemStore()
		for _, pv := range pvs {
			_, err := s.Upsert(ctx, pv)
			require.NoError(t, err)
		}
		return s
	}
	pv := func(name string) promptversion.PromptVersion {
		return promptversion.PromptVersion{Namespace: "default", Name: name, Repo: "https://git/x.git", Ref: "v1", Path: "p/s.txt"}
	}

	t.Run("promptRef resolves from the store → stamped", func(t *testing.T) {
		objs := objsFor("p1")
		require.Nil(t, stampResolvedPrompt(ctx, storeWith(t, pv("p1")), objs, "default"))
		p, ok := stampedPointer(t, objs)
		require.True(t, ok)
		assert.Equal(t, "p1", p.Name)
		assert.Equal(t, "v1", p.Ref)
		assert.Equal(t, "https://git/x.git", p.Repo)
	})

	t.Run("unresolved promptRef → 400 error, NOT stamped (so the edit path can fail loud)", func(t *testing.T) {
		objs := objsFor("ghost")
		err := stampResolvedPrompt(ctx, storeWith(t), objs, "default")
		require.NotNil(t, err)
		assert.Equal(t, 400, err.status)
		_, ok := stampedPointer(t, objs)
		assert.False(t, ok)
	})

	t.Run("nil store → 501 error", func(t *testing.T) {
		objs := objsFor("p1")
		err := stampResolvedPrompt(ctx, nil, objs, "default")
		require.NotNil(t, err)
		assert.Equal(t, 501, err.status)
	})

	t.Run("no promptRef → nil, not stamped", func(t *testing.T) {
		objs := objsFor("")
		require.Nil(t, stampResolvedPrompt(ctx, storeWith(t), objs, "default"))
		_, ok := stampedPointer(t, objs)
		assert.False(t, ok)
	})
}
