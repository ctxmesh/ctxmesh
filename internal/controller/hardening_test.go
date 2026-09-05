package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// TestTheAgentContainerIsHardenedByDefault. Before M162 the user container carried NO
// securityContext — the controller set exactly one, on the launcher-inject initContainer, whose
// comment advertised it as hardened. Every agent image therefore ran with whatever the container
// runtime allowed, and the platform's containment story stopped at the pod boundary.
func TestTheAgentContainerIsHardenedByDefault(t *testing.T) {
	t.Parallel()

	sc := agentSecurityContext(false)
	require.NotNil(t, sc, "the default must be hardened, not absent")
	assert.True(t, *sc.RunAsNonRoot)
	assert.False(t, *sc.AllowPrivilegeEscalation)
	require.NotNil(t, sc.Capabilities)
	assert.Equal(t, []corev1.Capability{capabilityAll}, sc.Capabilities.Drop)
	require.NotNil(t, sc.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type)
}

// TestTheProfileOmitsWhatWouldBreakRealImages. ReadOnlyRootFilesystem and a pinned RunAsUser
// are deliberately absent: both break a large fraction of real images (anything writing to /tmp,
// anything with a baked-in uid), and a default that breaks most images is one operators disable
// wholesale — leaving them with LESS protection than a narrower default they keep.
func TestTheProfileOmitsWhatWouldBreakRealImages(t *testing.T) {
	t.Parallel()

	sc := agentSecurityContext(false)
	require.NotNil(t, sc)
	assert.Nil(t, sc.ReadOnlyRootFilesystem, "a read-only rootfs breaks too many images to be a default")
	assert.Nil(t, sc.RunAsUser, "enforce non-root without dictating WHICH non-root uid")
}

// TestUnconfinedIsByteIdenticalToThePreviousShape. Opting out returns nil rather than a
// permissive context, so a declared exception leaves the pod spec exactly as it was before this
// field existed — opting out must not itself change the workload.
func TestUnconfinedIsByteIdenticalToThePreviousShape(t *testing.T) {
	t.Parallel()
	assert.Nil(t, agentSecurityContext(true))
}
