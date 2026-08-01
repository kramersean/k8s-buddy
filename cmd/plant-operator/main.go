// Command plant-operator is K8s Buddy's Kubernetes operator: it watches
// Plant custom resources and drives the Deployment, Service, ConfigMap,
// PodDisruptionBudget, ServiceAccount, and NetworkPolicy each one owns
// toward the state internal/controller's builders describe (see
// internal/controller/
// resources.go and plant_controller.go for that logic itself). This file
// owns only what internal/controller does not -- wiring a controller-runtime
// manager, resolving flags, and the process lifecycle.
//
// Unlike cmd/buddy-api, which is a plain HTTP service and uses log/slog,
// this binary is controller-runtime-native: it uses controller-runtime's own
// manager, leader election, and zap-backed logr logger throughout, because
// that is the idiomatic shape for an operator built on that framework and
// every piece of controller-runtime machinery this binary wires up already
// expects a logr.Logger, not a *slog.Logger.
package main

import (
	"flag"
	"os"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	healthz "sigs.k8s.io/controller-runtime/pkg/healthz"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

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

// cacheOptions restricts what the manager's informer cache actually holds
// for each of the six child types PlantReconciler owns, to just those objects
// carrying app.kubernetes.io/managed-by=plant-operator.
//
// Without it, SetupWithManager's Owns(&corev1.ConfigMap{}) and
// Owns(&corev1.ServiceAccount{}) mean this process establishes a cluster-wide
// LIST+WATCH on every ConfigMap and every ServiceAccount in the cluster and
// holds them all in memory — on a real cluster that is thousands of objects,
// including every ConfigMap belonging to every other team, none of which this
// operator will ever reconcile. The ClusterRole granting the read is
// genuinely required (a Plant may be created in any namespace, so the
// operator cannot be namespace-scoped); the memory footprint and the watch
// traffic are not.
//
// Two properties make this safe rather than merely smaller:
//
//   - Every child this operator creates carries the label, set by
//     internal/controller's LabelsFor on every single build, and
//     mergeLabels re-asserts it on every reconcile. A child cannot end up
//     outside the filter through normal operation.
//   - Plant itself is deliberately absent from ByObject. The filter applies
//     only to the types listed; the Plant type keeps its full, unfiltered
//     cache, so a Plant carrying no labels at all is still watched and still
//     reconciled. Filtering the primary resource by a label the USER would
//     have to remember to set is how an operator ends up silently ignoring
//     the objects it exists for.
//
// The one behavioral consequence is the one PlantReconciler.APIReader exists
// to handle: a pre-existing, foreign object of the same name is invisible
// through this cache. The adoption check reads through the uncached API
// reader precisely so it can still see it.
func cacheOptions() cache.Options {
	managed := labels.SelectorFromSet(labels.Set{
		controller.LabelManagedBy: controller.ManagedByValue,
	})
	byLabel := cache.ByObject{Label: managed}

	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&appsv1.Deployment{}:            byLabel,
			&corev1.Service{}:               byLabel,
			&corev1.ConfigMap{}:             byLabel,
			&corev1.ServiceAccount{}:        byLabel,
			&policyv1.PodDisruptionBudget{}: byLabel,
			&networkingv1.NetworkPolicy{}:   byLabel,
		},
	}
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool

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

	opts := ctrlzap.Options{Development: false} // Development: false -> JSON output, matching buddy-api's structured logging.
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	logf.SetLogger(ctrlzap.New(ctrlzap.UseFlagOptions(&opts)))
	setupLog := logf.Log.WithName("setup")

	// One line, everything a human reading `kubectl logs` needs to know
	// this process's identity and exact resolved configuration -- no silent
	// defaults to go hunting for. Mirrors cmd/buddy-api's own startup log.
	setupLog.Info("starting plant-operator",
		"version", version,
		"commit", commit,
		"metricsBindAddress", metricsAddr,
		"healthProbeBindAddress", probeAddr,
		"leaderElection", enableLeaderElection,
	)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cacheOptions(),
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
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
		// in cacheOptions() below does not apply to. See
		// PlantReconciler.APIReader's own comment.
		APIReader: mgr.GetAPIReader(),
		Recorder:  mgr.GetEventRecorderFor("plant-controller"), //nolint:staticcheck // record.EventRecorder is PlantReconciler.Recorder's fixed field type
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Plant")
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

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
