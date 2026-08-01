// Command chaos-buddy is K8s Buddy's controlled failure injector: on a
// fixed interval it either deletes one pod matching a label selector
// (pod-kill) or flips one pod unready for a bounded window
// (readiness-flap), in a single namespace it is bound to by RBAC as well
// as by its own configuration.
//
// This file owns configuration loading and validation (env-var based,
// validation-not-silent-fallback, mirroring cmd/buddy-api/main.go's
// loadConfig exactly), building the real Kubernetes client, wiring the
// three buddy_chaos_* Prometheus metrics against an injected registry
// (mirroring internal/telemetry.NewMetrics's own pattern, without
// modifying that package -- chaos-buddy's metric set is small enough, and
// specific enough to this binary, that it does not belong in a package
// buddy-api and plant-operator also depend on), serving /metrics and
// /healthz, and the poll loop that drives internal/chaos's pure decision
// logic (engine.go) and client calls (kube.go).
//
// The decision logic itself -- pod selection, kill-switch enforcement,
// namespace refusal, dry-run -- lives entirely in internal/chaos, where it
// is unit-tested without a cluster. Nothing in this file makes a safety
// decision on its own; it only reads configuration, calls
// chaos.Decide/chaos.Execute, and reports the result.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/sean-kramer/k8s-buddy/internal/chaos"
)

// version and commit are overridden at build time via
// `-ldflags "-X main.version=... -X main.commit=..."`; see the Makefile's
// docker-build-chaos target and build/Dockerfile.chaos-buddy. Left as
// their defaults for `go run` and any local build that skips ldflags,
// mirroring cmd/buddy-api's own version/commit vars exactly.
var (
	version = "dev"
	commit  = "unknown"
)

// readHeaderTimeout mirrors cmd/buddy-api's own readHeaderTimeout: gosec's
// G112 rule requires an explicit value here, since an http.Server with no
// ReadHeaderTimeout can be held open indefinitely by a slow-headers
// client.
const readHeaderTimeout = 10 * time.Second

// httpShutdownTimeout bounds how long the metrics/healthz server's
// Shutdown waits for its (normally trivial) in-flight requests to drain.
// chaos-buddy has no user-facing traffic the way buddy-api does -- only
// Prometheus scrapes and kubelet probes -- so this is a short safety
// ceiling, not a tuned value.
const httpShutdownTimeout = 5 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		// The structured logger normally used everywhere else may not
		// exist yet if configuration loading itself is what failed, so
		// this bootstrap logger -- independent of any config -- is what
		// reports a startup failure. Mirrors cmd/buddy-api/main.go.
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("chaos-buddy exited with error", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dryRun, err := parseDryRunFlag(args)
	if err != nil {
		return err
	}

	cfg, err := loadConfig(dryRun)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logLevel, err := parseLogLevel(cfg.logLevel)
	if err != nil {
		return fmt.Errorf("parse log level: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("resolve in-cluster Kubernetes config (chaos-buddy is designed to run only inside the cluster it targets): %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("build Kubernetes clientset: %w", err)
	}
	client := chaos.NewClient(clientset, nil)

	reg := prometheus.NewRegistry()
	metrics := newChaosMetrics(reg)

	// One line, everything a human reading `kubectl logs` needs to know
	// this process's identity and exact resolved configuration -- no
	// silent defaults to go hunting for. Mirrors cmd/buddy-api's own
	// startup log.
	logger.Info("starting chaos-buddy",
		"version", version,
		"commit", commit,
		"targetNamespace", cfg.targetNamespace,
		"labelSelector", cfg.labelSelector,
		"mode", string(cfg.mode),
		"interval", cfg.interval.String(),
		"dryRun", cfg.dryRun,
		"configMapName", cfg.configMapName,
		"logLevel", cfg.logLevel,
	)
	if cfg.dryRun {
		logger.Info("dry-run is enabled: chaos-buddy will log every intended action and delete/flap nothing")
	} else {
		logger.Warn("dry-run is DISABLED: chaos-buddy will perform real, destructive actions against matching pods")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	httpServer := &http.Server{
		Addr:              ":8080",
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	serveErrs := make(chan error, 1)
	go func() {
		logger.Info("http server listening", "addr", httpServer.Addr)
		err := httpServer.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrs <- fmt.Errorf("listen and serve: %w", err)
			return
		}
		serveErrs <- nil
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	//nolint:gosec // G404: seeds pod selection among equally-valid chaos
	// targets, not a security-sensitive choice.
	source := rand.New(rand.NewSource(time.Now().UnixNano()))

	runLoop(sigCtx, client, cfg, metrics, logger, source)

	logger.Info("shutdown: signal received, stopping http server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down http server: %w", err)
	}

	select {
	case err := <-serveErrs:
		if err != nil {
			return err
		}
	case <-time.After(httpShutdownTimeout):
		logger.Warn("timed out waiting for the http server goroutine to exit after shutdown")
	}

	logger.Info("shutdown: complete")
	return nil
}

// runLoop drives one chaos.Decide/chaos.Execute cycle immediately, then
// again every cfg.interval, until ctx is canceled (SIGINT/SIGTERM). It
// never returns an error itself: every failure inside a single iteration
// (a list error, a delete error, an unreadable kill switch) is logged and
// folded into that iteration's outcome, never allowed to crash the
// process -- a single bad iteration must not take down the loop that is
// supposed to keep polling the kill switch.
func runLoop(ctx context.Context, client chaos.PodClient, cfg config, metrics *chaosMetrics, logger *slog.Logger, source *rand.Rand) {
	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()

	runIteration(ctx, client, cfg, metrics, logger, source)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runIteration(ctx, client, cfg, metrics, logger, source)
		}
	}
}

