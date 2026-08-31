package preflight

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// realPair returns a matching (privSeedB64, pubB64) Ed25519 pair via the same helpers the platform uses.
func realPair(t *testing.T) (string, string) {
	t.Helper()
	pub, priv, err := runcap.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	return runcap.EncodePrivateSeed(priv), runcap.EncodePublicKey(pub)
}

func TestCheck_CoherentConfig_NoErrors(t *testing.T) {
	priv, pub := realPair(t)
	cfg := Config{
		RequiredEnv: map[string]string{
			"TOKEN_SERVICE_URL":         "http://ts.svc:8443",
			"EGRESS_SIDECAR_IMAGE":      "dev.local/egress-sidecar:x",
			"MCP_CAPABILITY_PUBLIC_KEY": pub,
			"MCP_CREDENTIAL_NAMESPACE":  "ctxmesh",
			"COST_ROLLUP_ENABLED":       "1",
		},
		CapabilityPrivateSeed: priv,
		CapabilityPublicKey:   pub,
		ControlPlaneDSN:       "postgres://x",
	}
	errs := Check(context.Background(), cfg, func(context.Context, string) error { return nil })
	if len(errs) != 0 {
		t.Fatalf("expected no errors on a coherent config, got: %v", errs)
	}
}

func TestCheck_MissingRequiredEnv_FailsLoud(t *testing.T) {
	cfg := Config{RequiredEnv: map[string]string{"TOKEN_SERVICE_URL": "", "COST_ROLLUP_ENABLED": "1"}}
	errs := Check(context.Background(), cfg, nil)
	if len(errs) != 1 {
		t.Fatalf("expected exactly 1 error (TOKEN_SERVICE_URL empty), got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "TOKEN_SERVICE_URL") {
		t.Fatalf("error should name the empty key, got: %v", errs[0])
	}
}

func TestCheck_KeypairMismatch_FailsLoud(t *testing.T) {
	priv, _ := realPair(t)
	_, otherPub := realPair(t) // a different pair's public key
	cfg := Config{CapabilityPrivateSeed: priv, CapabilityPublicKey: otherPub}
	errs := Check(context.Background(), cfg, nil)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "MISMATCH") {
		t.Fatalf("expected a keypair MISMATCH error, got: %v", errs)
	}
}

func TestCheck_HalfConfiguredKeypair_FailsLoud(t *testing.T) {
	priv, _ := realPair(t)
	cfg := Config{CapabilityPrivateSeed: priv} // public missing
	errs := Check(context.Background(), cfg, nil)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "HALF-configured") {
		t.Fatalf("expected a HALF-configured error, got: %v", errs)
	}
}

func TestCheck_UnreachableDSN_FailsLoud(t *testing.T) {
	cfg := Config{ControlPlaneDSN: "postgres://nope"}
	ping := func(context.Context, string) error { return errors.New("dial tcp: connection refused") }
	errs := Check(context.Background(), cfg, ping)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "unreachable") {
		t.Fatalf("expected an unreachable-DSN error, got: %v", errs)
	}
}

func TestCheck_AllProblemsReportedTogether(t *testing.T) {
	priv, _ := realPair(t)
	_, otherPub := realPair(t)
	cfg := Config{
		RequiredEnv:           map[string]string{"TOKEN_SERVICE_URL": "", "MCP_CREDENTIAL_NAMESPACE": ""},
		CapabilityPrivateSeed: priv,
		CapabilityPublicKey:   otherPub,
		ControlPlaneDSN:       "postgres://nope",
	}
	errs := Check(context.Background(), cfg, func(context.Context, string) error { return errors.New("refused") })
	// 2 empty env + 1 keypair mismatch + 1 DSN = 4; the operator fixes all in one pass.
	if len(errs) != 4 {
		t.Fatalf("expected 4 aggregated errors, got %d: %v", len(errs), errs)
	}
}
