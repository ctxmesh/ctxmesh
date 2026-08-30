// Package preflight validates that an agentry install is COHERENTLY configured, and fails LOUD
// when it is not — so a misconfiguration surfaces at install time (a failed Helm hook with a clear
// message) instead of silently at runtime as a missing citation, a queued-forever workflow, or a
// per-user credential silently downgraded to a shared one (GA audit's "correct-when-configured,
// silent-when-not" theme; M124/Gate A, ADR 0095).
//
// It is deliberately dependency-light and side-effect-free except for an optional DSN ping: the checks
// are pure functions over a Config, so they are unit-testable without a cluster. The bff binary runs it
// in `-preflight` mode (reusing the deployed image); a Helm post-install hook Job invokes that.
package preflight

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"sort"

	"github.com/ctxmesh/agentry/internal/runcap"
)

// Config is the resolved install configuration the preflight validates. Values are read from the
// environment by the caller (cmd/bff `-preflight`), which mirrors exactly what the control plane sees.
type Config struct {
	// RequiredEnv maps a config key → its resolved value. An empty value fails the check with an
	// actionable message. These are the settings whose silent-empty state the audit found breaks a
	// feature invisibly (TOKEN_SERVICE_URL → KB retrieval, EGRESS_SIDECAR_IMAGE / MCP_CAPABILITY_PUBLIC_KEY
	// → OBO tool calls, MCP_CREDENTIAL_NAMESPACE → grant isolation, COST_ROLLUP_ENABLED → cost).
	RequiredEnv map[string]string

	// The capability keypair. If both are set they MUST be a matching Ed25519 pair (the public key the
	// controller injects into sidecars must verify capabilities the BFF signs with the private seed);
	// a mismatch means OBO verification will reject every real capability. Empty values are handled by
	// RequiredEnv (public) / the fail-closed mint path (private, m124.6), not here.
	CapabilityPrivateSeed string
	CapabilityPublicKey   string

	// ControlPlaneDSN, if set, is pinged (via PingDSN) — a bad DSN is a dead install (the BFF already
	// hard-fails on it at startup, ADR 0044; the preflight surfaces it before the pods crashloop).
	ControlPlaneDSN string
}

// PingDSN reaches the control-plane store and returns an error if it is unreachable. Injected so the
// checks stay unit-testable (a fake in tests, a real sql.Open+PingContext in cmd/bff). Nil skips the
// reachability check (env + keypair coherence still run).
type PingDSN func(ctx context.Context, dsn string) error

// Check runs every coherence check and returns ALL failures (not just the first) so an operator fixes
// the whole misconfiguration in one pass. A nil/empty result means the install is coherent.
func Check(ctx context.Context, cfg Config, ping PingDSN) []error {
	var errs []error

	// 1. Required settings must be non-empty — the silent-feature-off surface.
	for _, k := range sortedKeys(cfg.RequiredEnv) {
		if cfg.RequiredEnv[k] == "" {
			errs = append(errs, fmt.Errorf("required config %s is empty — the chart must default/derive it (a fresh install left a feature silently disabled)", k))
		}
	}

	// 2. Capability keypair must be a matching Ed25519 pair when both are present.
	if cfg.CapabilityPrivateSeed != "" && cfg.CapabilityPublicKey != "" {
		if err := checkKeypair(cfg.CapabilityPrivateSeed, cfg.CapabilityPublicKey); err != nil {
			errs = append(errs, err)
		}
	} else if cfg.CapabilityPrivateSeed != "" || cfg.CapabilityPublicKey != "" {
		errs = append(errs, fmt.Errorf("capability keypair is HALF-configured (only the %s is set) — OBO needs BOTH the private seed (BFF/run-worker sign) and the derived public key (controller→sidecars verify)", halfName(cfg)))
	}

	// 3. Control-plane store must be reachable.
	if cfg.ControlPlaneDSN != "" && ping != nil {
		if err := ping(ctx, cfg.ControlPlaneDSN); err != nil {
			errs = append(errs, fmt.Errorf("control-plane store (CONTROLPLANE_DSN) is unreachable: %w — check the DSN + that the database is up", err))
		}
	}

	return errs
}

// checkKeypair derives the public key from the private seed and compares it to the configured public
// key; a mismatch means every OBO capability the BFF signs will fail verification at the sidecar.
func checkKeypair(privSeedB64, pubB64 string) error {
	priv, err := runcap.DecodePrivateSeed(privSeedB64)
	if err != nil {
		return fmt.Errorf("MCP_CAPABILITY_PRIVATE_KEY is not a valid Ed25519 seed: %w", err)
	}
	if _, err := runcap.DecodePublicKey(pubB64); err != nil {
		return fmt.Errorf("MCP_CAPABILITY_PUBLIC_KEY is not a valid Ed25519 public key: %w", err)
	}
	derived := runcap.EncodePublicKey(priv.Public().(ed25519.PublicKey))
	if derived != pubB64 {
		return fmt.Errorf("capability keypair MISMATCH: MCP_CAPABILITY_PUBLIC_KEY does not match the public key derived from MCP_CAPABILITY_PRIVATE_KEY — OBO tool calls will fail verification (re-derive the public key from the seed, or regenerate the pair)")
	}
	return nil
}

func halfName(cfg Config) string {
	if cfg.CapabilityPrivateSeed != "" {
		return "private seed"
	}
	return "public key"
}

// sortedKeys returns the map keys in deterministic order so the failure list (and tests) are stable.
func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
