package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/sean-kramer/k8s-buddy/internal/mood"
	"github.com/sean-kramer/k8s-buddy/internal/telemetry"
)

// newTestServer builds a Server against a fresh registry and a discard
// logger, so tests never share Prometheus state or spam test output with
// JSON log lines. Any zero-valued PlantName/Species in cfg is filled in
// with a placeholder so /status always has something to report.
func newTestServer(t *testing.T, cfg Config) (*Server, *prometheus.Registry) {
	t.Helper()

	reg := prometheus.NewRegistry()
	m := telemetry.NewMetrics(reg, telemetry.BuildInfo{
		Version: "test", Commit: "test", GoVersion: "go-test",
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	if cfg.PlantName == "" {
		cfg.PlantName = "fernie"
	}
	if cfg.Species == "" {
		cfg.Species = "fern"
	}

	return New(cfg, logger, m, reg), reg
}

// doRequest sends a single request through the server's fully-wired
// Handler (middleware included) and returns the recorded response.
func doRequest(s *Server, method, path string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// findSeries returns the dto.Metric within family that carries exactly
// labels, mirroring the helper telemetry's own tests use, so assertions
// here can check specific label combinations without needing access to
// telemetry's unexported collector fields.
func findSeries(t *testing.T, reg *prometheus.Registry, family string, labels map[string]string) (*dto.Metric, bool) {
	t.Helper()

	families, err := reg.Gather()
	require.NoError(t, err)

	for _, fam := range families {
		if fam.GetName() != family {
			continue
		}
		for _, metric := range fam.GetMetric() {
			got := map[string]string{}
			for _, lp := range metric.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			if len(got) != len(labels) {
				continue
			}
			match := true
			for k, v := range labels {
				if got[k] != v {
					match = false
					break
				}
			}
			if match {
				return metric, true
			}
		}
	}
	return nil, false
}

// -- /healthz: the key probe-semantics test ---------------------------------

func TestHealthz_AlwaysOK_EvenWhenNotReady(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, Config{})
	s.SetReady(false)

	rec := doRequest(s, http.MethodGet, "/healthz", nil)

	require.Equal(t, http.StatusOK, rec.Code,
		"liveness must stay 200 while the process is alive, even when readiness has been "+
			"flipped off -- healthz is a crash-loop guard, not a mirror of readyz")

	var body statusBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "ok", body.Status)
}

func TestHealthz_AlwaysOK_WhenReady(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, Config{})
	s.SetReady(true)

	rec := doRequest(s, http.MethodGet, "/healthz", nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

// -- /readyz ------------------------------------------------------------

func TestReadyz_200WhenReady_503WhenNot(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, Config{})

	s.SetReady(true)
	rec := doRequest(s, http.MethodGet, "/readyz", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	s.SetReady(false)
	rec = doRequest(s, http.MethodGet, "/readyz", nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body statusBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "not ready", body.Status)
}

// -- /status --------------------------------------------------------------

func TestStatus_DecodesIntoMoodReport_AndMoodMatchesScore(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, Config{LatencyBudget: 250 * time.Millisecond})
	s.SetReady(true)

	rec := doRequest(s, http.MethodGet, "/status", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var report mood.Report
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&report))

	require.Equal(t, mood.FromScore(report.HealthScore), report.Mood,
		"the reported mood must match what FromScore derives from the reported score")
	require.Equal(t, report.Mood.Message(), report.Message)
	require.True(t, report.Ready)
	require.Equal(t, "fernie", report.Name)
	require.Equal(t, "fern", report.Species)
}

func TestStatus_NotReady_CapsScoreAndMood(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, Config{})
	s.SetReady(false)

	rec := doRequest(s, http.MethodGet, "/status", nil)

	var report mood.Report
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&report))

	require.False(t, report.Ready)
	require.LessOrEqual(t, report.HealthScore, 35.0,
		"an unready server's score must be capped, per mood.Signals.Score's documented behavior")
	require.Equal(t, mood.FromScore(report.HealthScore), report.Mood)
}

