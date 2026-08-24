/*
Copyright 2026.
Licensed under the Apache License, Version 2.0 (the "License").
*/

package bff

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ctxmesh/agent-engine/internal/runcap"
)

// TestOBOMintingPrecondition_FailsClosed is the M124/Gate A security-tier guard (ADR 0095 §2):
// a per-user-OBO install with no capability signer must REFUSE to start, never silently downgrade
// to the shared org/public credential. Only the (OBO-required, minting-disabled) cell errors.
func TestOBOMintingPrecondition_FailsClosed(t *testing.T) {
	cases := []struct {
		name        string
		oboRequired bool
		mintingOn   bool
		wantErr     bool
	}{
		{"no-OBO install, no signer — unaffected (dev/org-only)", false, false, false},
		{"no-OBO install, signer present — unaffected", false, true, false},
		{"OBO required, signer present — correct install starts", true, true, false},
		{"OBO required, NO signer — FAIL CLOSED (refuse to serve)", true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := OBOMintingPrecondition(tc.oboRequired, tc.mintingOn)
			if tc.wantErr {
				require.Error(t, err, "OBO required + no minting must fail closed")
				assert.Contains(t, err.Error(), "MCP_OBO_REQUIRED=true")
				assert.Contains(t, err.Error(), "refusing to serve")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestCapabilityMintingEnabled_TracksSeed proves the signal the guard reads: minting is enabled iff
// a valid capability private seed is configured. Absent seed ⇒ disabled ⇒ (with OBO required) the
// precondition refuses — so the process can never reach the runtime path that resolves org/public.
func TestCapabilityMintingEnabled_TracksSeed(t *testing.T) {
	noSeed := NewServer(Options{Log: logr.Discard()})
	assert.False(t, noSeed.CapabilityMintingEnabled(), "no seed ⇒ minting disabled")
	// The fail-closed follows: an OBO-required install built like this refuses to serve.
	require.Error(t, OBOMintingPrecondition(true, noSeed.CapabilityMintingEnabled()))

	_, priv, err := runcap.GenerateKeyPair()
	require.NoError(t, err)
	withSeed := NewServer(Options{Log: logr.Discard(), MCPCapabilityPrivateSeedB64: runcap.EncodePrivateSeed(priv)})
	assert.True(t, withSeed.CapabilityMintingEnabled(), "valid seed ⇒ minting enabled")
	require.NoError(t, OBOMintingPrecondition(true, withSeed.CapabilityMintingEnabled()))
}
