package main

import (
	"testing"

	"github.com/go-logr/logr"
)

// Unset, the posture follows whether a launcher can bind at all; an explicit value always wins.
func TestRequireProofOfPossession(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		canBind bool
		want    bool
	}{
		{"", true, true},
		{"", false, false},
		{"false", true, false},
		{" TRUE ", true, true},
	} {
		got, err := requireProofOfPossession(logr.Discard(), tc.raw, tc.canBind)
		if err != nil || got != tc.want {
			t.Errorf("RUNCAP_REQUIRE_POP=%q canBind=%v: got %v, %v; want %v", tc.raw, tc.canBind, got, err, tc.want)
		}
	}
	// Requiring proofs no launcher can obtain is a configuration error, not a posture.
	if _, err := requireProofOfPossession(logr.Discard(), "true", false); err == nil {
		t.Error("RUNCAP_REQUIRE_POP=true without a state layer must stop the BFF")
	}
	// A value that is not a boolean must not silently pick a posture.
	if _, err := requireProofOfPossession(logr.Discard(), "yes-please", true); err == nil {
		t.Error("RUNCAP_REQUIRE_POP must be a boolean")
	}
}
