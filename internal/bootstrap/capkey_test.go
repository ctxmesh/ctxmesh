package bootstrap

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ctxmesh/agentry/internal/runcap"
)

// fakeSecret is an in-memory SecretOps that records what was written.
type fakeSecret struct {
	priv, pub  string
	exists     bool
	createErr  error // if set, Create returns it
	created    bool
	pubPatched bool
}

func (f *fakeSecret) Get(_ context.Context, _, _ string) (string, string, bool, error) {
	return f.priv, f.pub, f.exists, nil
}

func (f *fakeSecret) Create(_ context.Context, _, _, priv, pub string) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.priv, f.pub, f.exists, f.created = priv, pub, true, true
	return nil
}

func (f *fakeSecret) SetPublicKey(_ context.Context, _, _, pub string) error {
	f.pub, f.pubPatched = pub, true
	return nil
}

type fakeDeploy struct {
	restarted []string
	err       error // if set, RolloutRestart returns it
}

func (d *fakeDeploy) RolloutRestart(_ context.Context, _, name string) error {
	if d.err != nil {
		return d.err
	}
	d.restarted = append(d.restarted, name)
	return nil
}

var consumers = []string{"agentry-bff", "agentry-controller-manager", "run-worker"}

func TestEnsure_Absent_GeneratesCreatesRestarts(t *testing.T) {
	sec := &fakeSecret{exists: false}
	dep := &fakeDeploy{}
	changed, err := EnsureCapabilityKey(context.Background(), sec, dep, "ns", consumers)
	if err != nil || !changed {
		t.Fatalf("want changed,nil; got %v,%v", changed, err)
	}
	if !sec.created || sec.priv == "" || sec.pub == "" {
		t.Fatalf("expected a fresh keypair created; sec=%+v", sec)
	}
	// The written pair must be coherent (pub derives from priv).
	if got := mustDerive(t, sec.priv); got != sec.pub {
		t.Fatalf("created pair incoherent: derived %q != stored %q", got, sec.pub)
	}
	if len(dep.restarted) != 3 {
		t.Fatalf("expected all 3 consumers restarted, got %v", dep.restarted)
	}
}

func TestEnsure_BothPresent_NoOp_NeverReKeys(t *testing.T) {
	priv, pub := realPair(t)
	sec := &fakeSecret{priv: priv, pub: pub, exists: true}
	dep := &fakeDeploy{}
	changed, err := EnsureCapabilityKey(context.Background(), sec, dep, "ns", consumers)
	if err != nil || changed {
		t.Fatalf("want false,nil (no re-key); got %v,%v", changed, err)
	}
	if sec.created || sec.pubPatched {
		t.Fatalf("must NOT touch a provisioned Secret; sec=%+v", sec)
	}
	if len(dep.restarted) != 0 {
		t.Fatalf("no restart on a no-op, got %v", dep.restarted)
	}
	if sec.priv != priv {
		t.Fatalf("private seed changed — the never-re-key invariant is broken")
	}
}

func TestEnsure_PrivateOnly_DerivesPublic_NeverTouchesPrivate(t *testing.T) {
	priv, wantPub := realPair(t)
	sec := &fakeSecret{priv: priv, pub: "", exists: true}
	dep := &fakeDeploy{}
	changed, err := EnsureCapabilityKey(context.Background(), sec, dep, "ns", consumers)
	if err != nil || !changed {
		t.Fatalf("want changed,nil; got %v,%v", changed, err)
	}
	if !sec.pubPatched || sec.pub != wantPub {
		t.Fatalf("expected the public key derived+patched to %q, got %q (patched=%v)", wantPub, sec.pub, sec.pubPatched)
	}
	if sec.priv != priv {
		t.Fatalf("private seed changed while completing the public key")
	}
	if len(dep.restarted) != 3 {
		t.Fatalf("expected consumers restarted after the patch, got %v", dep.restarted)
	}
}

func TestEnsure_PublicOnly_FailsLoud(t *testing.T) {
	_, pub := realPair(t)
	sec := &fakeSecret{priv: "", pub: pub, exists: true}
	_, err := EnsureCapabilityKey(context.Background(), sec, &fakeDeploy{}, "ns", consumers)
	if err == nil {
		t.Fatal("public-only Secret is incoherent — expected an error")
	}
}

func TestEnsure_ExistsButEmpty_FailsLoud(t *testing.T) {
	// A BYO operator created bff-capability with the wrong key names / a placeholder → neither key present.
	sec := &fakeSecret{priv: "", pub: "", exists: true}
	dep := &fakeDeploy{}
	changed, err := EnsureCapabilityKey(context.Background(), sec, dep, "ns", consumers)
	if err == nil {
		t.Fatal("an existing-but-empty Secret must fail loud, not pass as provisioned")
	}
	if changed || sec.created {
		t.Fatalf("must not overwrite/regenerate an existing Secret; changed=%v created=%v", changed, sec.created)
	}
}

func TestEnsure_RestartFailure_FailsLoud(t *testing.T) {
	// Key generated, but a consumer restart fails (RBAC drift / apiserver error): the hook must FAIL,
	// not swallow it (a key-less consumer = silently-broken OBO — the thing Gate A kills).
	sec := &fakeSecret{exists: false}
	dep := &fakeDeploy{err: errors.New("forbidden")}
	_, err := EnsureCapabilityKey(context.Background(), sec, dep, "ns", consumers)
	if err == nil {
		t.Fatal("a rollout-restart failure must fail the hook loud")
	}
}

func TestEnsure_CreateRace_TreatedAsProvisioned(t *testing.T) {
	sec := &fakeSecret{exists: false, createErr: apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, SecretName)}
	dep := &fakeDeploy{}
	changed, err := EnsureCapabilityKey(context.Background(), sec, dep, "ns", consumers)
	if err != nil || changed {
		t.Fatalf("an AlreadyExists race should be a clean no-op; got changed=%v err=%v", changed, err)
	}
	if len(dep.restarted) != 0 {
		t.Fatalf("no restart when the create raced, got %v", dep.restarted)
	}
}

func realPair(t *testing.T) (string, string) {
	t.Helper()
	pub, priv, err := runcap.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	return runcap.EncodePrivateSeed(priv), runcap.EncodePublicKey(pub)
}

func mustDerive(t *testing.T, privB64 string) string {
	t.Helper()
	p, err := derivePublic(privB64)
	if err != nil {
		t.Fatalf("derivePublic: %v", err)
	}
	return p
}
