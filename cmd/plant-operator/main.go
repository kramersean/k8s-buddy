// Command plant-operator is K8s Buddy's Kubernetes operator: it watches
// Plant custom resources and drives the Deployment, Service, ConfigMap,
// PodDisruptionBudget, ServiceAccount, and NetworkPolicy each one owns
// toward the state internal/controller's builders describe (see
// internal/controller/
// resources.go and plant_controller.go for that logic itself). This file
// owns only what internal/controller does not -- wiring a controller-runtime
// manager, resolving flags, and the process lifecycle. It also owns Plant's
// two admission webhooks (api/v1alpha1/plant_webhook.go) and their
// self-signed certificate bootstrap (webhookcerts.go, this package -- see
// docs/adr/0009-webhook-certificate-strategy.md for why).
//
// Unlike cmd/buddy-api, which is a plain HTTP service and uses log/slog,
// this binary is controller-runtime-native: it uses controller-runtime's own
// manager, leader election, and zap-backed logr logger throughout, because
// that is the idiomatic shape for an operator built on that framework and
// every piece of controller-runtime machinery this binary wires up already
// expects a logr.Logger, not a *slog.Logger.
package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	healthz "sigs.k8s.io/controller-runtime/pkg/healthz"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsfilters "sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	buddyv1alpha1 "github.com/sean-kramer/k8s-buddy/api/v1alpha1"
	"github.com/sean-kramer/k8s-buddy/internal/controller"
)

// version and commit are overridden at build time via
// `-ldflags "-X main.version=... -X main.commit=..."`; see the Makefile's
// build target and build/Dockerfile.plant-operator. Left as their defaults
// for `go run` and any local build that skips ldflags -- mirroring
// cmd/buddy-api's own version/commit vars exactly.
var (
	version = "dev"
	commit  = "unknown"
)

// scheme is the runtime.Scheme every client and cache this manager owns
// resolves types through: the standard client-go types (Deployment, Service,
// ConfigMap, PodDisruptionBudget, ServiceAccount, Event, ...) -- all
// registered by clientgoscheme.AddToScheme -- plus Plant itself.
var scheme = k8sruntime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(buddyv1alpha1.AddToScheme(scheme))
}

