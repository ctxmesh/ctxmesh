// Package bootstrap holds install-time provisioning the chart runs via Helm hooks. EnsureCapabilityKey
// generates the platform Ed25519 capability keypair (ADR 0030) into the `bff-capability` Secret on a
// fresh install so OBO works out-of-box, WITHOUT ever re-keying an existing install (which would
// invalidate every user's OBO grant → mass re-consent). M124/Gate A, ADR 0095 (post-install,pre-upgrade
// hook; generate-iff-absent; the private seed never leaves the Secret).
package bootstrap

import (
	"context"
	"crypto/ed25519"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ctxmesh/ctxmesh/internal/runcap"
)

// isAlreadyExists reports whether a Create raced a parallel hook. Uses the apimachinery matcher so it
// works on the real client-go error; a plain error (in tests) is simply not-already-exists.
func isAlreadyExists(err error) bool { return apierrors.IsAlreadyExists(err) }

// Key names inside the bff-capability Secret (the env names the BFF/controller consume, so a plain
// envFrom/secretKeyRef wires them directly).
const (
	SecretName    = "bff-capability"
	PrivateKeyKey = "MCP_CAPABILITY_PRIVATE_KEY"
	PublicKeyKey  = "MCP_CAPABILITY_PUBLIC_KEY"
)

// SecretOps is the minimal Secret surface EnsureCapabilityKey needs — injected so the logic is
// unit-testable without a cluster (a fake in tests; a client-go adapter in cmd/bff). Values are the
// runcap base64 strings (the env values), not raw bytes.
type SecretOps interface {
	// Get returns the private-seed + public-key values and whether the Secret exists. A missing Secret
	// is (─,─,false,nil), NOT an error.
	Get(ctx context.Context, ns, name string) (privB64, pubB64 string, exists bool, err error)
	// Create writes a new Secret with both keys (stringData). An AlreadyExists error (a race with a
	// parallel hook) is surfaced and the caller treats it as already-provisioned — the winning hook owns
	// the restart. (A rare priv-only race is completed by the next pre-upgrade run.)
	Create(ctx context.Context, ns, name, privB64, pubB64 string) error
	// SetPublicKey patches ONLY the public key onto an existing Secret (a BYO-private-only operator);
	// it must never touch the private seed.
	SetPublicKey(ctx context.Context, ns, name, pubB64 string) error
}

// DeployOps rollout-restarts a consumer Deployment. Implementations MUST treat NotFound as success
// (a run-worker may not be deployed) — return nil.
type DeployOps interface {
	RolloutRestart(ctx context.Context, ns, name string) error
}

// EnsureCapabilityKey makes the bff-capability Secret coherent and returns whether it changed anything:
//   - absent            → generate a fresh pair, Create it;
//   - private-only (BYO)→ derive the public key from the seed, patch only the public key;
//   - both present      → no-op (NEVER re-key);
//
// and, if it changed the Secret, rollout-restarts the consumers so their pods pick up the env at start
// (on a fresh install the Deployments race the post-install hook and resolve env only at container
// start — without the restart a pod that started key-less stays key-less forever).
// The logger is carried on the context (logf.IntoContext at the call site), not passed alongside it —
// a function takes a context OR a logger, never both.
func EnsureCapabilityKey(
	ctx context.Context, sec SecretOps, dep DeployOps, ns string, consumers []string,
) (changed bool, err error) {
	log := logf.FromContext(ctx)
	priv, pub, exists, err := sec.Get(ctx, ns, SecretName)
	if err != nil {
		return false, fmt.Errorf("get %s: %w", SecretName, err)
	}

	switch {
	case !exists:
		p, s, gErr := generate()
		if gErr != nil {
			return false, gErr
		}
		if cErr := sec.Create(ctx, ns, SecretName, s, p); cErr != nil {
			// A parallel hook may have created it first — treat as already-provisioned, not an error.
			if isAlreadyExists(cErr) {
				log.Info("bff-capability already created by a concurrent hook — skipping", "secret", SecretName)
				return false, nil
			}
			return false, fmt.Errorf("create %s: %w", SecretName, cErr)
		}
		log.Info("generated the platform capability keypair", "secret", SecretName)
		changed = true

	case priv != "" && pub == "":
		// BYO private-only: complete the public key WITHOUT touching the private seed.
		derived, dErr := derivePublic(priv)
		if dErr != nil {
			return false, fmt.Errorf("derive public key from the existing private seed: %w", dErr)
		}
		if pErr := sec.SetPublicKey(ctx, ns, SecretName, derived); pErr != nil {
			return false, fmt.Errorf("patch public key onto %s: %w", SecretName, pErr)
		}
		log.Info("derived + set the public key on an existing private-only bff-capability", "secret", SecretName)
		changed = true

	case priv == "" && pub != "":
		// Public-only is incoherent (nothing can sign) — loud, but do not fabricate a private key.
		return false, fmt.Errorf("%s has a public key but no private seed — it cannot sign capabilities; provide the matching MCP_CAPABILITY_PRIVATE_KEY or delete the Secret to regenerate", SecretName)

	case priv == "" && pub == "":
		// Exists but EMPTY (e.g. a BYO operator created it with the wrong data-key names, or a placeholder).
		// Do NOT silently pass it off as provisioned — that is exactly the silent-when-not failure this
		// milestone kills. We also cannot generate INTO it (never-overwrite), so fail loud.
		return false, fmt.Errorf("%s exists but contains neither MCP_CAPABILITY_PRIVATE_KEY nor MCP_CAPABILITY_PUBLIC_KEY — populate it with a keypair (or delete it so the keygen regenerates); the platform cannot mint capabilities without a key", SecretName)

	default:
		// Both present → the never-re-key invariant: do nothing.
		log.Info("bff-capability already provisioned — no change", "secret", SecretName)
	}

	if changed {
		for _, name := range consumers {
			// FAIL LOUD: a consumer that did NOT restart keeps running with the pre-keygen (empty) env and
			// OBO is silently dead. A non-NotFound restart error aborts the hook (with backoffLimit:0 so it
			// fails the install rather than retrying-to-a-no-op). NotFound is tolerated by the adapter (nil).
			if rErr := dep.RolloutRestart(ctx, ns, name); rErr != nil {
				return true, fmt.Errorf("rollout-restart %s (needed to pick up the new capability key): %w", name, rErr)
			}
			log.Info("requested rollout-restart to pick up the capability key", "deployment", name)
		}
	}
	return changed, nil
}

func generate() (pubB64, privB64 string, err error) {
	pub, priv, err := runcap.GenerateKeyPair()
	if err != nil {
		return "", "", fmt.Errorf("generate keypair: %w", err)
	}
	return runcap.EncodePublicKey(pub), runcap.EncodePrivateSeed(priv), nil
}

func derivePublic(privSeedB64 string) (string, error) {
	priv, err := runcap.DecodePrivateSeed(privSeedB64)
	if err != nil {
		return "", err
	}
	return runcap.EncodePublicKey(priv.Public().(ed25519.PublicKey)), nil
}