// runIteration performs exactly one iteration: read the kill switch,
// reflect it in buddy_chaos_enabled, list candidate pods, decide, and
// (unless the decision is ActionNone) execute -- recording the result as
// both a log line and buddy_chaos_actions_total.
func runIteration(ctx context.Context, client chaos.PodClient, cfg config, metrics *chaosMetrics, logger *slog.Logger, source *rand.Rand) {
	enabled, switchErr := client.ReadSwitch(ctx, cfg.targetNamespace, cfg.configMapName)
	permits := chaos.SwitchPermits(enabled, switchErr)
	metrics.setEnabled(permits)

	if switchErr != nil {
		logger.Error("kill switch ConfigMap could not be read; failing closed for this iteration",
			"error", switchErr, "configMap", cfg.configMapName, "namespace", cfg.targetNamespace)
	}

	pods, err := client.ListPods(ctx, cfg.targetNamespace, cfg.labelSelector)
	if err != nil {
		logger.Error("list candidate pods failed", "error", err, "namespace", cfg.targetNamespace, "labelSelector", cfg.labelSelector)
		return
	}

	decision := chaos.Decide(cfg.mode, enabled, switchErr, cfg.targetNamespace, pods, source)
	if decision.Kind == chaos.ActionNone {
		logger.Info("no chaos action this iteration", "reason", decision.Reason, "candidateCount", len(pods))
		return
	}

	outcome, err := chaos.Execute(ctx, client, decision, cfg.dryRun)
	metrics.recordAction(string(cfg.mode), outcome)

	fields := []any{
		"mode", string(cfg.mode),
		"targetNamespace", decision.Target.Namespace,
		"targetPod", decision.Target.Name,
		"outcome", outcome,
		"dryRun", cfg.dryRun,
	}
	if err != nil {
		logger.Error("chaos action failed", append(fields, "error", err)...)
		return
	}
	logger.Info("chaos action completed", fields...)
}

// chaosMetrics is the small, chaos-buddy-specific Prometheus collector
// set: the three buddy_chaos_* metrics from the platform plan's Global
// Constraints, registered against an injected registry the same way
// internal/telemetry.NewMetrics registers buddy-api's -- never against
// prometheus.DefaultRegisterer, so tests (and any future second instance
// in the same process) never share state.
type chaosMetrics struct {
	actionsTotal        *prometheus.CounterVec
	lastActionTimestamp *prometheus.GaugeVec
	enabled             prometheus.Gauge
}

