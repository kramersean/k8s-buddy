// Command buddy-api is K8s Buddy's HTTP service: the component Kubernetes
// probes, chaos experiments target, and Prometheus scrapes. This file owns
// everything internal/api does not -- reading and validating configuration
// from the environment, binding a socket, and the process lifecycle,
// including the graceful shutdown sequence that makes rolling this
// component out and back in zero-downtime.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kramersean/k8s-buddy/internal/api"
	"github.com/kramersean/k8s-buddy/internal/telemetry"
)

// version and commit are overridden at build time via
// `-ldflags "-X main.version=... -X main.commit=..."`; see the Makefile's
// build target. Left as their defaults for `go run` and any local build
// that skips ldflags.
var (
	version = "dev"
	commit  = "unknown"
)

// readHeaderTimeout bounds how long the server waits to receive a
// request's headers. gosec's G112 rule requires this to be set
// explicitly: without it, a slow-headers client (accidental or
// adversarial) can hold a connection open indefinitely and exhaust the
// server's file descriptors one connection at a time.
const readHeaderTimeout = 10 * time.Second

// readTimeout, writeTimeout, and idleTimeout are the remaining
// http.Server timeouts. None of buddy-api's handlers legitimately take
// anywhere near this long -- /work's worst case is WorkMaxDelay, which
// defaults to 200ms -- so these exist purely as a backstop against a
// stalled or malicious client, not as a real operational constraint.
const (
	readTimeout  = 30 * time.Second
	writeTimeout = 30 * time.Second
	idleTimeout  = 90 * time.Second
)

// shutdownDrainTimeout bounds how long the final shutdown phase waits for
// in-flight requests to finish once http.Server.Shutdown stops accepting
// new ones. It is fixed at 15s per the plan, not user-configurable like
// the pre-drain delay is, since it's a safety ceiling rather than a
// tuning knob tied to any particular cluster's kube-proxy propagation
// time.
const shutdownDrainTimeout = 15 * time.Second

// serveGoroutineDrainTimeout bounds how long run() waits, after
// gracefulShutdown returns, for the ListenAndServe goroutine to report
// its final result. httpServer.Shutdown has already returned by that
// point, so the goroutine should exit essentially immediately; this is
// only a safety ceiling against it never doing so.
const serveGoroutineDrainTimeout = 2 * time.Second

