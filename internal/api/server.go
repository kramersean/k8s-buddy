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
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kramersean/k8s-buddy/internal/mood"
	"github.com/kramersean/k8s-buddy/internal/telemetry"
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
	// HealthRefreshInterval is how often RunHealthRefresher recomputes and
	// publishes buddy_health_score, buddy_mood, and buddy_ready, independent
	// of whether anything has called /status. See RunHealthRefresher's own
	// doc comment for why this exists at all: without it, those three
	// gauges are wrong -- not merely stale -- on every pod nothing has ever
	// curled /status on, which in practice is most pods most of the time,
	// since Prometheus scrapes /metrics, never /status. cmd/buddy-api is
	// responsible for validating this is positive before RunHealthRefresher
	// is ever called, the same way it validates every other Config field
	// New itself does not (see New's own doc comment).
	HealthRefreshInterval time.Duration
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

	// work is the rolling window of recent /work observations that feeds
	// the ErrorRate and P95Latency signals on /status. See window.go, and
	// currentReport below, for why a window rather than lifetime counters.
	work workWindow
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
// Ready comes straight from s.ready -- this process's own in-memory state,
// known with total confidence. ErrorRate and P95Latency both come from the
// rolling window of the last workWindowSize /work observations (see
// window.go), so the mood tracks how this pod is behaving *right now*.
//
// The window replaced an earlier design that used lifetime failed/total
// counters for ErrorRate and left P95Latency at its zero value on the
// theory that Prometheus's histogram_quantile computes percentiles better.
// It does -- for dashboards. But it left /status structurally unable to
// report anything but a perfect latency score, and lifetime counters mean
// a pod that served a million clean requests and is failing every request
// now still reports a near-zero error rate. Between them, five of the six
// moods were unreachable in the shipped system: /status was a constant, not
// a signal. Prometheus still gets every observation via ObserveWork and is
// still the right place to graph the percentile over time; this window is
// what lets the pod answer "how am I doing?" about itself.
//
// RestartCount stays 0 deliberately. Container restarts are a property of
// the Pod, visible only in its status via the Kubernetes API -- a process
// cannot count its own restarts, since a restart is precisely the event
// that destroys the memory that would hold the count. Reporting anything
// but 0 from in here would be a guess.
func (s *Server) currentReport() mood.Report {
	errorRate, p95, _ := s.work.stats()

	signals := mood.Signals{
		Ready:         s.ready.Load(),
		ErrorRate:     errorRate,
		P95Latency:    p95,
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

// RunHealthRefresher periodically pushes this Server's current health
// signals into Prometheus, independent of whether anything has ever called
// /status.
//
// Why this exists: buddy_health_score, buddy_mood, and buddy_ready used to
// be set ONLY by statusHandler and SetReady -- both request/event-driven,
// never by anything Prometheus's own scrape of /metrics triggers. A pod
// that has served real traffic, is perfectly healthy, and that no human or
// script has ever curled /status on therefore reported health 0, ready 0,
// and NO buddy_mood series at all (telemetry.NewMetrics pre-zeroes every
// mood series only at construction time via SetHealth's own zeroing logic,
// which itself is never invoked until the first call) -- indistinguishable
// from a dead plant on any dashboard or alert built against those three
// metrics, for a fleet that was never anything but healthy. This is the
// same class of bug Plan 1 already fixed once, for buddy_work_requests_total
// and buddy_work_duration_seconds, by pre-initializing every outcome series
// at construction time in telemetry.NewMetrics; these three gauges slipped
// through the same door because they are computed from LIVE state (the
// rolling /work window, current readiness) rather than a fixed label
// vocabulary, so there was nothing to pre-initialize -- only something to
// keep refreshing.
//
// RunHealthRefresher recomputes the signal through s.currentReport, the
// EXACT same method statusHandler calls, and pushes it with the same
// s.syncMetrics statusHandler and SetReady already use -- so the gauge and
// whatever a human sees from /status can never disagree, and there is
// exactly one place (currentReport) that derives a mood.Report from this
// Server's state, never two.
//
// It blocks until ctx is done, at which point it returns nil -- callers
// run it in its own goroutine (see cmd/buddy-api's run(), which passes the
// same context signal.NotifyContext returns, so this goroutine stops at
// the very start of the graceful shutdown sequence, before anything else,
// and never blocks or delays termination). interval must be positive:
// cmd/buddy-api treats a non-positive BUDDY_HEALTH_REFRESH_INTERVAL as a
// startup config error, the same way it treats a non-positive
// BUDDY_LATENCY_BUDGET or BUDDY_SHUTDOWN_DELAY -- never a silently disabled
// refresher and never a busy loop. This method still checks and returns an
// error rather than trusting that validation blindly, since a defensive
// check here is what makes the "config error, not a busy loop" contract
// directly testable in this package without going through cmd/buddy-api's
// own env parsing at all.
func (s *Server) RunHealthRefresher(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("health refresh interval must be positive, got %s", interval)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.syncMetrics(s.currentReport())
		}
	}
}
