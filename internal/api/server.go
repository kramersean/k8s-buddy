// Package api implements buddy-api's HTTP surface: the liveness and
// readiness probes Kubernetes calls, the simulated-work and status
// endpoints the demo is built around, the chaos hook that flips
// readiness on command, and the /metrics endpoint Prometheus scrapes.
//
// This package is where Kubernetes probe semantics actually live, so its
// doc comments are deliberately verbose about *why* each handler behaves
// the way it does, not just what it does -- see healthzHandler and
// readyzHandler in handlers.go, and cmd/buddy-api's graceful shutdown
// sequence, for the two places that reasoning matters most.
package api

import (
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sean-kramer/k8s-buddy/internal/mood"
	"github.com/sean-kramer/k8s-buddy/internal/telemetry"
)

// Config controls how a Server built by New behaves. cmd/buddy-api is
// responsible for populating one of these from validated environment
// variables; this package assumes the values it's given are already
// sane (for example, WorkMinDelay <= WorkMaxDelay) rather than
// re-validating them, since New has no error return with which to reject
// a bad Config.
type Config struct {
	// PlantName is this plant's display name, reported on /status.
	PlantName string
	// Species is this plant's species, reported on /status.
	Species string
	// LatencyBudget is the /work latency above which a request is
	// classified "warning" instead of "success", and also the budget
	// mood.Signals.Score measures P95Latency against.
	LatencyBudget time.Duration
	// WorkErrorRate is the probability, in [0,1], that /work reports a
	// simulated failure.
	WorkErrorRate float64
	// WorkMinDelay is the lower bound of /work's simulated delay.
	WorkMinDelay time.Duration
	// WorkMaxDelay is the upper bound of /work's simulated delay.
	WorkMaxDelay time.Duration
	// EnableChaosEndpoints controls whether POST /chaos/readiness is
	// registered at all. When false, the route is never added to the
	// mux -- a request to it gets an ordinary 404, not a 403 or a
	// disabled-feature error, because a chaos endpoint that merely
	// *rejects* requests in production still means the endpoint exists
	// and can be probed; one that was never registered cannot.
	EnableChaosEndpoints bool
	// Rand is the randomness source /work uses to sample its simulated
	// delay and outcome. A nil Rand means Server picks a time-seeded
	// one; tests inject a fixed-seed *rand.Rand so /work's behavior is
	// deterministic and assertable.
	Rand *rand.Rand
}

// Server is buddy-api's HTTP service. Build one with New and mount its
// Handler; Server itself never listens on a socket, so it has no Start
// or Close method -- that lifecycle belongs to cmd/buddy-api, which owns
// the *http.Server and the process's signal handling.
type Server struct {
	cfg     Config
	log     *slog.Logger
	metrics *telemetry.Metrics
	reg     *prometheus.Registry

	startedAt time.Time

	// ready backs both /readyz and the readiness component of /status.
	// atomic.Bool rather than a mutex-guarded bool because it is read on
	// every /healthz-adjacent request and written rarely (chaos, and
	// once at shutdown) -- a classic single-writer-many-readers shape
	// atomics are built for.
	ready atomic.Bool

	// rand and randMu back /work's simulated delay and outcome
	// sampling. *rand.Rand is not safe for concurrent use, and /work can
	// be hit concurrently, so every draw from rand is taken under randMu.
	randMu sync.Mutex
	rand   *rand.Rand

	// totalWork and failedWork are lifetime counters (not a rolling
	// window) feeding /status's self-reported ErrorRate. See
	// currentReport's doc comment for why this package stops at a
	// lifetime ratio rather than reimplementing a windowed percentile
	// tracker for latency too.
	totalWork  atomic.Int64
	failedWork atomic.Int64
}

// New constructs a Server. It never fails -- there is nothing in Config
// that can be invalid in a way this constructor is responsible for
// catching; cmd/buddy-api validates the environment variables that
// populate Config before this is ever called.
//
// A Server starts ready. Plan 1's buddy-api has no external dependency to
// wait on during startup (no database migration, no downstream service
// handshake), so there is no "still warming up" phase for readiness to
// protect against. From here, only two things ever change it: the
// /chaos/readiness endpoint, and the first step of cmd/buddy-api's
// graceful shutdown sequence.
func New(cfg Config, log *slog.Logger, m *telemetry.Metrics, reg *prometheus.Registry) *Server {
	resolvedRand := cfg.Rand
	if resolvedRand == nil {
		//nolint:gosec // G404: seeds a non-cryptographic RNG used only to
		// pick a simulated /work delay and outcome; never security-sensitive.
		resolvedRand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}

	s := &Server{
		cfg:       cfg,
		log:       log,
		metrics:   m,
		reg:       reg,
		startedAt: time.Now(),
		rand:      resolvedRand,
	}
	s.ready.Store(true)
	return s
}