func TestStatus_SyncsIntoMetrics(t *testing.T) {
	t.Parallel()

	s, reg := newTestServer(t, Config{})
	s.SetReady(true)

	rec := doRequest(s, http.MethodGet, "/status", nil)
	var report mood.Report
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&report))

	metric, ok := findSeries(t, reg, "buddy_health_score", nil)
	require.True(t, ok)
	require.InDelta(t, report.HealthScore, metric.GetGauge().GetValue(), 0.001)

	readyMetric, ok := findSeries(t, reg, "buddy_ready", nil)
	require.True(t, ok)
	require.Equal(t, 1.0, readyMetric.GetGauge().GetValue())
}

// -- /work ------------------------------------------------------------------

func TestWork_ErrorRateOne_AlwaysFails(t *testing.T) {
	t.Parallel()

	s, reg := newTestServer(t, Config{
		WorkErrorRate: 1.0,
		WorkMinDelay:  time.Millisecond,
		WorkMaxDelay:  2 * time.Millisecond,
		Rand:          rand.New(rand.NewSource(1)), //nolint:gosec // deterministic test seed
	})

	rec := doRequest(s, http.MethodGet, "/work", nil)
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	var body workResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	require.Equal(t, workOutcomeFailure, body.Outcome)

	metric, ok := findSeries(t, reg, "buddy_work_requests_total", map[string]string{"outcome": "failure"})
	require.True(t, ok, "expected a failure series in buddy_work_requests_total")
	require.Equal(t, 1.0, metric.GetCounter().GetValue())
}

func TestWork_ErrorRateZero_AlwaysSucceeds(t *testing.T) {
	t.Parallel()

	s, reg := newTestServer(t, Config{
		WorkErrorRate: 0.0,
		LatencyBudget: time.Second, // generous, so a short delay never trips a warning
		WorkMinDelay:  time.Millisecond,
		WorkMaxDelay:  2 * time.Millisecond,
		Rand:          rand.New(rand.NewSource(1)), //nolint:gosec // deterministic test seed
	})

	rec := doRequest(s, http.MethodGet, "/work", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var body workResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	require.Equal(t, workOutcomeSuccess, body.Outcome)

	metric, ok := findSeries(t, reg, "buddy_work_requests_total", map[string]string{"outcome": "success"})
	require.True(t, ok)
	require.Equal(t, 1.0, metric.GetCounter().GetValue())

	_, failed := findSeries(t, reg, "buddy_work_requests_total", map[string]string{"outcome": "failure"})
	require.False(t, failed, "an all-success run must not create a failure series")
}

func TestWork_RecordsDurationHistogramObservation(t *testing.T) {
	t.Parallel()

	s, reg := newTestServer(t, Config{
		WorkErrorRate: 0.0,
		LatencyBudget: time.Second,
		WorkMinDelay:  time.Millisecond,
		WorkMaxDelay:  2 * time.Millisecond,
		Rand:          rand.New(rand.NewSource(2)), //nolint:gosec // deterministic test seed
	})

	doRequest(s, http.MethodGet, "/work", nil)

	metric, ok := findSeries(t, reg, "buddy_work_duration_seconds", map[string]string{"outcome": "success"})
	require.True(t, ok, "expected a success series in buddy_work_duration_seconds")
	require.EqualValues(t, 1, metric.GetHistogram().GetSampleCount())
}

func TestWork_DelayExceedsBudget_ReturnsDistinctWarningBody(t *testing.T) {
	t.Parallel()

	s, reg := newTestServer(t, Config{
		WorkErrorRate: 0.0,
		LatencyBudget: time.Millisecond, // tiny budget, so any sampled delay >= min trips it
		WorkMinDelay:  5 * time.Millisecond,
		WorkMaxDelay:  6 * time.Millisecond,
		Rand:          rand.New(rand.NewSource(3)), //nolint:gosec // deterministic test seed
	})

	rec := doRequest(s, http.MethodGet, "/work", nil)
	require.Equal(t, http.StatusOK, rec.Code, "a warning is still a 200, distinct only in body")

	var body workResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
	require.Equal(t, workOutcomeWarning, body.Outcome)

	require.NotEqual(t, workMessage(workOutcomeSuccess, 0, 0), body.Message,
		"a warning's message body must differ from a success's")

	metric, ok := findSeries(t, reg, "buddy_work_requests_total", map[string]string{"outcome": "warning"})
	require.True(t, ok)
	require.Equal(t, 1.0, metric.GetCounter().GetValue())
}

// -- /chaos/readiness ---------------------------------------------------

func TestChaosReadiness_NotRegistered_WhenDisabled(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, Config{EnableChaosEndpoints: false})

	rec := doRequest(s, http.MethodPost, "/chaos/readiness", strings.NewReader(`{"ready":false}`))
	require.Equal(t, http.StatusNotFound, rec.Code,
		"a disabled chaos endpoint must genuinely not exist (404), not just refuse the request")
}