// webhookConfigurationNames mirrors, by convention (see plant_webhook.go's
// own comment on the same three-way agreement), the two names
// api/v1alpha1/plant_webhook.go's +kubebuilder:webhookconfiguration markers
// bake into config/webhook/manifests.yaml, and that internal/controller/
// plant_controller.go's own +kubebuilder:rbac resourceNames pins the
// caBundle-patch grant to. charts/k8s-buddy overrides both flags (its own
// webhook configuration objects are release-scoped, not these fixed names)
// via its Deployment template's args -- see that chart's own
// templates/deployment.yaml and values.yaml webhook block.
const (
	defaultMutatingWebhookConfigurationName   = "plant-operator-mutating-webhook-configuration"
	defaultValidatingWebhookConfigurationName = "plant-operator-validating-webhook-configuration"
	defaultWebhookServiceName                 = "plant-operator-webhook"
	// defaultPodNamespace is used only when POD_NAMESPACE is unset -- an
	// out-of-cluster `go run ./cmd/plant-operator`, or a manifest that
	// forgot the downward-API env var this project's own Deployment always
	// sets. It matches deploy/kustomize/operator's own namespace so a local
	// run against the kind cluster still resolves sane webhook DNS names.
	defaultPodNamespace = "k8s-buddy-system"
)

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var webhookPort int
	var webhookCertDir string
	var webhookServiceName string
	var mutatingWebhookConfigurationName string
	var validatingWebhookConfigurationName string
	var allowedImageRegistriesRaw string

	// Flag names and defaults follow controller-runtime's own convention
	// (the same names kubebuilder scaffolds every manager with), with
	// addresses adjusted to this project's pinned ports -- metrics 8081,
	// health/readiness 8082 -- per the operator plan's Global Constraints.
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8081", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8082", "The address the health/readiness probe endpoint binds to.")
	// Defaults to false, matching kubebuilder's own scaffold: leader
	// election needs a Lease in a real namespace to contend over, which an
	// out-of-cluster `go run ./cmd/plant-operator` has no way to resolve
	// (it fails outright with "unable to find leader election namespace").
	// The deployed manifest (deploy/kustomize/operator/deployment.yaml)
	// passes --leader-elect=true explicitly, so leader election is still
	// genuinely enabled in-cluster -- this default only affects a bare
	// local run.
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for the operator manager. Enabling this ensures there is only one active "+
			"plant-operator at a time when running more than one replica.")

	// Port 9443 is controller-runtime's own webhook.DefaultPort, and the one
	// every kustomize/Helm Service in this project targets.
	flag.IntVar(&webhookPort, "webhook-port", 9443, "The port the admission webhook server binds to.")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir",
		envOrDefault("WEBHOOK_CERT_DIR", filepath.Join(os.TempDir(), "k8s-webhook-server", "serving-certs")),
		"Directory (backed by an emptyDir volume in every shipped manifest -- see docs/adr/0009) this process "+
			"writes its self-signed serving certificate (tls.crt/tls.key) into at every startup, and the webhook "+
			"server reads them back from.")
	flag.StringVar(&webhookServiceName, "webhook-service-name", envOrDefault("WEBHOOK_SERVICE_NAME", defaultWebhookServiceName),
		"Name of the Service fronting this process's webhook port -- used to compute the DNS SANs on the "+
			"self-signed serving certificate this process generates at startup.")
	flag.StringVar(&mutatingWebhookConfigurationName, "mutating-webhook-configuration-name",
		envOrDefault("MUTATING_WEBHOOK_CONFIGURATION_NAME", defaultMutatingWebhookConfigurationName),
		"Name of the MutatingWebhookConfiguration this process patches its self-signed CA bundle into at startup.")
	flag.StringVar(&validatingWebhookConfigurationName, "validating-webhook-configuration-name",
		envOrDefault("VALIDATING_WEBHOOK_CONFIGURATION_NAME", defaultValidatingWebhookConfigurationName),
		"Name of the ValidatingWebhookConfiguration this process patches its self-signed CA bundle into at startup.")
	// The default is non-empty (ghcr.io/ and docker.io/library/ -- see
	// buddyv1alpha1.DefaultAllowedImageRegistries) so reaching allow-all
	// requires an OPERATOR explicitly passing an empty value, never leaving
	// this flag/env var unset. See PlantCustomValidator's own doc comment.
	flag.StringVar(&allowedImageRegistriesRaw, "allowed-image-registries",
		envOrDefault("ALLOWED_IMAGE_REGISTRIES", strings.Join(buddyv1alpha1.DefaultAllowedImageRegistries(), ",")),
		"Comma-separated list of image registry prefixes spec.image is allowed to reference (e.g. "+
			"\"ghcr.io/,docker.io/library/\"). An empty value allows every registry -- an explicit opt-out, "+
			"never the default.")

	opts := ctrlzap.Options{Development: false} // Development: false -> JSON output, matching buddy-api's structured logging.
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	logf.SetLogger(ctrlzap.New(ctrlzap.UseFlagOptions(&opts)))
	setupLog := logf.Log.WithName("setup")

	allowedImageRegistries := parseAllowedImageRegistries(allowedImageRegistriesRaw)

	// POD_NAMESPACE is set via the downward API (fieldRef: metadata.namespace)
	// on every Deployment this project ships (deploy/kustomize/operator and
	// charts/k8s-buddy) specifically so this resolves correctly regardless of
	// which namespace a given install lands in -- see webhookServiceDNSNames
	// and docs/adr/0009.
	podNamespace := envOrDefault("POD_NAMESPACE", defaultPodNamespace)

	// One line, everything a human reading `kubectl logs` needs to know
	// this process's identity and exact resolved configuration -- no silent
	// defaults to go hunting for. Mirrors cmd/buddy-api's own startup log.
	setupLog.Info("starting plant-operator",
		"version", version,
		"commit", commit,
		"metricsBindAddress", metricsAddr,
		"healthProbeBindAddress", probeAddr,
		"leaderElection", enableLeaderElection,
		"webhookPort", webhookPort,
		"webhookCertDir", webhookCertDir,
		"webhookServiceName", webhookServiceName,
		"podNamespace", podNamespace,
		"mutatingWebhookConfigurationName", mutatingWebhookConfigurationName,
		"validatingWebhookConfigurationName", validatingWebhookConfigurationName,
		"allowedImageRegistries", allowedImageRegistries,
	)

	// Certificate bootstrap (docs/adr/0009): a fresh self-signed CA and
	// serving certificate, generated and written to webhookCertDir (an
	// emptyDir-backed path in every shipped manifest) BEFORE the manager --
	// and therefore its webhook server -- ever starts listening, and the CA
	// bundle patched into both webhook configurations before this process
	// does anything else that could result in a Plant write being evaluated
	// against a caBundle that still points at a previous process's
	// now-discarded CA.
	cfg := ctrl.GetConfigOrDie()

	dnsNames := webhookServiceDNSNames(webhookServiceName, podNamespace)
	caPEM, leafNotAfter, err := generateWebhookServingCertificate(webhookCertDir, dnsNames)
	if err != nil {
		setupLog.Error(err, "unable to generate webhook serving certificate")
		os.Exit(1)
	}
	setupLog.Info("generated self-signed webhook serving certificate",
		"dnsNames", dnsNames, "certDir", webhookCertDir, "notAfter", leafNotAfter.Format(time.RFC3339))

	patchCtx, patchCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err = patchWebhookCABundles(patchCtx, cfg, scheme, mutatingWebhookConfigurationName, validatingWebhookConfigurationName, caPEM)
	patchCancel()
	if err != nil {
		setupLog.Error(err, "unable to patch webhook CA bundles",
			"mutatingWebhookConfigurationName", mutatingWebhookConfigurationName,
			"validatingWebhookConfigurationName", validatingWebhookConfigurationName)
		os.Exit(1)
	}
	setupLog.Info("merged this process's CA into both webhook configurations' caBundle",
		"mutatingWebhookConfigurationName", mutatingWebhookConfigurationName,
		"validatingWebhookConfigurationName", validatingWebhookConfigurationName)

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Cache:  controller.CacheOptions(),
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    webhookPort,
			CertDir: webhookCertDir,
		}),
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
			// This closes the last item carried out of Plan 2 (see
			// docs/adr/0008-deferred-to-plan-3.md): the operator's
			// controller-runtime metrics used to be served on :8081 with no
			// authentication at all -- anything that could reach the pod's
			// network could read them. WithAuthenticationAndAuthorization
			// wraps the handler so every request must present a bearer
			// token the API server recognizes (TokenReview) AND be
			// authorized for `GET` on the non-resource URL `/metrics`
			// (SubjectAccessReview) -- see config/rbac/role.yaml (the
			// tokenreviews/subjectaccessreviews rules, generated from
			// plant_controller.go's own +kubebuilder:rbac markers) for the
			// operator's half of that check, and
			// config/rbac/metrics_reader_role.yaml plus
			// deploy/observability/prometheus-rbac.yaml for the scraper's
			// half. SecureServing:true is paired with it deliberately: a
			// bearer token sent over plaintext HTTP defeats much of the
			// point of requiring one. With no CertDir configured and no
			// cert-manager in this project's scope, the metrics server
			// falls back to an in-memory self-signed certificate generated
			// fresh at startup (see controller-runtime's
			// metricsserver.createListener) -- good enough to encrypt the
			// transport for a cluster-internal scrape; Prometheus's
			// ServiceMonitor is configured with insecureSkipVerify for
			// exactly this reason (see deploy/kustomize/operator/
			// servicemonitor.yaml).
			SecureServing:  true,
			FilterProvider: metricsfilters.WithAuthenticationAndAuthorization,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		// A fixed, project-scoped ID: two different operators (or two
		// different Plans of this same one) racing for leadership on the
		// same cluster must never be able to collide on a generic name like
		// "controller-leader-election".
		LeaderElectionID: "plant-operator.buddy.k8s-buddy.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	reconciler := &controller.PlantReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		// The manager's UNCACHED reader. PlantReconciler uses it for
		// exactly one thing -- checking whether a same-named object it is
		// about to write already exists and belongs to someone else -- and
		// that check is only meaningful against a reader the label filter
		// in controller.CacheOptions() does not apply to. See
		// PlantReconciler.APIReader's own comment.
		APIReader: mgr.GetAPIReader(),
		Recorder:  mgr.GetEventRecorderFor("plant-controller"), //nolint:staticcheck // record.EventRecorder is PlantReconciler.Recorder's fixed field type
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Plant")
		os.Exit(1)
	}

	if err := buddyv1alpha1.SetupPlantWebhookWithManager(mgr, allowedImageRegistries); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "Plant")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
	// The certificate-expiry backstop (see webhookCertificateValidity and
	// webhookCertExpiryCheck's own comments, webhookcerts.go): registered on
	// BOTH checks so a liveness-probe failure actually restarts this
	// container (minting a fresh certificate) and a readiness-probe failure
	// pulls it out of the webhook Service's endpoints as early as possible.
	if err := mgr.AddHealthzCheck("webhook-cert-expiry", webhookCertExpiryCheck(leafNotAfter)); err != nil {
		setupLog.Error(err, "unable to set up webhook certificate expiry health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("webhook-cert-expiry", webhookCertExpiryCheck(leafNotAfter)); err != nil {
		setupLog.Error(err, "unable to set up webhook certificate expiry ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// envOrDefault returns the named environment variable's value if it is set
// at all (including set-to-empty, via os.LookupEnv rather than a bare
// os.Getenv) -- the distinction that lets --allowed-image-registries=""
// (or ALLOWED_IMAGE_REGISTRIES="") reach PlantCustomValidator's genuine
// allow-all opt-out, rather than an unset env var being silently
// indistinguishable from one explicitly set empty.
func envOrDefault(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// parseAllowedImageRegistries turns the --allowed-image-registries flag's
// raw comma-separated value into the slice PlantCustomValidator expects. An
// entirely empty (after trimming) raw value returns nil -- the explicit
// allow-all opt-out -- rather than a one-element slice containing "".
func parseAllowedImageRegistries(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}
