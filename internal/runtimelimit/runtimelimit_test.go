package runtimelimit

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Each case restores the previous limit so the tests do not leak a ceiling into the rest
// of the package's runs.
func withRestore(t *testing.T) {
	t.Helper()
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
}

func TestApply_SetsEightyPercentOfTheContainerLimit(t *testing.T) {
	withRestore(t)
	t.Setenv("GOMEMLIMIT", "")
	t.Setenv("CONTAINER_MEMORY_LIMIT_BYTES", "1073741824") // 1Gi
	got := Apply()
	assert.Equal(t, int64(858993459), got, "80% of 1Gi")
	assert.Equal(t, got, debug.SetMemoryLimit(-1), "the runtime must actually carry it")
}

// An operator's explicit choice is never overridden — the runtime already honours it.
func TestApply_ExplicitGoMemLimitWins(t *testing.T) {
	withRestore(t)
	t.Setenv("GOMEMLIMIT", "512MiB")
	t.Setenv("CONTAINER_MEMORY_LIMIT_BYTES", "1073741824")
	assert.Zero(t, Apply(), "an explicit GOMEMLIMIT must be left alone")
}

// No limit means no limit. Inventing a ceiling for an unconstrained container would make
// the GC collect harder for no reason.
func TestApply_NoLimitIsLeftAlone(t *testing.T) {
	withRestore(t)
	t.Setenv("GOMEMLIMIT", "")
	t.Setenv("CONTAINER_MEMORY_LIMIT_BYTES", "")
	assert.Zero(t, Apply())
}

// A container with no memory limit reports a sentinel near MaxInt64 through the downward
// API; 80% of that is not a limit.
func TestApply_UnboundedSentinelIsIgnored(t *testing.T) {
	withRestore(t)
	t.Setenv("GOMEMLIMIT", "")
	t.Setenv("CONTAINER_MEMORY_LIMIT_BYTES", "9223372036854771712")
	assert.Zero(t, Apply())
}

func TestApply_GarbageIsIgnored(t *testing.T) {
	withRestore(t)
	t.Setenv("GOMEMLIMIT", "")
	for _, bad := range []string{"not-a-number", "-1", "0"} {
		t.Setenv("CONTAINER_MEMORY_LIMIT_BYTES", bad)
		assert.Zero(t, Apply(), "input %q", bad)
	}
}
