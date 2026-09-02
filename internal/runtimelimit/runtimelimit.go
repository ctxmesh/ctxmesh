// Package runtimelimit teaches the Go runtime about the container memory limit.
//
// WHY THIS EXISTS (M148/m148.12). Go's garbage collector sizes the heap from GOGC — by
// default, grow to roughly twice the live heap — with NO knowledge of the cgroup it runs
// in. So a Go service in Kubernetes does not slow down as it approaches its memory
// limit; it grows straight through it and the kernel kills the process. Raising the
// limit does not fix that, it only moves the wall.
//
// M148 watched exactly that happen. The BFF OOMKilled at 512Mi during its first real
// knowledge ingestion, was measured at a 582Mi peak, raised to 1Gi — and OOMKilled again
// at 1Gi under repeated ingestions, because the ceiling was never the problem. Nothing
// was telling the runtime to collect harder as it ran out of room.
//
// GOMEMLIMIT (Go 1.19+) is the soft limit the GC actually respects: as the heap
// approaches it the collector runs more often, trading throughput for staying alive.
// That is the correct failure mode for a control plane — slower beats dead.
package runtimelimit

import (
	"os"
	"runtime/debug"
	"strconv"
)

// headroom is the fraction of the container limit handed to the Go heap.
//
// The remainder is NOT waste: it covers everything the GC does not account for — the
// runtime's own structures, goroutine stacks, cgo/OS allocations, and the fact that
// GOMEMLIMIT is a SOFT limit the collector aims at rather than a hard cap it enforces. A
// process given 100% of its cgroup as GOMEMLIMIT can still be OOMKilled; the headroom is
// what makes the soft limit useful.
const headroom = 0.8

// Apply sets GOMEMLIMIT from the container memory limit exposed via the downward API
// (CONTAINER_MEMORY_LIMIT_BYTES). It returns the limit it applied, in bytes, or 0 when
// no limit was available — an unlimited or unset container is left entirely alone rather
// than given an invented ceiling.
//
// An explicit GOMEMLIMIT in the environment always wins: the Go runtime already honours
// it, and an operator who set one deliberately must not be silently overridden.
func Apply() int64 {
	if os.Getenv("GOMEMLIMIT") != "" {
		return 0
	}
	raw := os.Getenv("CONTAINER_MEMORY_LIMIT_BYTES")
	if raw == "" {
		return 0
	}
	limit, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || limit <= 0 {
		return 0
	}
	// A container with no memory limit reports a sentinel close to MaxInt64 through the
	// downward API. Applying 80% of that is not a limit, it is a rounding error.
	if limit > (1 << 50) {
		return 0
	}
	applied := int64(float64(limit) * headroom)
	debug.SetMemoryLimit(applied)
	return applied
}