// newChaosMetrics registers every buddy_chaos_* series against reg and
// pre-initializes every (mode, outcome) combination to 0, mirroring
// internal/telemetry.NewMetrics's own reasoning for buddy_work_requests_total:
// a *Vec exports nothing at all for a label combination it has never been
// given, so without this a freshly started pod would export no
// buddy_chaos_actions_total series until its first action -- and a
// dashboard or alert querying it during that window would see "no data"
// instead of a truthful 0.
func newChaosMetrics(reg prometheus.Registerer) *chaosMetrics {
	factory := promauto.With(reg)

	m := &chaosMetrics{
		actionsTotal: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "buddy_chaos_actions_total",
			Help: "Total count of chaos actions attempted, by mode and outcome.",
		}, []string{"mode", "outcome"}),
		lastActionTimestamp: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "buddy_chaos_last_action_timestamp_seconds",
			Help: "Unix time of the last chaos action attempted, by mode.",
		}, []string{"mode"}),
		enabled: factory.NewGauge(prometheus.GaugeOpts{
			Name: "buddy_chaos_enabled",
			Help: "1 when the kill switch currently permits chaos actions, 0 otherwise.",
		}),
	}

	outcomes := []string{chaos.OutcomeSuccess, chaos.OutcomeFailure, chaos.OutcomeDryRun}
	for _, mode := range chaos.SupportedModes() {
		for _, outcome := range outcomes {
			m.actionsTotal.WithLabelValues(string(mode), outcome)
		}
		m.lastActionTimestamp.WithLabelValues(string(mode))
	}

	return m
}

// recordAction increments buddy_chaos_actions_total{mode,outcome} and sets
// buddy_chaos_last_action_timestamp_seconds{mode} to now. outcome=="" (an
// ActionNone iteration) is a caller error runIteration never actually
// makes -- it returns before calling this -- but is guarded here anyway
// rather than trusted, so a future caller can't silently create a
// meaningless empty-outcome series.
func (m *chaosMetrics) recordAction(mode, outcome string) {
	if outcome == "" {
		return
	}
	m.actionsTotal.WithLabelValues(mode, outcome).Inc()
	m.lastActionTimestamp.WithLabelValues(mode).Set(float64(time.Now().Unix()))
}

// setEnabled sets buddy_chaos_enabled to 1 or 0.
func (m *chaosMetrics) setEnabled(permits bool) {
	v := 0.0
	if permits {
		v = 1.0
	}
	m.enabled.Set(v)
}

// config is chaos-buddy's resolved runtime configuration.
type config struct {
	targetNamespace string
	labelSelector   string
	mode            chaos.Mode
	interval        time.Duration
	dryRun          bool
	configMapName   string
	logLevel        string
}

// parseDryRunFlag registers and parses the --dry-run flag described in the
// platform plan's safety requirements. Its DEFAULT is seeded from
// CHAOS_DRY_RUN (itself defaulting to true if unset), so the shipped
// Deployment -- which sets CHAOS_DRY_RUN=true in its ConfigMap and passes
// no --dry-run argument at all -- starts inert, and flipping dry-run off
// for the live demo is a ConfigMap/env edit (see
// deploy/kustomize/chaos/configmap.yaml) followed by a rollout, not a
// manifest rewrite. An explicit --dry-run=... argument (useful for a local
// `go run`) still overrides that default, since flag.Parse always wins
// over a flag's own default value.
func parseDryRunFlag(args []string) (bool, error) {
	defaultDryRun, err := parseBoolEnv("CHAOS_DRY_RUN", true)
	if err != nil {
		return false, err
	}

	fs := flag.NewFlagSet("chaos-buddy", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", defaultDryRun,
		"If true (default), log every intended chaos action without deleting or flapping anything.")
	if err := fs.Parse(args); err != nil {
		return false, fmt.Errorf("parse flags: %w", err)
	}
	return *dryRun, nil
}

