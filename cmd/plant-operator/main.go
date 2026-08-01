// Command plant-operator is K8s Buddy's Kubernetes operator: it watches
// Plant custom resources and drives the Deployment, Service, ConfigMap,
// PodDisruptionBudget, and ServiceAccount each one owns toward the state
// internal/controller's builders describe (see internal/controller/
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

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
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
	flag.BoolVar(&enableLeaderElection, "leader-elect", true,
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
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("plant-controller"), //nolint:staticcheck // record.EventRecorder is PlantReconciler.Recorder's fixed field type
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
