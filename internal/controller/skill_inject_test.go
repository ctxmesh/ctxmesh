package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// TestOnlyRefsReachThePod. The bodies deliberately do NOT travel with the pod: the launcher
// fetches a description up front and a body only when the model asks. Shipping every attached
// body into the container would defeat the reason skills exist rather than a longer prompt.
func TestOnlyRefsReachThePod(t *testing.T) {
	t.Parallel()

	got := injectSkills([]corev1.Container{{Name: "user"}},
		[]string{"summarise@sha256:aaa", "translate@sha256:bbb"}, nil, false)
	require.Len(t, got, 1)

	var val string
	for _, e := range got[0].Env {
		if e.Name == envSkillRefs {
			val = e.Value
		}
	}
	assert.Equal(t, "summarise@sha256:aaa,translate@sha256:bbb", val)
}

// TestNoSkillsInjectsNothing — an agent that attaches none must be byte-identical to one from
// before the feature existed, or upgrading the platform re-rolls the whole fleet.
func TestNoSkillsInjectsNothing(t *testing.T) {
	t.Parallel()

	in := []corev1.Container{{Name: "user"}}
	got := injectSkills(in, nil, nil, false)
	assert.Empty(t, got[0].Env)
}

// TestInjectionDoesNotDuplicateEnv. A repeated env name is "last wins" in some runtimes and an
// error in others; neither is worth relying on.
func TestInjectionDoesNotDuplicateEnv(t *testing.T) {
	t.Parallel()

	in := []corev1.Container{{Name: "user", Env: []corev1.EnvVar{{Name: envSkillRefs, Value: "stale"}}}}
	got := injectSkills(in, []string{"summarise@sha256:aaa"}, nil, false)

	count := 0
	for _, e := range got[0].Env {
		if e.Name == envSkillRefs {
			count++
			assert.Equal(t, "summarise@sha256:aaa", e.Value, "the stale value must be replaced")
		}
	}
	assert.Equal(t, 1, count)
}

// TestInjectionDoesNotMutateTheInput — buildPodTemplate reuses slices, and an in-place write
// would leak one agent's skills into another's container spec.
func TestInjectionDoesNotMutateTheInput(t *testing.T) {
	t.Parallel()

	in := []corev1.Container{{Name: "user"}}
	_ = injectSkills(in, []string{"summarise@sha256:aaa"}, nil, false)
	assert.Empty(t, in[0].Env, "the caller's slice must be untouched")
}

// TestDescriptionsRideInEnv. GET /skills is called on EVERY run, so it must cost nothing.
// Shipping the descriptions in env lets the launcher answer with no I/O at all — if that call
// were a network round-trip, progressive disclosure would cost more than it saves.
func TestDescriptionsRideInEnv(t *testing.T) {
	t.Parallel()

	got := injectSkills([]corev1.Container{{Name: "user"}},
		[]string{"summarise@sha256:aaa"},
		map[string]string{"summarise": "Summarises long documents."}, false)

	var desc string
	for _, e := range got[0].Env {
		if e.Name == envSkillDescriptions {
			desc = e.Value
		}
	}
	assert.Contains(t, desc, "Summarises long documents.")
}

// TestSkillDirIsSetOnlyWhenBodiesAreMounted. Setting it unconditionally would make the launcher
// answer a 404 "no mounted body" for content that was never staged, instead of the 501 that
// says the capability is absent on this agent — two different facts.
func TestSkillDirIsSetOnlyWhenBodiesAreMounted(t *testing.T) {
	t.Parallel()

	unmounted := injectSkills([]corev1.Container{{Name: "user"}}, []string{"s@sha256:aaa"}, nil, false)
	for _, e := range unmounted[0].Env {
		assert.NotEqual(t, envSkillDir, e.Name, "no mount ⇒ no SKILL_DIR")
	}
	assert.Empty(t, unmounted[0].VolumeMounts)

	mounted := injectSkills([]corev1.Container{{Name: "user"}}, []string{"s@sha256:aaa"}, nil, true)
	var dir string
	for _, e := range mounted[0].Env {
		if e.Name == envSkillDir {
			dir = e.Value
		}
	}
	assert.Equal(t, skillMountPath, dir)
	require.Len(t, mounted[0].VolumeMounts, 1)
	assert.True(t, mounted[0].VolumeMounts[0].ReadOnly, "a mounted body is never writable by the agent")
}