func main() {
	if err := run(); err != nil {
		// The structured logger normally used for everything else may not
		// exist yet if configuration loading itself is what failed, so
		// this bootstrap logger -- independent of any config -- is what
		// reports a startup failure.
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("buddy-api exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	logLevel, err := parseLogLevel(cfg.logLevel)
	if err != nil {
		return fmt.Errorf("parse log level: %w", err)
	}
	logger := telemetry.NewLogger(logLevel, os.Stdout)

	reg := prometheus.NewRegistry()
	buildInfo := telemetry.BuildInfo{
		Version:   version,
		Commit:    commit,
		GoVersion: runtime.Version(),
	}
	metrics := telemetry.NewMetrics(reg, buildInfo)

	srv := api.New(api.Config{
		PlantName:             cfg.plantName,
		Species:               cfg.species,
		LatencyBudget:         cfg.latencyBudget,
		WorkErrorRate:         cfg.workErrorRate,
		WorkMinDelay:          cfg.workMinDelay,
		WorkMaxDelay:          cfg.workMaxDelay,
		EnableChaosEndpoints:  cfg.enableChaosEndpoints,
		HealthRefreshInterval: cfg.healthRefreshInterval,
	}, logger, metrics, reg)

	// One line, everything a human reading `kubectl logs` needs to know
	// this process's identity and exact resolved configuration -- no
	// silent defaults to go hunting for.
	logger.Info("starting buddy-api",
		"version", version,
		"commit", commit,
		"goVersion", buildInfo.GoVersion,
		"plantName", cfg.plantName,
		"species", cfg.species,
		"port", cfg.port,
		"logLevel", cfg.logLevel,
		"latencyBudget", cfg.latencyBudget.String(),
		"workErrorRate", cfg.workErrorRate,
		"workMinDelay", cfg.workMinDelay.String(),
		"workMaxDelay", cfg.workMaxDelay.String(),
		"chaosEndpointsEnabled", cfg.enableChaosEndpoints,
		"shutdownDelay", cfg.shutdownDelay.String(),
		"healthRefreshInterval", cfg.healthRefreshInterval.String(),
	)

	httpServer := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
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

	// Keeps buddy_health_score, buddy_mood, and buddy_ready correct on
	// every scrape, independent of whether anything has ever called
	// /status -- see RunHealthRefresher's own doc comment for the bug this
	// closes. sigCtx is the same context the shutdown select below blocks
	// on, so this goroutine stops at the very first moment shutdown begins
	// (before gracefulShutdown's own phase 1), never leaking past process
	// lifetime and never delaying termination.
	go func() {
		if err := srv.RunHealthRefresher(sigCtx, cfg.healthRefreshInterval); err != nil {
			logger.Error("health refresher stopped", "error", err)
		}
	}()

	select {
	case err := <-serveErrs:
		// The listener died on its own (e.g. the port was already in
		// use) before any shutdown signal arrived -- nothing to drain,
		// just surface the error.
		return err
	case <-sigCtx.Done():
	}

	shutdownErr := gracefulShutdown(httpServer, srv, logger, cfg.shutdownDelay)

	// httpServer.Shutdown (inside gracefulShutdown) closes the listener,
	// which unblocks the ListenAndServe goroutine above and makes it send
	// its result on serveErrs. That goroutine already normalizes the
	// expected http.ErrServerClosed to nil before sending, so draining
	// here surfaces only a genuinely unexpected listener error -- one
	// that would otherwise sit in the buffered channel and be silently
	// discarded when this function returns. The drain is bounded so a
	// bug that somehow keeps the goroutine from ever sending can't hang
	// process shutdown.
	select {
	case serveErr := <-serveErrs:
		if serveErr != nil {
			logger.Error("http server reported an error during shutdown", "error", serveErr)
		}
	case <-time.After(serveGoroutineDrainTimeout):
		logger.Warn("timed out waiting for the http server goroutine to exit after shutdown")
	}

	return shutdownErr
}

// gracefulShutdown runs buddy-api's shutdown sequence in the exact order
// that makes a rolling deploy or a pod eviction zero-downtime instead of
// dropping in-flight requests. Each phase is logged as it happens, so the
// ordering is directly observable in `kubectl logs` rather than something
// a reader has to take on faith from the source.
//
// The order matters more than any individual step:
//
//  1. Mark not ready immediately. This is the very first thing that
//     happens, before any delay, so the clock on endpoint removal starts
//     ticking as early as possible.
//  2. Sleep shutdownDelay. See the comment on that call below -- this is
//     the step that makes the whole sequence correct rather than merely
//     plausible.
//  3. Shut the HTTP server down with a bounded drain timeout, so requests
//     already in flight get to finish instead of being cut off.
//
// Reordering steps 2 and 3 -- or skipping step 2 -- is the single most
// common way to implement "graceful shutdown" that isn't actually
// graceful: it looks correct in a local test (nothing else is watching
// readiness) and only breaks under real load-balanced traffic, exactly
// where it's expensive to notice.
func gracefulShutdown(httpServer *http.Server, srv *api.Server, logger *slog.Logger, delay time.Duration) error {
	logger.Info("shutdown: signal received, entering graceful shutdown sequence")

	// Phase 1: stop new traffic immediately. SetReady(false) flips what
	// /readyz reports on this pod's very next probe, which is what tells
	// the kubelet to pull this pod out of every Service's endpoint list.
	// This has to happen before anything else in this function -- in
	// particular, before the delay below -- because the delay's entire
	// purpose is to wait for a state change that must have already
	// started.
	srv.SetReady(false)
	logger.Info("shutdown: phase 1 complete -- readiness disabled, no longer routing new traffic")

	// Phase 2: wait for that readiness change to actually finish
	// propagating before touching the listener at all.
	//
	// Marking a pod not-ready doesn't remove it from Service endpoints
	// instantly: the kubelet has to observe the failing readiness probe,
	// the endpoints controller has to update the Endpoints/EndpointSlice
	// object, and then every node's kube-proxy (or equivalent dataplane)
	// has to receive and apply that update before it stops sending this
	// pod new traffic. That propagation is real wall-clock time, not
	// instantaneous, and it's happening across the whole cluster, not
	// just on this node. If the process shut its listener down as soon
	// as step 1 returned, any kube-proxy that hadn't yet caught up would
	// still route new connections here -- straight into a server that's
	// no longer accepting them, i.e. dropped requests during what should
	// have been a clean rollout. Sleeping here is what actually buys
	// zero-downtime; skipping it is what makes "graceful shutdown" a lie
	// that only shows up under real traffic.
	logger.Info("shutdown: phase 2 -- waiting for endpoint propagation", "delay", delay.String())
	time.Sleep(delay)
	logger.Info("shutdown: phase 2 complete")

	// Phase 3: now it's safe to stop accepting new connections and drain
	// whatever's already in flight. http.Server.Shutdown stops the
	// listener immediately, then waits (up to the context deadline) for
	// active handlers to return on their own.
	logger.Info("shutdown: phase 3 -- draining in-flight requests", "timeout", shutdownDrainTimeout.String())
	ctx, cancel := context.WithTimeout(context.Background(), shutdownDrainTimeout)
	defer cancel()
	if err := httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("shutdown: phase 3 complete -- all connections drained")

	logger.Info("shutdown: complete")
	return nil
}

// config is buddy-api's resolved runtime configuration, parsed and
// validated from environment variables by loadConfig.
type config struct {
	plantName             string
	species               string
	port                  int
	logLevel              string
	latencyBudget         time.Duration
	workErrorRate         float64
	workMinDelay          time.Duration
	workMaxDelay          time.Duration
	enableChaosEndpoints  bool
	shutdownDelay         time.Duration
	healthRefreshInterval time.Duration
}

// loadConfig reads buddy-api's configuration from the environment. Every
// variable has a documented default; an unset variable silently takes
// that default, but a variable that IS set to something invalid is
// always a startup error naming the variable and the bad value -- never
// a silent fallback to the default. A config error a human never sees
// because it was quietly swallowed is worse than a pod that fails to
// start with a clear message.
func loadConfig() (config, error) {
	cfg := config{
		plantName: getEnv("BUDDY_NAME", "fernie"),
		species:   getEnv("BUDDY_SPECIES", "fern"),
		logLevel:  getEnv("BUDDY_LOG_LEVEL", "info"),
	}

	var err error

	if cfg.port, err = parseIntEnv("BUDDY_PORT", 8080); err != nil {
		return config{}, err
	}
	if cfg.port < 1 || cfg.port > 65535 {
		return config{}, fmt.Errorf("invalid BUDDY_PORT: %d is not a valid TCP port (1-65535)", cfg.port)
	}

	// 150ms, not the plan's original 250ms: with the shipped
	// BUDDY_WORK_MAX_DELAY of 200ms, a 250ms budget would sit above every
	// possible sampled delay, making the "warning" outcome mathematically
	// unreachable under default configuration -- the demo could never
	// show it. 150ms sits strictly inside [WorkMinDelay, WorkMaxDelay],
	// so a sampled delay above budget is a real, reachable outcome. See
	// TestWork_ShippedDefaults_WarningIsReachable in internal/api for the
	// regression guard against this becoming unreachable again.
	if cfg.latencyBudget, err = parseDurationEnv("BUDDY_LATENCY_BUDGET", 150*time.Millisecond); err != nil {
		return config{}, err
	}
	// A non-positive budget isn't a valid "small budget", it's a
	// different feature entirely: mood.Signals.Score treats
	// LatencyBudget <= 0 as "no budget configured" (full latency marks
	// always awarded), and sampleWorkOutcome can never classify anything
	// as a warning. Silently accepting e.g. BUDDY_LATENCY_BUDGET=-1s
	// would quietly disable an entire outcome class -- exactly the
	// silent-fallback failure mode this file's validation exists to
	// prevent, just arriving through a duration instead of a missing
	// variable.
	if cfg.latencyBudget <= 0 {
		return config{}, fmt.Errorf("invalid BUDDY_LATENCY_BUDGET: %s must be a positive duration", cfg.latencyBudget)
	}

	if cfg.workErrorRate, err = parseFloatEnv("BUDDY_WORK_ERROR_RATE", 0.05); err != nil {
		return config{}, err
	}
	if cfg.workErrorRate < 0 || cfg.workErrorRate > 1 {
		return config{}, fmt.Errorf("invalid BUDDY_WORK_ERROR_RATE: %v is not in [0,1]", cfg.workErrorRate)
	}

	if cfg.workMinDelay, err = parseDurationEnv("BUDDY_WORK_MIN_DELAY", 10*time.Millisecond); err != nil {
		return config{}, err
	}
	if cfg.workMinDelay < 0 {
		return config{}, fmt.Errorf("invalid BUDDY_WORK_MIN_DELAY: %s must not be negative", cfg.workMinDelay)
	}
	if cfg.workMaxDelay, err = parseDurationEnv("BUDDY_WORK_MAX_DELAY", 200*time.Millisecond); err != nil {
		return config{}, err
	}
	if cfg.workMaxDelay < 0 {
		return config{}, fmt.Errorf("invalid BUDDY_WORK_MAX_DELAY: %s must not be negative", cfg.workMaxDelay)
	}
	if cfg.workMinDelay > cfg.workMaxDelay {
		return config{}, fmt.Errorf(
			"invalid BUDDY_WORK_MIN_DELAY/BUDDY_WORK_MAX_DELAY: min (%s) must be <= max (%s)",
			cfg.workMinDelay, cfg.workMaxDelay,
		)
	}

	if cfg.enableChaosEndpoints, err = parseBoolEnv("BUDDY_ENABLE_CHAOS_ENDPOINTS", false); err != nil {
		return config{}, err
	}
	if cfg.shutdownDelay, err = parseDurationEnv("BUDDY_SHUTDOWN_DELAY", 5*time.Second); err != nil {
		return config{}, err
	}
	if cfg.shutdownDelay < 0 {
		return config{}, fmt.Errorf("invalid BUDDY_SHUTDOWN_DELAY: %s must not be negative", cfg.shutdownDelay)
	}

	if cfg.healthRefreshInterval, err = parseDurationEnv("BUDDY_HEALTH_REFRESH_INTERVAL", 5*time.Second); err != nil {
		return config{}, err
	}
	// Non-positive isn't a valid "refresh instantly" or "disable the
	// refresher" -- it's an invalid config, full stop. api.RunHealthRefresher
	// treats it the same way (returns an error rather than busy-looping via
	// a <=0 time.Ticker, which panics), but the check belongs here first: a
	// misconfigured BUDDY_HEALTH_REFRESH_INTERVAL should fail loudly at
	// startup, naming the bad value, not surface later as a goroutine that
	// silently never started.
	if cfg.healthRefreshInterval <= 0 {
		return config{}, fmt.Errorf("invalid BUDDY_HEALTH_REFRESH_INTERVAL: %s must be a positive duration", cfg.healthRefreshInterval)
	}

	return cfg, nil
}

// getEnv returns the environment variable key's value, or def if it is
// unset or empty. An empty string is treated the same as unset for every
// variable buddy-api reads, since none of them has a meaningful empty
// value.
func getEnv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// parseIntEnv parses key as an integer, returning def if it is unset or
// empty, and an error naming key if it is set but not a valid integer.
func parseIntEnv(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %q is not a valid integer: %w", key, v, err)
	}
	return n, nil
}

// parseFloatEnv parses key as a float64, returning def if it is unset or
// empty, and an error naming key if it is set but not a valid float.
func parseFloatEnv(key string, def float64) (float64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %q is not a valid float: %w", key, v, err)
	}
	return f, nil
}

// parseBoolEnv parses key with strconv.ParseBool, returning def if it is
// unset or empty, and an error naming key if it is set but not a valid
// boolean.
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

// parseDurationEnv parses key with time.ParseDuration, returning def if
// it is unset or empty, and an error naming key if it is set but not a
// valid duration.
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

// parseLogLevel maps BUDDY_LOG_LEVEL's string value to a slog.Level. It
// is case-insensitive and accepts "warning" as a synonym for "warn";
// anything else is a startup error naming the value it rejected.
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
		return 0, fmt.Errorf("invalid BUDDY_LOG_LEVEL: %q (want debug, info, warn, or error)", s)
	}
}