// loadConfig reads chaos-buddy's configuration from the environment,
// mirroring cmd/buddy-api/main.go's loadConfig exactly:
// validation-not-silent-fallback for every variable that is set but
// invalid, and CHAOS_TARGET_NAMESPACE/CHAOS_LABEL_SELECTOR having no
// default at all -- an unset required variable, or an empty label
// selector, is always a startup error naming exactly what is wrong,
// never an implicit "match everything" or "do nothing safely."
//
// CHAOS_LABEL_SELECTOR gets two checks, not one: chaos.ValidateLabelSelector
// (internal/chaos/engine.go) rejects an empty/whitespace-only selector, and
// labels.Parse below additionally rejects one that is non-empty but
// syntactically invalid (e.g. "===not a selector==="). The second check
// lives here, in main.go, rather than in the pure engine package, because
// it requires k8s.io/apimachinery/pkg/labels -- engine.go stays free of
// every Kubernetes import so it can be unit-tested without a cluster; this
// file already imports client-go for the real client, so it is the right
// place for a k8s-API-shaped validation. Without it, a typo'd selector
// would still start the process cleanly and then fail identically on every
// single loop iteration forever (ListPods returning a "not a valid
// selector" error) -- fail-safe (Decide would never see a candidate list at
// all, so it could never act on the wrong pods) but not fail-fast, so a
// misconfigured chaos-buddy would sit in a crash-free but useless loop
// instead of refusing to start with an actionable error.
func loadConfig(dryRun bool) (config, error) {
	targetNamespace, ok := os.LookupEnv("CHAOS_TARGET_NAMESPACE")
	if !ok || strings.TrimSpace(targetNamespace) == "" {
		return config{}, errors.New("CHAOS_TARGET_NAMESPACE is required and must not be empty")
	}

	labelSelector := getEnv("CHAOS_LABEL_SELECTOR", "")
	if err := chaos.ValidateLabelSelector(labelSelector); err != nil {
		return config{}, err
	}
	if _, err := labels.Parse(labelSelector); err != nil {
		return config{}, fmt.Errorf("invalid CHAOS_LABEL_SELECTOR: %q is not a valid label selector: %w", labelSelector, err)
	}

	modeStr := getEnv("CHAOS_MODE", string(chaos.ModePodKill))
	mode, err := chaos.ParseMode(modeStr)
	if err != nil {
		return config{}, err
	}

	interval, err := parseDurationEnv("CHAOS_INTERVAL", 30*time.Second)
	if err != nil {
		return config{}, err
	}
	if interval <= 0 {
		return config{}, fmt.Errorf("invalid CHAOS_INTERVAL: %s must be a positive duration", interval)
	}

	return config{
		targetNamespace: targetNamespace,
		labelSelector:   labelSelector,
		mode:            mode,
		interval:        interval,
		dryRun:          dryRun,
		configMapName:   getEnv("CHAOS_CONFIGMAP_NAME", "chaos-buddy-switch"),
		logLevel:        getEnv("CHAOS_LOG_LEVEL", "info"),
	}, nil
}

// getEnv returns the environment variable key's value, or def if it is
// unset or empty. Mirrors cmd/buddy-api/main.go's getEnv exactly.
func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// parseBoolEnv parses key with strconv.ParseBool, returning def if it is
// unset or empty, and an error naming key if it is set but not a valid
// boolean. Mirrors cmd/buddy-api/main.go's parseBoolEnv exactly.
func parseBoolEnv(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid %s: %q is not a valid boolean: %w", key, v, err)
	}
	return b, nil
}

// parseDurationEnv parses key with time.ParseDuration, returning def if it
// is unset or empty, and an error naming key if it is set but not a valid
// duration. Mirrors cmd/buddy-api/main.go's parseDurationEnv exactly.
func parseDurationEnv(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %q is not a valid duration: %w", key, v, err)
	}
	return d, nil
}

// parseLogLevel maps CHAOS_LOG_LEVEL's string value to a slog.Level.
// Mirrors cmd/buddy-api/main.go's parseLogLevel exactly.
func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid CHAOS_LOG_LEVEL: %q (want debug, info, warn, or error)", s)
	}
}
