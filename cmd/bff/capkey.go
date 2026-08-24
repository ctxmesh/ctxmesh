/*
Copyright 2026.
Licensed under the Apache License, Version 2.0 (the "License").
*/

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ctxmesh/agent-engine/internal/bootstrap"
)

// runEnsureCapabilityKey is the `bff -ensure-capability-key` mode: the Helm post-install/pre-upgrade hook
// that generates the platform capability keypair into the bff-capability Secret iff absent (never
// re-keys), then rollout-restarts the consumers (M124/Gate A, ADR 0095). Returns a process exit code.
func runEnsureCapabilityKey(ctx context.Context) int {
	logger := log.Log.WithName("ensure-capability-key")
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		logger.Error(fmt.Errorf("POD_NAMESPACE unset"), "cannot resolve the install namespace")
		return 1
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Error(err, "in-cluster config")
		return 1
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.Error(err, "kubernetes client")
		return 1
	}
	sec := &clientSecretOps{cs: cs}
	dep := &clientDeployOps{cs: cs}
	// The env consumers of bff-capability (bff.yaml / control-plane.yaml / run-worker.yaml). run-worker
	// may be absent (dep tolerates NotFound).
	consumers := []string{"agent-engine-bff", "agent-engine-controller-manager", "run-worker"}

	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := bootstrap.EnsureCapabilityKey(cctx, sec, dep, ns, consumers, logger); err != nil {
		logger.Error(err, "ensure-capability-key FAILED")
		return 1
	}
	logger.Info("ensure-capability-key OK")
	return 0
}

// --- client-go adapters (implement the bootstrap interfaces) --------------------------------------

type clientSecretOps struct{ cs kubernetes.Interface }

func (o *clientSecretOps) Get(ctx context.Context, ns, name string) (string, string, bool, error) {
	s, err := o.cs.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	// Secret.Data holds the DECODED bytes (the runcap base64 STRINGS are stored as the value bytes).
	return string(s.Data[bootstrap.PrivateKeyKey]), string(s.Data[bootstrap.PublicKeyKey]), true, nil
}

func (o *clientSecretOps) Create(ctx context.Context, ns, name, priv, pub string) error {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			// Never GC on `helm uninstall`-of-a-single-resource / hook cleanup — keep the key so an
			// upgrade/reinstall doesn't re-key and invalidate every OBO grant (ADR 0095).
			Annotations: map[string]string{"helm.sh/resource-policy": "keep"},
			Labels:      map[string]string{"app.kubernetes.io/name": "agent-engine", "app.kubernetes.io/managed-by": "agent-engine-keygen"},
		},
		Type: corev1.SecretTypeOpaque,
		// stringData: the API stores the runcap base64 STRING verbatim as the value (do NOT hand-base64
		// into Data — that double-encodes; the classic bug).
		StringData: map[string]string{bootstrap.PrivateKeyKey: priv, bootstrap.PublicKeyKey: pub},
	}
	_, err := o.cs.CoreV1().Secrets(ns).Create(ctx, s, metav1.CreateOptions{})
	return err
}

func (o *clientSecretOps) SetPublicKey(ctx context.Context, ns, name, pub string) error {
	// A strategic-merge patch with stringData: the apiserver merges it into data — completes the public
	// key without reading/rewriting (hence never touching) the private seed.
	patch := fmt.Sprintf(`{"stringData":{%q:%q}}`, bootstrap.PublicKeyKey, pub)
	_, err := o.cs.CoreV1().Secrets(ns).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

type clientDeployOps struct{ cs kubernetes.Interface }

func (o *clientDeployOps) RolloutRestart(ctx context.Context, ns, name string) error {
	// Same trigger `kubectl rollout restart` uses: bump a pod-template annotation → a new ReplicaSet.
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().Format(time.RFC3339))
	_, err := o.cs.AppsV1().Deployments(ns).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil // a consumer (e.g. run-worker) may not be deployed — not an error.
	}
	return err
}