// Handler returns the fully-wired HTTP handler: every route, plus the
// middleware chain wrapped around all of them. Each call builds a fresh
// *http.ServeMux, so calling Handler more than once is safe (if unusual)
// and never mutates shared state.
//
// EnableChaosEndpoints gates registration of POST /chaos/readiness at the
// mux level: when it's false, the line that would add that route simply
// doesn't run, so the mux has no pattern for it at all and any request to
// it falls through to the mux's own 404 -- the same 404 an entirely made
// -up path would get. A chaos endpoint reachable (even to be told "no")
// in a default deployment is a security finding; one that doesn't exist
// cannot be.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.healthzHandler)
	mux.HandleFunc("GET /readyz", s.readyzHandler)
	mux.HandleFunc("GET /status", s.statusHandler)
	mux.HandleFunc("GET /work", s.workHandler)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.reg, promhttp.HandlerOpts{}))

	if s.cfg.EnableChaosEndpoints {
		mux.HandleFunc("POST /chaos/readiness", s.chaosReadinessHandler)
	}

	// Outermost to innermost: request ID/logging wraps everything so its
	// timer covers metrics recording and panic recovery too; metrics
	// wraps recovery so a recovered panic's 500 still gets counted under
	// the route it happened on; recovery wraps the mux directly so it's
	// the last line of defense before a handler's own panic would
	// otherwise unwind out of ServeHTTP entirely.
	return s.withRequestLogging(s.withMetrics(s.withRecovery(mux)))
}

// SetReady flips readiness immediately and pushes the resulting state
// into Prometheus via syncMetrics, so buddy_ready (and buddy_health_score
// and buddy_mood, which move with it) never lag behind what /readyz is
// already telling callers.
//
// SetReady has exactly two callers: the /chaos/readiness handler, and --
// far more importantly -- the very first step of cmd/buddy-api's
// graceful shutdown sequence. Calling this before anything else during
// shutdown is what starts the clock on kube-proxy withdrawing this pod
// from Service endpoints while the process is still fully alive and
// still finishing whatever requests were already in flight.
func (s *Server) SetReady(ready bool) {
	s.ready.Store(ready)
	s.syncMetrics(s.currentReport())
}

// currentReport builds this instant's mood.Report from the server's live
// state.
//
// Ready comes straight from s.ready -- this process's own in-memory
// state, known with total confidence. ErrorRate is a lifetime ratio of
// failedWork/totalWork from /work's own outcomes; cheap enough to keep
// exactly, so it does.
//
// P95Latency is deliberately left at its zero value. Computing a real
// percentile requires a windowed quantile estimator, and this package
// already feeds every /work observation into
// buddy_work_duration_seconds (see workHandler and ObserveWork) --
// Prometheus computes exactly that percentile, over a real sliding
// window, via histogram_quantile, more correctly than an in-process
// approximation could. Reimplementing that here would duplicate logic
// Prometheus already does better and risk this endpoint's number
// disagreeing with what Grafana shows for the same data.
func (s *Server) currentReport() mood.Report {
	var errorRate float64
	if total := s.totalWork.Load(); total > 0 {
		errorRate = float64(s.failedWork.Load()) / float64(total)
	}

	signals := mood.Signals{
		Ready:         s.ready.Load(),
		ErrorRate:     errorRate,
		LatencyBudget: s.cfg.LatencyBudget,
	}

	uptime := time.Since(s.startedAt)
	return mood.NewReport(signals, s.cfg.PlantName, s.cfg.Species, uptime)
}

// syncMetrics pushes r's health score, mood, and readiness into
// Prometheus in one call, so the three gauges can never individually
// disagree with the mood.Report a caller of /status just received.
// Called from statusHandler on every /status hit, and from SetReady on
// every readiness transition, so a scrape landing at any point in
// between still reads whichever of those happened most recently rather
// than a value stale since process start.
func (s *Server) syncMetrics(r mood.Report) {
	s.metrics.SetHealth(r.HealthScore, r.Mood, r.Ready)
}