func TestChaosReadiness_FlipsReadiness_WhenEnabled(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, Config{EnableChaosEndpoints: true})
	s.SetReady(true)

	rec := doRequest(s, http.MethodPost, "/chaos/readiness", strings.NewReader(`{"ready":false}`))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = doRequest(s, http.MethodGet, "/readyz", nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "readiness must actually flip")

	rec = doRequest(s, http.MethodPost, "/chaos/readiness", strings.NewReader(`{"ready":true}`))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = doRequest(s, http.MethodGet, "/readyz", nil)
	require.Equal(t, http.StatusOK, rec.Code, "readiness must flip back")
}

func TestChaosReadiness_InvalidBody_400(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, Config{EnableChaosEndpoints: true})

	rec := doRequest(s, http.MethodPost, "/chaos/readiness", strings.NewReader(`not json`))
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// -- /metrics -------------------------------------------------------------

func TestMetrics_ExpositionContainsBuildInfo(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, Config{})

	rec := doRequest(s, http.MethodGet, "/metrics", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "buddy_build_info")
}

// -- middleware: panic recovery ------------------------------------------

func TestWithRecovery_PanicBecomes500_WithoutCrashingTheProcess(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, Config{})

	panicky := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rec := httptest.NewRecorder()

	require.NotPanics(t, func() {
		s.withRecovery(panicky).ServeHTTP(rec, req)
	})
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	var body statusBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "internal error", body.Status)
}

// -- middleware: metrics cardinality guard --------------------------------

func TestWithMetrics_RecordsRoutePattern_ForAKnownRoute(t *testing.T) {
	t.Parallel()

	s, reg := newTestServer(t, Config{})

	doRequest(s, http.MethodGet, "/healthz", nil)

	metric, ok := findSeries(t, reg, "buddy_http_requests_total", map[string]string{
		"path": "/healthz", "method": "GET", "code": "200",
	})
	require.True(t, ok, "expected buddy_http_requests_total labeled with the route pattern /healthz")
	require.Equal(t, 1.0, metric.GetCounter().GetValue())
}

func TestWithMetrics_UnmatchedPath_NeverRecordedAsRawPath(t *testing.T) {
	t.Parallel()

	s, reg := newTestServer(t, Config{})

	rawPath := "/this/path/does/not/exist/12345"
	rec := doRequest(s, http.MethodGet, rawPath, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// The raw path must never appear as a "path" label value anywhere in
	// buddy_http_requests_total -- that would be exactly the unbounded
	// cardinality bug this middleware exists to prevent.
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, fam := range families {
		if fam.GetName() != "buddy_http_requests_total" {
			continue
		}
		for _, metric := range fam.GetMetric() {
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == "path" {
					require.NotEqual(t, rawPath, lp.GetValue())
				}
			}
		}
	}

	// Instead, the unmatched request must be recorded under the stable
	// placeholder series.
	metric, ok := findSeries(t, reg, "buddy_http_requests_total", map[string]string{
		"path": unmatchedRoutePattern, "method": "GET", "code": "404",
	})
	require.True(t, ok, "expected the unmatched request to be recorded under the placeholder series")
	require.Equal(t, 1.0, metric.GetCounter().GetValue())
}
