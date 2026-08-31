/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	eventingv1 "knative.dev/eventing/pkg/apis/eventing/v1"
	servingv1 "knative.dev/serving/pkg/apis/serving/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	agentsv1alpha1 "github.com/ctxmesh/ctxmesh/api/v1alpha1"
	agentsv1beta1 "github.com/ctxmesh/ctxmesh/api/v1beta1"
	"github.com/ctxmesh/ctxmesh/internal/audit"
	"github.com/ctxmesh/ctxmesh/internal/controller"
	"github.com/ctxmesh/ctxmesh/internal/controlplane"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/agentcapability"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/alertstore"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/auditlog"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/costrollup"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/enduseragent"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/killscope"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/knowledge"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/namespacetenant"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/onlinescore"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/spawnbudget"
	"github.com/ctxmesh/ctxmesh/internal/controlplane/toolregistry"
	"github.com/ctxmesh/ctxmesh/internal/ingestion"
	"github.com/ctxmesh/ctxmesh/internal/kedatypes"
	"github.com/ctxmesh/ctxmesh/internal/objectstore"
	"github.com/ctxmesh/ctxmesh/internal/pki"
	"github.com/ctxmesh/ctxmesh/internal/prompt"
	"github.com/ctxmesh/ctxmesh/internal/promql"
	"github.com/ctxmesh/ctxmesh/internal/run"
	enginewebhook "github.com/ctxmesh/ctxmesh/internal/webhook"
	// +kubebuilder:scaffold:imports
)

// approvalRunLister adapts the durable run store's ListWaitingApproval to the controller's
// ApprovalRunLister (M75, ADR 0069 §3): it maps run.WaitingApproval → controller.WaitingApprovalRun so
// the AlertPolicy reconciler stays decoupled from the run package. It exposes ONLY the read — no run
// mutation reaches the reconciler.
type approvalRunLister struct {
	store interface {
		ListWaitingApproval(
			ctx context.Context, namespace string, kinds []run.ActionKind, limit int,
		) ([]run.WaitingApproval, error)
	}
}

func (a approvalRunLister) ListWaitingApproval(
	ctx context.Context, namespace string,
) ([]controller.WaitingApprovalRun, error) {
	// plan_approval only, unbounded (the AlertPolicy notification path).
	waiting, err := a.store.ListWaitingApproval(ctx, namespace, []run.ActionKind{run.ActionPlanApproval}, 0)
	if err != nil {
		return nil, err
	}
	out := make([]controller.WaitingApprovalRun, 0, len(waiting))
	for _, w := range waiting {
		out = append(out, controller.WaitingApprovalRun{ID: w.ID, Agent: w.Agent, Message: w.Message})
	}
	return out, nil
}

// ingestionRunStatusReader adapts the durable run store's Get to the KnowledgeBase controller's
// terminal-failed safety-net (M80, ADR 0061 Fork 2): it returns the referenced ingestion Run's raw
// status string so the controller can un-stick a KB stuck at phase Ingesting when the run terminated
// out-of-band (no corpus-status row was written). READ-ONLY — no run mutation reaches the reconciler,
// and no KB-status RBAC is granted to the run-worker (ADR 0011: the CONTROLLER writes KB.status).
type ingestionRunStatusReader struct {
	store interface {
		Get(id string) (*run.Run, error)
	}
}

func (a ingestionRunStatusReader) IngestionRunStatus(
	_ context.Context, runID string,
) (status string, found bool, err error) {
	rn, gErr := a.store.Get(runID)
	if errors.Is(gErr, run.ErrNotFound) {
		return "", false, nil // the run row is gone (swept) — the safety-net leaves phase untouched.
	}
	if gErr != nil {
		return "", false, gErr
	}
	return string(rn.Status), true, nil
}

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// envValueTrue is the canonical "on" value for the boolean-ish env toggles the controller reads
// (a static "true" the chart/kustomize sets). One const so the several os.Getenv()=="true" gates
// share a spelling (goconst).
const envValueTrue = "true"

// Platform PKI constants (M128/Gate E, ADR 0102): the in-process cert-controller's
// artifact contract — the serving-cert Secret, the webhook Service (the cert SAN + the
// VWC clientConfig target), and the default local cert dir the controller-runtime webhook
// server reads (certwatcher hot-reload). These are GA-frozen wire contract (ADR 0102 §Consequences).
//
// Both the Secret and Service names are the config/default namePrefix (`ctxmesh-`) DEPLOYED forms:
// their manifests are named `webhook-server-cert` / `webhook-service` (pre-prefix) so kustomize renders
// them to exactly these constants, keeping the shipped resource, the runtime VWC clientConfig, and the
// cert SAN in agreement (M134: the fresh-boot proof caught the Service constant left un-prefixed while
// its manifest gets prefixed — an un-shipped contract mismatch, corrected before first ship).
const (
	webhookCertSecretName = "ctxmesh-webhook-server-cert"
	webhookServiceName    = "ctxmesh-webhook-service"
	defaultWebhookCertDir = "/tmp/k8s-webhook-server/serving-certs"
)

// Platform cert-controller RBAC (M128/Gate E, ADR 0102): the in-process rotator manages its
// own CA + serving-cert Secret and injects the caBundle into the tenant-label VWC. secrets
// create/update is already granted elsewhere for the manager; the VWC verbs are new here.
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update

