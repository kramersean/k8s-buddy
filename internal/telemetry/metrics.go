// Package telemetry provides the Prometheus metrics and structured-logging
// foundation shared by every K8s Buddy component. The metric names, label
// sets, and help strings defined here are a public contract: every Grafana
// panel in this project is drawn directly from them, so a rename or a
// dropped label here is a breaking change to the observability story, not
// an internal refactor.
package telemetry

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/sean-kramer/k8s-buddy/internal/mood"
)

// BuildInfo carries the version, commit, and Go runtime version reported by
// the buddy_build_info metric. cmd/buddy-api populates it at startup from
// linker-injected variables and runtime.Version.
type BuildInfo struct {
	// Version is the application's semantic version or build tag.
	Version string
	// Commit is the git commit SHA the binary was built from.
	Commit string
	// GoVersion is the Go toolchain version used to build the binary.
	GoVersion string
}

// The complete outcome vocabulary for a unit of simulated work. These are
// the canonical definitions: they are the "outcome" label values of
// buddy_work_requests_total and buddy_work_duration_seconds, and they are
// also the strings internal/api puts in the /work response body. They live
// here, in the package that owns the metric contract, so there is exactly
// one source of truth -- a second copy in the HTTP layer could drift and
// silently split one time series into two.
const (
	// OutcomeSuccess is a /work request that finished within budget.
	OutcomeSuccess = "success"
	// OutcomeWarning is a /work request that succeeded but exceeded the
	// configured latency budget.
	OutcomeWarning = "warning"
	// OutcomeFailure is a /work request that failed.
	OutcomeFailure = "failure"
)

// Outcomes returns every value in the outcome vocabulary. Each call
// returns a new slice, so callers may freely mutate the result.
func Outcomes() []string {
	return []string{OutcomeSuccess, OutcomeWarning, OutcomeFailure}
}

// workDurationBuckets are the histogram boundaries, in seconds, for
// buddy_work_duration_seconds. They span 5ms to 5s so both a fast success
// and a deliberately slow simulated /work request land in a meaningful
// bucket.
var workDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

// Metrics is the complete set of Prometheus collectors K8s Buddy exposes.
// Every metric name is prefixed buddy_, and its name, label set, and help
// string are a public contract that Grafana dashboards are built directly
// against. Construct a Metrics with NewMetrics; the zero value is not
// usable, since its collectors are never registered.
type Metrics struct {
	healthScore  prometheus.Gauge
	mood         *prometheus.GaugeVec
	ready        prometheus.Gauge
	workRequests *prometheus.CounterVec
	workDuration *prometheus.HistogramVec
	httpRequests *prometheus.CounterVec
}

