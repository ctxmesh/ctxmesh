package bff

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// computeTextualLineDiff is a pure line-diff (restored with the CRD-path test deletion, ADR 0044 m44.5b):
// identical → "", and a changed line emits a "-" (old) + "+" (new) with unchanged lines as " " context.
func TestComputeTextualLineDiff(t *testing.T) {
	assert.Empty(t, computeTextualLineDiff("same\n", "same\n"), "identical content → empty diff")

	diff := computeTextualLineDiff("one\ntwo\n", "one\nTWO\nthree\n")
	assert.Contains(t, diff, "-two", "removed line prefixed with -")
	assert.Contains(t, diff, "+TWO", "added line prefixed with +")
	assert.Contains(t, diff, "+three", "appended line prefixed with +")
	assert.Contains(t, diff, " one", "unchanged line is context (space-prefixed)")
	// A pure addition never emits a spurious deletion.
	assert.NotContains(t, strings.ReplaceAll(computeTextualLineDiff("a\n", "a\nb\n"), " a", ""), "-a")
}