// managerNamespace resolves the manager's install namespace (where the cert Secret lives +
// the webhook Service resolves): POD_NAMESPACE (downward API) first, then the ServiceAccount
// namespace file, then the install default. Used by the cert-controller (ADR 0102).
func managerNamespace() string {
	if ns := strings.TrimSpace(os.Getenv("POD_NAMESPACE")); ns != "" {
		return ns
	}
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "ctxmesh"
}

// selfControllerUsername asks the API server for the manager's OWN identity via SelfSubjectReview
// (authentication.k8s.io/v1) — the exact username the API server stamps into admission requests. This
// derives the tenant-label webhook's allowed principal BY CONSTRUCTION (M134, ADR 0102), removing the
// namePrefix-fragile TENANT_WEBHOOK_CONTROLLER_SA env literal as a wedge source. Uses a direct clientset
// (the manager cache isn't started yet). Granted to all authenticated principals via system:basic-user.
func selfControllerUsername(ctx context.Context, cfg *rest.Config) (string, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("build clientset for SelfSubjectReview: %w", err)
	}
	ssr, err := cs.AuthenticationV1().SelfSubjectReviews().Create(
		ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("SelfSubjectReview: %w", err)
	}
	u := strings.TrimSpace(ssr.Status.UserInfo.Username)
	if u == "" {
		return "", errors.New("SelfSubjectReview returned an empty username")
	}
	return u, nil
}

// runCleanupTenantVWC deletes the manager-owned tenant-label VWC and returns nil on success OR NotFound
// (M134, ADR 0102 — the Helm pre-delete hook). The VWC is created at runtime, so Helm can't GC it on
// uninstall; without this a fail-closed webhook with a dead backend would linger cluster-wide.
func runCleanupTenantVWC(ctx context.Context) error {
	cs, err := kubernetes.NewForConfig(ctrl.GetConfigOrDie())
	if err != nil {
		return fmt.Errorf("build clientset for VWC cleanup: %w", err)
	}
	name := enginewebhook.TenantLabelVWCName
	err = cs.AdmissionregistrationV1().ValidatingWebhookConfigurations().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ValidatingWebhookConfiguration %q: %w", name, err)
	}
	setupLog.Info("tenant-label VWC cleanup complete (deleted or already absent)", "vwc", name)
	return nil
}

// statelayerProxylessWarning returns a startup warning (and true) when the controller is proxy-less
// (STATELAYER_PROXY_URL unset) — an UNSUPPORTED combination with the default network isolation (C21,
// m52 / Fable audit P2-7). Proxy-less injects a DIRECT MEMORY_BACKEND_ADDR/TENANT_QUOTA_ADDR, but the
// M94/M97 default network isolation blocks cross-namespace :6379, so a proxy-less AND tenant-isolated
// install has memory fail-OPEN (silent loss) + budget fail-CLOSED (402). A set URL ⇒ ("", false): the
// supported default. Kept as a pure function so the preflight is unit-testable without a live manager.
func statelayerProxylessWarning(statelayerProxyURL string) (string, bool) {
	if strings.TrimSpace(statelayerProxyURL) != "" {
		return "", false
	}
	return "STATELAYER_PROXY_URL is unset (proxy-less / direct-Valkey mode): UNSUPPORTED for " +
		"tenant-isolated installs — the default network isolation (M94/M97) blocks cross-namespace :6379, " +
		"so agent memory fails OPEN (silent loss) and budget fails CLOSED (402). Set STATELAYER_PROXY_URL " +
		"for any install with network isolation enabled.", true
}