// NewMetrics registers every K8s Buddy metric against reg and returns a
// ready-to-use Metrics. Registration always targets the supplied
// Registerer, never prometheus.DefaultRegisterer: a global registry would
// make tests order-dependent (one test's metrics leaking into another's
// exposition), so every caller -- production and tests alike -- must bring
// its own. Two independent NewMetrics calls against two independent
// registries are always safe.
//
// Registering the same metric name twice against the same reg is a
// programming error, not a recoverable runtime condition, so NewMetrics
// panics rather than swallowing it: it builds every collector through
// promauto.With(reg), which panics on a duplicate registration instead of
// returning an error a caller could accidentally ignore.
//
// buddy_build_info is set to 1 with bi's labels exactly once, here, since
// build metadata is fixed for the lifetime of the process.
//
// Every /work outcome series is pre-initialized to 0 before this returns.
// A Prometheus *Vec exports nothing at all for a label combination it has
// never been given, so without this a freshly started pod would export no
// buddy_work_requests_total series whatsoever until its first /work call:
// an alert or dashboard querying the failure rate would read "no data"
// rather than the truthful 0, and rate() over a series that springs into
// existence mid-window is not the same thing as rate() over a series that
// was always there. This is the same staleness class SetHealth already
// avoids by explicitly zeroing every inactive buddy_mood series.
func NewMetrics(reg prometheus.Registerer, bi BuildInfo) *Metrics {
	factory := promauto.With(reg)

	m := &Metrics{
		healthScore: factory.NewGauge(prometheus.GaugeOpts{
			Name: "buddy_health_score",
			Help: "Current plant health, 0-100, where 100 is fully healthy.",
		}),
		mood: factory.NewGaugeVec(prometheus.GaugeOpts{
			Name: "buddy_mood",
			Help: "1 for the plant's currently active mood, 0 for every other mood.",
		}, []string{"mood"}),
		ready: factory.NewGauge(prometheus.GaugeOpts{
			Name: "buddy_ready",
			Help: "1 when the most recent readiness check passed, 0 otherwise.",
		}),
		workRequests: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "buddy_work_requests_total",
			Help: "Total count of /work requests, by outcome (success, warning, or failure).",
		}, []string{"outcome"}),
		workDuration: factory.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "buddy_work_duration_seconds",
			Help:    "Latency of /work requests in seconds, by outcome.",
			Buckets: workDurationBuckets,
		}, []string{"outcome"}),
		httpRequests: factory.NewCounterVec(prometheus.CounterOpts{
			Name: "buddy_http_requests_total",
			Help: "Total count of HTTP requests, by route pattern, method, and status code.",
		}, []string{"path", "method", "code"}),
	}

	// WithLabelValues creates the child collector for a label combination
	// if it does not already exist, so simply asking for each outcome is
	// what materializes the series at 0. Both the counter and the
	// histogram are pre-created, so their outcome label sets always match.
	for _, outcome := range Outcomes() {
		m.workRequests.WithLabelValues(outcome)
		m.workDuration.WithLabelValues(outcome)
	}

	buildInfo := factory.NewGaugeVec(prometheus.GaugeOpts{
		Name: "buddy_build_info",
		Help: "Always 1; labels report the running build's version, commit, and Go version.",
	}, []string{"version", "commit", "go_version"})
	buildInfo.WithLabelValues(bi.Version, bi.Commit, bi.GoVersion).Set(1)

	return m
}

// ObserveWork records the outcome and latency of a single /work request. It
// increments buddy_work_requests_total for outcome and records d, in
// seconds, into buddy_work_duration_seconds for the same outcome. outcome
// is expected to be one of OutcomeSuccess, OutcomeWarning, or
// OutcomeFailure; all three already exist at 0 (see NewMetrics), so this
// method only ever advances a series that is already being exported.
func (m *Metrics) ObserveWork(outcome string, d time.Duration) {
	m.workRequests.WithLabelValues(outcome).Inc()
	m.workDuration.WithLabelValues(outcome).Observe(d.Seconds())
}

// ObserveHTTP records one HTTP request against buddy_http_requests_total.
//
// path must be the matched ROUTE PATTERN (for example "/work" or
// "GET /healthz"), never the raw request URL. Raw URLs carry query strings,
// path parameters, and whatever an attacker or client probes with, all of
// which would make the path label unbounded in cardinality -- exactly what
// Prometheus label design must avoid. internal/api's withMetrics
// middleware is responsible for resolving the matched pattern before
// calling this method.
//
// code is converted to its decimal string form (e.g. "200"), since
// Prometheus label values are always strings.
func (m *Metrics) ObserveHTTP(path, method string, code int) {
	m.httpRequests.WithLabelValues(path, method, strconv.Itoa(code)).Inc()
}

// SetHealth updates buddy_health_score, buddy_ready, and buddy_mood
// together so a single call always leaves the three mutually consistent.
//
// buddy_mood is set to 1 for active and explicitly to 0 for every other
// mood in mood.AllMoods(). The explicit zeroing matters: a Prometheus gauge
// only reports whatever it was last Set to. If SetHealth only ever set the
// currently active mood's series, the previously active mood's series
// would keep reporting its last value (1) forever instead of dropping to
// 0 -- going stale rather than disappearing -- and a Grafana panel built on
// buddy_mood would render two moods as simultaneously active.
func (m *Metrics) SetHealth(score float64, active mood.Mood, ready bool) {
	m.healthScore.Set(score)

	readyValue := 0.0
	if ready {
		readyValue = 1.0
	}
	m.ready.Set(readyValue)

	for _, candidate := range mood.AllMoods() {
		value := 0.0
		if candidate == active {
			value = 1.0
		}
		m.mood.WithLabelValues(string(candidate)).Set(value)
	}
}