// launcherImageDigestWarning returns a startup warning (and true) when LAUNCHER_IMAGE is set but NOT
// digest-pinned (…@sha256:…) — C8b (ADR 0089). LAUNCHER_IMAGE is fleet-RCE-equivalent config: it becomes
// PID 1 in every injected agent pod, so it MUST be a digest, not a mutable tag a registry push could swap.
// The controller already refuses to INJECT a non-pinned launcher (launcherInjectionReady, fail-safe); this
// surfaces the misconfig ONCE at startup (loud) rather than only per-agent-reconcile. Empty (injection off)
// or a digest-pinned value ⇒ ("", false). Cosign signature verification is DEFERRED (m52.C8b-cosign): the
// digest pin already defeats mutable-tag + registry tampering (a digest is content-addressed), and the
// LAUNCHER_IMAGE setter is cluster-admin — cosign's marginal provenance value doesn't justify the sigstore
// dependency + key management now. Pure function so the preflight is unit-testable.
func launcherImageDigestWarning(launcherImage string) (string, bool) {
	img := strings.TrimSpace(launcherImage)
	if img == "" || strings.Contains(img, "@sha256:") {
		return "", false
	}
	return "LAUNCHER_IMAGE is set but NOT digest-pinned (…@sha256:…): launcher injection will be SKIPPED " +
		"(fail-safe) because a mutable tag is fleet-RCE-equivalent (it becomes PID 1 in every agent pod). " +
		"Pin LAUNCHER_IMAGE to a @sha256: digest to enable injection.", true
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(agentsv1alpha1.AddToScheme(scheme))
	// v1beta1 carries the AgentTeam kind (M64) the AgentTeam controller reconciles + the AgentDeployment
	// controller reads; it MUST be registered or the manager fails to construct those controllers (the
	// v1alpha1 Hub covers the graduated CRDs, but AgentTeam is v1beta1-only).
	utilruntime.Must(agentsv1beta1.AddToScheme(scheme))
	utilruntime.Must(servingv1.AddToScheme(scheme))
	utilruntime.Must(eventingv1.AddToScheme(scheme))
	utilruntime.Must(kedatypes.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// splitCommaList parses a comma-separated env list, trimming blanks. Used for the egress-redirect CIDR
// exclusions, where a stray empty entry would otherwise become a malformed iptables rule.
func splitCommaList(raw string) []string {
	out := []string{}
	for p := range strings.SplitSeq(raw, ",") {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	// cleanup-tenant-vwc (M134, ADR 0102): one-shot mode for the Helm pre-delete hook — delete the
	// manager-owned tenant-label VWC and exit, so `helm uninstall` never orphans a fail-closed webhook
	// (the VWC is created at runtime, not a Helm resource, so Helm can't GC it). Idempotent (NotFound
	// is success); runs whether or not the webhook was ever enabled.
	var cleanupTenantVWC bool
	flag.BoolVar(&cleanupTenantVWC, "cleanup-tenant-vwc", false,
		"Delete the tenant-label ValidatingWebhookConfiguration and exit (Helm pre-delete hook).")
	// Production-safe logging by default (OTH-5): Development=true uses a console encoder +
	// DPanic-level stacktraces meant for local dev, not the prod manager. Default to structured
	// (JSON) production logging; an operator can still opt into dev logging via --zap-devel.
	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// M134 pre-delete hook: run the one-shot VWC cleanup BEFORE any manager wiring, then exit.
	if cleanupTenantVWC {
		if err := runCleanupTenantVWC(context.Background()); err != nil {
			setupLog.Error(err, "cleanup-tenant-vwc failed")
			os.Exit(1)
		}
		os.Exit(0)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	// Platform PKI (M128/Gate E, ADR 0102): when the tenant-label webhook is enabled but no
	// external cert path is supplied, default to the standard local cert dir. The in-process
	// cert-controller (below) writes the serving cert THERE and controller-runtime's certwatcher
	// hot-reloads it — no cert-manager, no external cert, no first-boot Secret-mount race.
	if webhookCertPath == "" && os.Getenv("ENABLE_TENANT_LABEL_WEBHOOK") == envValueTrue {
		webhookCertPath = defaultWebhookCertDir
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsServerOptions,
		// SEC-3: never cache Secrets on the manager client. The ModelRoute reconciler reads
		// provider-key Secrets from ARBITRARY tenant namespaces (SecretBinding.spec.secretRef)
		// — a full-object Secret cache mirrors EVERY Secret in the cluster into manager memory
		// (128Mi → OOM) and puts every tenant's secret material in one process. Live
		// (uncached) Secret reads keep memory bounded AND preserve the syncGatewaySecrets
		// clobber-guard (a filtered/absent cache would return NotFound for an unlabelled
		// pre-existing Secret → overwrite it). Rotation events still fire via the
		// metadata-only Watch (builder.OnlyMetadata) in the ModelRoute reconciler.
		Client:                 client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		// Domain matches the API group ctxmesh.ai (was ctxmesh.io — a kubebuilder-init
		// inconsistency, OTH-5). Safe to rename pre-release: no deployed manager holds the old
		// lease, so there is no cross-upgrade dual-leader window.
		LeaderElectionID: "7ab0b236.ctxmesh.ai",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// OBO egress-sidecar injection (ADR 0030). Default-off (no drift); a values-gated Helm
	// install turns it on and provides the sidecar image, the platform capability public key,
	// and the locked credential namespace. Shared by the deployment (injects the sidecar) and
	// binding (rewrites remote endpoints) reconcilers.
	oboEgress := controller.OBOEgressConfig{
		Enabled:                os.Getenv("MCP_OBO_EGRESS_ENABLED") == envValueTrue,
		SidecarImage:           os.Getenv("EGRESS_SIDECAR_IMAGE"),
		CapabilityPublicKeyB64: os.Getenv("MCP_CAPABILITY_PUBLIC_KEY"),
		CapabilityAudience:     os.Getenv("MCP_CAPABILITY_AUDIENCE"),
		CredentialNamespace:    os.Getenv("MCP_CREDENTIAL_NAMESPACE"),
		TokenServiceURL:        os.Getenv("TOKEN_SERVICE_URL"),
		// M146.7 (m52.M143-knobs): the two tool-call timeouts finally get a config surface. Both were
		// read by cmd/egress-sidecar and set by NOTHING, so an operator with a legitimately slow tool
		// had no supported way to change them. Empty ⇒ the sidecar's own defaults, so an install that
		// sets neither is byte-unchanged.
		ResponseHeaderTimeout: os.Getenv("EGRESS_RESPONSE_HEADER_TIMEOUT"),
		StreamIdleTimeout:     os.Getenv("EGRESS_STREAM_IDLE_TIMEOUT"),
	}
	if oboEgress.Enabled {
		setupLog.Info("OBO egress-sidecar injection ENABLED (ADR 0030)",
			"sidecarImage", oboEgress.SidecarImage, "delegating", oboEgress.TokenServiceURL != "")
	}

	// ToolRegistry is retired to Postgres (ADR 0044 / M45): the CRD no longer exists,
	// so the operator reads ToolRegistries from the control-plane store and drives
	// binding re-validation from the leader-elected poll source (the CRD-watch
	// replacement). CONTROLPLANE_DSN is therefore REQUIRED — there is no CRD to fall
	// back to. OpenDB runs the goose migrations (session-locked) at start-up.
	cpDSN := strings.TrimSpace(os.Getenv("CONTROLPLANE_DSN"))
	if cpDSN == "" {
		setupLog.Error(nil, "CONTROLPLANE_DSN is required: ToolRegistry is retired to Postgres "+
			"(ADR 0044) — there is no CRD to read")
		os.Exit(1)
	}
	cpDB, cpErr := controlplane.OpenDB(context.Background(), cpDSN)
	if cpErr != nil {
		setupLog.Error(cpErr, "Failed to open the control-plane store (CONTROLPLANE_DSN)")
		os.Exit(1)
	}
	defer func() { _ = cpDB.Close() }()
	toolRegistryStore := toolregistry.NewPostgresStore(cpDB)

	// Read-switch: ONE shared Postgres reader for BOTH the MCPToolBinding and
	// AgentDeployment reconcilers — they compute the pushed manifest and the pod
	// template from the same resolveAgentBindings logic, so a split reader would
	// silently drift them. The leader-elected poll source replaces the CRD watch.
	registryReader := controller.NewPostgresRegistryReader(toolRegistryStore)
	registryChangesCh := make(chan event.GenericEvent)
	if err := mgr.Add(controller.NewRegistryPollSource(toolRegistryStore, registryChangesCh)); err != nil {
		setupLog.Error(err, "Failed to add the ToolRegistry poll source")
		os.Exit(1)
	}
	var registryChanges <-chan event.GenericEvent = registryChangesCh
	setupLog.Info("ToolRegistry served from Postgres (ADR 0044): read-switch + poll source active")

	// C21 (m52 / Fable audit P2-7): a proxy-less controller (STATELAYER_PROXY_URL unset) injects
	// DIRECT MEMORY_BACKEND_ADDR/TENANT_QUOTA_ADDR, but the M94/M97 default network isolation blocks
	// cross-namespace :6379 — so a proxy-less AND tenant-isolated install has memory fail-OPEN + budget
	// fail-CLOSED. Warn loudly at startup (not a hard fail — an intentionally proxy-less, isolation-off
	// dev install is legitimate). The default install SETS the proxy URL, so this only fires on drift.
	statelayerProxyURL := strings.TrimSpace(os.Getenv("STATELAYER_PROXY_URL"))
	if msg, warn := statelayerProxylessWarning(statelayerProxyURL); warn {
		setupLog.Info("startup preflight WARNING (C21): " + msg)
	}
	// C8b (ADR 0089): LAUNCHER_IMAGE is fleet-RCE-equivalent config — warn loudly at startup if it is set
	// but not digest-pinned (the controller then fail-safe SKIPS injection). Cosign verify deferred.
	if msg, warn := launcherImageDigestWarning(os.Getenv("LAUNCHER_IMAGE")); warn {
		setupLog.Info("startup preflight WARNING (C8b): " + msg)
	}

	if err := (&controller.AgentDeploymentReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(), // uncached telemetry-Secret read (collector env stability)
		Scheme:    mgr.GetScheme(),
		OBOEgress: oboEgress,
		// End-user AGENT exposure mirror (M137/EU1b, ADR 0107): the reconciler writes an endUserAccess
		// agent's endpoint + spec here so the BFF resolves an end-user run without a K8s read.
		EndUserAgentStore: enduseragent.NewPostgresStore(cpDB),
		// Capability registry (M141, ADR 0120): the reconciler registers an agent's capability descriptor
		// here so the BFF's discovery path can rank a registry's agents against a capability query — again
		// with no K8s read on the BFF SA.
		AgentCapabilityStore: agentcapability.NewPostgresStore(cpDB),
		// Declared per-team spawn budget (M142.6, m52.C19b): the controller can read AgentTeam, the BFF
		// cannot (ADR 0011), so the number the BFF's authoritative gate enforces is projected here.
		SpawnBudgetStore: spawnbudget.NewPostgresStore(cpDB),
		// spec.suspend projection (M146.6, ADR 0126 §4): the BFF cannot read AgentDeployment, so the
		// declarative halt reaches the worker + the run-create edge through this mirror.
		KillScopeStore: killscope.NewPostgresStore(cpDB),
		// M146.7 (m52.M143-knobs): how long to wait for a referenced KnowledgeBase to appear before
		// deploying without it. Zero ⇒ the 30s default; NEGATIVE disables the settle gate entirely
		// (pre-M143 behaviour, accepting the revision churn) — an escape hatch, not a setting to reach
		// for. Controller-side only, so unlike the egress timeouts it has no digest interaction.
		KnowledgeSettleWindow: envDuration("KNOWLEDGE_SETTLE_WINDOW"),
		// Injected sidecar image overrides (audit OPS-1): empty ⇒ the dev.local defaults,
		// which ImagePullBackOff off a kind cluster, so a real install sets these.
		// The L4 egress redirect (M142.4, ADR 0123): OFF unless explicitly enabled AND given an image.
		// Default-off because it rewrites every agent pod's networking — the M128/M134 class of change
		// that ships as a proven mechanism and is flipped deliberately, never defaulted on at birth.
		EgressRedirect: controller.EgressRedirectConfig{
			Enabled:      strings.TrimSpace(os.Getenv("EGRESS_REDIRECT_ENABLED")) == envValueTrue,
			Image:        strings.TrimSpace(os.Getenv("EGRESS_REDIRECT_IMAGE")),
			ExcludeCIDRs: splitCommaList(os.Getenv("EGRESS_REDIRECT_EXCLUDE_CIDRS")),
		},
		CollectorImage: strings.TrimSpace(os.Getenv("COLLECTOR_IMAGE")),
		DiscoveryImage: strings.TrimSpace(os.Getenv("DISCOVERY_IMAGE")),
		// Dev data plane gate (OPS-2): whether to inject the DEV-ONLY object-store + Langfuse
		// feedback creds into agent pods. From DEV_DATA_PLANE, which the chart templates from
		// .Values.devDataPlane.enabled (true == the kustomize dev posture; profile=production sets
		// it false). False ⇒ neither dev credential family is injected, so a production render
		// never ships the bundled dev.local creds.
		DevDataPlane: strings.TrimSpace(os.Getenv("DEV_DATA_PLANE")) == envValueTrue,
		// State-layer proxy URL (M51, ADR 0050 §8 phase 1): opt-in. Set ⇒ memory-bound
		// agents route session/shared memory through the proxy; empty ⇒ direct Valkey.
		StatelayerProxyURL: statelayerProxyURL,
		// Launcher injection (C8, ADR 0079): opt-in + digest-pinned. Set ⇒ agents run the
		// platform-injected launcher (initContainer + emptyDir + Command override) so a
		// launcher fix rolls centrally; empty (default) ⇒ baked-launcher images run unchanged.
		// REQUIRES the Knative podspec-init-containers/-volumes-emptydir feature flags.
		LauncherImage: strings.TrimSpace(os.Getenv("LAUNCHER_IMAGE")),
		// Prompt-only deploy (M9): the resolve seam. v1 ships the deterministic,
		// OFFLINE fixture resolver — the dev/CI environment has no live git remote
		// (ADR 0004, mock-first). A production go-git resolver is a drop-in future
		// impl of prompt.Resolver swapped in HERE; nothing else changes.
		PromptResolver: prompt.NewFixtureResolver(),
		// Registry read source — nil ⇒ CRD default; the shared Postgres reader when
		// ToolRegistry is retired (RETIRE_TR). MUST be the SAME instance the
		// MCPToolBinding reconciler uses (below), or the two drift.
		Registry: registryReader,
		// Online-score store (m69.8, ADR 0062 Fork 4): the SAME cpDB store the regression
		// detector reads (below). The human rollback actuator's healthy-target damping guard
		// reads it to refuse a rollback to a version that itself regressed. nil-safe (dev
		// without cpDB ⇒ the store-backed half of the guard is skipped; the auto-trigger is
		// deferred, so this is guard-side only).
		OnlineScore: onlinescore.NewPostgresStore(cpDB),
		// Online-scoring config writer (m84.3, ADR 0062 Fork 2 / ADR 0011): the controller resolves each
		// agent's evalSuiteRef → EvalSuite.spec.online and UPSERTS/CLEARS the per-(ns, agent) config row the
		// BFF online-scoring worker reads (cpDB, no agent-CRD RBAC on the BFF SA). Same cpDB store; nil-safe.
		OnlineConfig: onlinescore.NewPostgresStore(cpDB),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "agentdeployment")
		os.Exit(1)
	}
	if err := (&controller.ModelRouteReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "modelroute")
		os.Exit(1)
	}
	if err := (&controller.MCPToolBindingReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		OBOEgress: oboEgress,
		// Same shared reader as the AgentDeployment reconciler (above) + the poll
		// channel that replaces the CRD watch — both nil unless RETIRE_TR.
		Registry:        registryReader,
		RegistryChanges: registryChanges,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "mcptoolbinding")
		os.Exit(1)
	}
	if err := (&controller.AgentRegistryReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "agentregistry")
		os.Exit(1)
	}
	if err := (&controller.AgentScalingPolicyReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "agentscalingpolicy")
		os.Exit(1)
	}
	if err := (&controller.AgentTeamReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "agentteam")
		os.Exit(1)
	}
	if err := (&controller.GuardrailPolicyReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "guardrailpolicy")
		os.Exit(1)
	}
	if err := (&controller.ApprovalPolicyReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "approvalpolicy")
		os.Exit(1)
	}
	if err := (&controller.FeedbackStoreReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "feedbackstore")
		os.Exit(1)
	}
	if err := (&controller.WorkflowReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "workflow")
		os.Exit(1)
	}
	if err := (&controller.TenantReconciler{
		Client: mgr.GetClient(),
		// Namespace→tenant membership mirror (m73.3, ADR 0067 §6): the reconcile mirrors the tenant's
		// owned member namespaces into a small (namespace, tenant) index so the m73.4 catalog resolves
		// membership without the BFF reading namespaces (ADR 0011). The manager's existing cpDB store
		// (mirrors the KnowledgeBase / regression-detector wiring); nil-safe if cpDB were absent.
		NamespaceTenant: namespacetenant.NewPostgresStore(cpDB),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "tenant")
		os.Exit(1)
	}
	// The durable run store over the manager's existing cpDB — the SAME Postgres run store the BFF/run-worker
	// build from cpDB, a READ-ONLY controller wiring (no config change, no new RBAC). Built ONCE here and shared
	// by the KnowledgeBase terminal-failed safety-net (M80, below) and the AlertPolicy approvalWaiting condition
	// (M75, further below). Nil-safe: if it fails to build, both read-features degrade off (the rest is
	// unaffected). NewPostgresStore runs its migrations session-locked, so a single instance is correct + cheap.
	var ctrlRunStore run.Store
	if rs, runErr := run.NewPostgresStore(context.Background(), cpDB); runErr != nil {
		setupLog.Error(runErr, "run store for controller read-features unavailable — "+
			"KnowledgeBase terminal-failed safety-net + HITL approval-waiting alerts disabled (the rest is unaffected)")
	} else {
		ctrlRunStore = rs
	}

	// KnowledgeBase reconciler (M68, ADR 0061): the finalizer's two-store GC + the status projection
	// from the corpus-status channel need the knowledge store (from the manager's existing cpDB) + the
	// durable object store (from OBJECT_STORE_ADDR). Both are typed-nil-safe: an unconfigured store ⇒ the
	// finalizer skips that half with a WARN and status stays validate-only (a dev deployment without them).
	// IngestionRuns (M80, ADR 0061 Fork 2) is the read-only run-store slice for the terminal-failed safety-net:
	// a KB stuck Ingesting whose ingestionRunRef run terminated out-of-band (no corpus-status row) is projected
	// Failed. nil (no cpDB run store) ⇒ the safety-net is disabled and the corpus-status channel is the sole source.
	var kbObjStore objectstore.ObjectStore
	if ms, dsErr := objectstore.NewMinioStore(); dsErr != nil {
		setupLog.Error(dsErr, "Failed to init the durable KB object store (OBJECT_STORE_ADDR)")
		os.Exit(1)
	} else if ms != nil {
		kbObjStore = ms
	}
	var kbIngestionRuns controller.KnowledgeBaseIngestionRunReader
	if ctrlRunStore != nil {
		kbIngestionRuns = ingestionRunStatusReader{store: ctrlRunStore}
	}
	// Scheduled re-ingest write seam (M140.4): the shared ingestion.Creator (the SAME builder the BFF /ingest
	// handler uses) resolves the source + creates a run. Wired only when both the object store + the run store
	// are present; nil ⇒ scheduled refresh is disabled (a dev deployment without them). Scheduled refresh needs
	// the run-worker pool to execute the created runs.
	var kbIngestionCreator controller.KnowledgeBaseIngestionRunCreator
	if kbObjStore != nil && ctrlRunStore != nil {
		kbIngestionCreator = &ingestion.Creator{DocStore: kbObjStore, RunStore: ctrlRunStore}
	}
	if err := (&controller.KnowledgeBaseReconciler{
		Client:              mgr.GetClient(),
		Knowledge:           knowledge.NewPostgresStore(cpDB),
		ObjectStore:         kbObjStore,
		IngestionRuns:       kbIngestionRuns,
		IngestionRunCreator: kbIngestionCreator,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "knowledgebase")
		os.Exit(1)
	}
	// Regression detector (M69, ADR 0062 Fork 4): maintains the RegressionDetected condition on
	// each AgentDeployment by comparing the serving AgentVersion's online-score aggregates against
	// the prior version's baseline (delta-vs-baseline + min-n + persistence). DETECTION ONLY — the
	// auto-rollback trigger is DEFERRED (PRD §17.4); the console pairs the condition with a
	// one-click HUMAN rollback (m69.8). The store is the manager's existing cpDB (mirrors the
	// KnowledgeBase wiring above); nil-safe if cpDB were absent (the detector abstains → Unknown).
	if err := (&controller.RegressionDetectorReconciler{
		Client:      mgr.GetClient(),
		OnlineScore: onlinescore.NewPostgresStore(cpDB),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "regressiondetector")
		os.Exit(1)
	}
	// AlertPolicy approvalWaiting condition (M75, ADR 0069 §3): reads runs paused on plan_approval to fire the
	// HITL notification, off the SHARED cpDB run store built above (a read-only wiring, no new RBAC). Nil-safe:
	// if the shared store failed to build, approval-waiting eval is simply disabled (Runs stays nil).
	var approvalRuns controller.ApprovalRunLister
	if lister, ok := ctrlRunStore.(interface {
		ListWaitingApproval(
			ctx context.Context, namespace string, kinds []run.ActionKind, limit int,
		) ([]run.WaitingApproval, error)
	}); ok && ctrlRunStore != nil {
		approvalRuns = approvalRunLister{store: lister}
	}

	// AlertPolicy runFailureRate condition (M84, ADR 0063 D2): counts failed/total runs per (namespace, agent)
	// over the condition's window, off the SAME SHARED cpDB run store (a read-only COUNT wiring, no new RBAC).
	// The durable pgStore satisfies CountRunOutcomes directly (Store's mem twin does not implement it — a dev
	// deployment without cpDB simply abstains). Nil-safe: if the shared store failed to build, runFailureRate
	// eval is disabled (RunOutcomes stays nil).
	var runOutcomes controller.RunOutcomeCounter
	if counter, ok := ctrlRunStore.(controller.RunOutcomeCounter); ok && ctrlRunStore != nil {
		runOutcomes = counter
	}

	// AlertPolicy errorRate + p95Latency conditions (M84, ADR 0076): read Knative queue-proxy per-revision
	// request metrics through the shared internal/promql instant client. The endpoint comes from
	// PROMETHEUS_URL (+ optional PROMETHEUS_TOKEN), mirroring the BFF's Prometheus adapter. UNSET or
	// unbuildable ⇒ the client stays nil and both SLO conditions ABSTAIN (a clear status reason, never a
	// false alert). This is HTTP to Prometheus — no new K8s RBAC (ADR 0076).
	var promMetrics controller.PromQLQuerier
	if promURL := os.Getenv("PROMETHEUS_URL"); promURL != "" {
		pc, perr := promql.New(promql.Config{
			BaseURL:     promURL,
			BearerToken: os.Getenv("PROMETHEUS_TOKEN"),
		})
		if perr != nil {
			setupLog.Error(perr, "PROMETHEUS_URL set but promql client build failed — errorRate/p95Latency will abstain")
		} else {
			promMetrics = pc
		}
	}

	// AlertPolicy reconciler (M70, ADR 0063 D2): evaluates each policy condition, fires once per
	// false→true transition (dedup in .status), and PERSISTS fired alerts to the durable alerts ledger
	// + audit_log (m70.4). Stores are the manager's existing cpDB (mirrors the regression detector +
	// KnowledgeBase wiring); all nil-safe if cpDB were absent. NOTIFICATION dispatch (webhook/console
	// feed) is m70.5. The reconciler reads Tenants for the budgetSoft condition. Runs + ConsoleURL are
	// the M75 approvalWaiting condition (ADR 0069 §3): the run store lists plan_approval-waiting runs
	// and ConsoleURL prefixes the notification's deep-link to the AUTHENTICATED console approval view.
	if err := (&controller.AlertPolicyReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Alerts:      alertstore.NewPostgresStore(cpDB),
		Rollups:     costrollup.NewPostgresStore(cpDB),
		Audit:       auditlog.NewPostgresStore(cpDB),
		Runs:        approvalRuns,
		RunOutcomes: runOutcomes,
		PromMetrics: promMetrics,
		ConsoleURL:  os.Getenv("CONSOLE_URL"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "alertpolicy")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	// Control-plane audit (M11.4, PRD §20): a controller-emitted audit trail of
	// mutating actions (create/update/delete) on every agent CRD. It watches the
	// CRDs via the manager cache and logs a structured entry per mutation —
	// crucially including DELETE, which the reconcilers do not observe without a
	// finalizer. The "who" is best-effort (managedFields field-manager); the
	// precise authenticated caller requires an admission webhook (phase-2).
	// Audit sink (M63, ADR 0056): tee the always-on greppable LogSink with an async PostgresSink that
	// persists mutations to the audit_log store — the queryable projection GET /api/audit reads. The
	// PostgresSink runs on every replica (like the Auditor, NeedLeaderElection=false); its inserts are
	// idempotent so cross-replica duplicate observations collapse to one row (never leader-elect it).
	auditPGSink := audit.NewPostgresSink(auditlog.NewPostgresStore(cpDB), ctrl.Log)
	if err := mgr.Add(auditPGSink); err != nil {
		setupLog.Error(err, "Failed to register the audit Postgres sink")
		os.Exit(1)
	}
	auditSink := audit.MultiSink{audit.NewLogSink(ctrl.Log), auditPGSink}
	if err := audit.NewAuditor(mgr.GetCache(), mgr.GetScheme(), auditSink).
		SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to set up control-plane audit")
		os.Exit(1)
	}
	// Audit retention pruner (M63, ADR 0056 §5): a LEADER-ELECTED Runnable that keeps the audit_log
	// bounded to AUDIT_RETENTION_DAYS (default 90). Unlike the sink it must NOT run on every replica —
	// the delete is bounded and one leader suffices. audit_log is a HOT store, not the compliance record.
	auditPruner := audit.NewRetentionPruner(
		auditlog.NewPostgresStore(cpDB), auditRetention(setupLog), ctrl.Log)
	if err := mgr.Add(auditPruner); err != nil {
		setupLog.Error(err, "Failed to register the audit retention pruner")
		os.Exit(1)
	}

	// Tenant-label ValidatingWebhook (C14, audit P1-3): OPT-IN (ENABLE_TENANT_LABEL_WEBHOOK=true) because it
	// needs webhook serving certs + a ValidatingWebhookConfiguration (config/webhook) — a user-gated deploy
	// step the base install does not yet wire (no cert-manager). When enabled, only the Tenant controller's
	// SA (TENANT_WEBHOOK_CONTROLLER_SA) may set/change the `agents.ctxmesh.ai/tenant` namespace label.
	//
	// signalCtx is created ONCE here (ctrl.SetupSignalHandler panics if called twice) so the webhook
	// cert-ready goroutine below can abort on shutdown; mgr.Start blocks on the same context.
	signalCtx := ctrl.SetupSignalHandler()
	if os.Getenv("ENABLE_TENANT_LABEL_WEBHOOK") == envValueTrue {
		// The ONLY principal the webhook allows to set/change the tenant label. Precedence (M134, ADR 0102):
		//   1. TENANT_WEBHOOK_CONTROLLER_SA (verbatim) — the envtest/integration + exotic-identity override.
		//   2. Self-derive via SelfSubjectReview: ask the API server for the manager's OWN username — the
		//      EXACT string it stamps into admission.Request.UserInfo.Username — so the allow-check is an
		//      invariant BY CONSTRUCTION (the allowed principal is definitionally the manager), not a
		//      namePrefix-fragile env literal that can silently drift from the real SA and self-wedge every
		//      install. SelfSubjectReview (authentication.k8s.io/v1, GA 1.28) is granted to all authenticated
		//      principals via system:basic-user — no RBAC add.
		// audit P2-2: an empty/unknown SA would deny EVERYONE — including the controller's own label stamping
		// — wedging every Tenant reconcile once the VWC is applied; so refuse to start rather than self-lock.
		controllerSA := strings.TrimSpace(os.Getenv("TENANT_WEBHOOK_CONTROLLER_SA"))
		if controllerSA == "" {
			derived, derr := selfControllerUsername(signalCtx, mgr.GetConfig())
			if derr != nil {
				setupLog.Error(derr, "could not self-derive the controller SA for the tenant-label webhook "+
					"(set TENANT_WEBHOOK_CONTROLLER_SA to override) — refusing to start (would deny the controller itself)")
				os.Exit(1)
			}
			controllerSA = derived
			setupLog.Info("tenant-label webhook: self-derived the controller principal via SelfSubjectReview",
				"user", controllerSA)
		}

		// Platform PKI (M128/Gate E, ADR 0102): the in-process cert-controller generates + rotates
		// the CA + the webhook serving cert (no cert-manager), writing them into webhookCertPath so
		// the webhook server can serve TLS. certReady closes once the cert is bootstrapped — m128.5
		// gates the VWC creation on it so a fail-closed webhook is never wired before its cert exists.
		certReady, err := pki.SetupWebhookCertRotator(mgr, pki.WebhookCertConfig{
			Namespace:          managerNamespace(),
			ServiceName:        webhookServiceName,
			CertDir:            webhookCertPath,
			SecretName:         webhookCertSecretName,
			CAName:             "ctxmesh-ca",
			CAOrganization:     "ctxmesh",
			CADuration:         5 * 365 * 24 * time.Hour, // ADR 0102: CA ~5y
			ServerCertDuration: 90 * 24 * time.Hour,      // ADR 0102: leaf ~90d, rotates ahead of expiry
			// The rotator keeps this VWC's caBundle current on rotation (M128/Gate E, ADR 0102 — no cert-manager).
			ValidatingWebhookName: enginewebhook.TenantLabelVWCName,
		})
		if err != nil {
			setupLog.Error(err, "unable to set up the webhook cert-controller (M128/ADR 0102)")
			os.Exit(1)
		}

		// M134 first-boot fix (Fable-reviewed, ADR 0102): DEFER registering the webhook handler until the
		// serving cert is on disk. SetupTenantLabelWebhook calls mgr.GetWebhookServer(), which LAZILY adds
		// + starts the webhook server; controller-runtime starts the Webhooks runnable group FIRST (before
		// the caches where the cert-rotator runs), and the server's certwatcher fails HARD on a missing
		// tls.crt → mgr.Start errors → the manager exits → CrashLoop forever (the fresh-boot wedge caught
		// live on the M134 throwaway cluster). Gating the FIRST GetWebhookServer() call on certReady defers
		// the server start until the projected Secret's cert exists. A plain goroutine (NOT mgr.Add) because
		// registration must run on EVERY replica (each backs the webhook Service); a RunnableFunc would land
		// in the leader-election group and never register on non-leaders.
		go func() {
			select {
			case <-signalCtx.Done():
				return
			case <-certReady:
			}
			enginewebhook.SetupTenantLabelWebhook(mgr, controllerSA)
			setupLog.Info("tenant-label webhook handler registered (cert ready — server starting)")
		}()

		// Keep this replica OUT of the webhook Service endpoints until it is actually serving TLS, so the
		// API server never hits connection-refused → a spurious fail-closed denial on a tenant-label write
		// during a rollout. Uses the LOCAL webhookServer var, never mgr.GetWebhookServer() (which would
		// trigger the lazy pre-Start add and resurrect the crash).
		if rerr := mgr.AddReadyzCheck("webhook-server", webhookServer.StartedChecker()); rerr != nil {
			setupLog.Error(rerr, "unable to add the webhook-server readyz check")
			os.Exit(1)
		}

		// The manager OWNS the tenant-label VWC (ADR 0102 §2): create it only AFTER the cert is ready,
		// so there is no uncertified-VWC window and the namespace/exemption are correct for ANY install
		// namespace (no chart templating). The cert-controller maintains the caBundle thereafter.
		vwcNS := managerNamespace()
		if addErr := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return nil
			case <-certReady:
			}
			if err := enginewebhook.ApplyTenantLabelVWC(ctx, mgr.GetClient(), vwcNS, webhookServiceName, nil); err != nil {
				setupLog.Error(err, "applying the manager-owned tenant-label VWC (M128/ADR 0102)")
				return err
			}
			setupLog.Info("tenant-label VWC applied (manager-owned; cert-controller maintains the caBundle)")
			return nil
		})); addErr != nil {
			setupLog.Error(addErr, "unable to register the tenant-label VWC applier")
			os.Exit(1)
		}
		setupLog.Info("tenant-label ValidatingWebhook + in-process cert-controller registered (M128/Gate E)")
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(signalCtx); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

// auditRetention resolves the audit_log retention window from AUDIT_RETENTION_DAYS (a positive integer
// number of days). Unset/blank/invalid/non-positive ⇒ the default (audit.DefaultRetention, 90d), logged
// so a fat-fingered value fails loud rather than silently disabling retention.
func auditRetention(log logr.Logger) time.Duration {
	raw := strings.TrimSpace(os.Getenv("AUDIT_RETENTION_DAYS"))
	if raw == "" {
		return audit.DefaultRetention
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days <= 0 {
		log.Info("AUDIT_RETENTION_DAYS is not a positive integer; using the default",
			"value", raw, "default", audit.DefaultRetention.String())
		return audit.DefaultRetention
	}
	return time.Duration(days) * 24 * time.Hour
}

// envDuration parses an optional duration env var (e.g. "45s", "2m"). Blank or unparseable ⇒ 0, which
// every consumer reads as "use my default" — a typo must not silently disable a safety window.
func envDuration(name string) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		setupLog.Info("WARNING: ignoring unparseable duration env var; using the default", "var", name, "value", v)
		return 0
	}
	return d
}
