package telemetry_test

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/sean-kramer/k8s-buddy/internal/mood"
	"github.com/sean-kramer/k8s-buddy/internal/telemetry"
)

// testBuildInfo is a fixed BuildInfo used across tests so expected label
// values are easy to read at each call site.
var testBuildInfo = telemetry.BuildInfo{
	Version:   "v1.2.3",
	Commit:    "deadbeef",
	GoVersion: "go1.26.5",
}

func newTestMetrics(t *testing.T) (*telemetry.Metrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := telemetry.NewMetrics(reg, testBuildInfo)
	return m, reg
}

// findMetric gathers reg and returns the dto.Metric within family that
// carries exactly the given labels, so tests can assert on a specific
// series of a vector metric without needing access to telemetry's
// unexported collector fields.
func findMetric(t *testing.T, reg *prometheus.Registry, family string, labels map[string]string) *dto.Metric {
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
			if labelsEqual(got, labels) {
				return metric
			}
		}
	}

	t.Fatalf("no series found for family %q with labels %v", family, labels)
	return nil
}

func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestNewMetrics_HealthScoreExposition(t *testing.T) {
	t.Parallel()
	m, reg := newTestMetrics(t)

	m.SetHealth(87.5, mood.MoodSprouting, true)

	expected := `
# HELP buddy_health_score Current plant health, 0-100, where 100 is fully healthy.
# TYPE buddy_health_score gauge
buddy_health_score 87.5
`
	require.NoError(t, testutil.CollectAndCompare(reg, strings.NewReader(expected), "buddy_health_score"))
}

func TestNewMetrics_ReadyExposition(t *testing.T) {
	t.Parallel()

	t.Run("ready true", func(t *testing.T) {
		t.Parallel()
		m, reg := newTestMetrics(t)
		m.SetHealth(100, mood.MoodLeafy, true)

		expected := `
# HELP buddy_ready 1 when the most recent readiness check passed, 0 otherwise.
# TYPE buddy_ready gauge
buddy_ready 1
`
		require.NoError(t, testutil.CollectAndCompare(reg, strings.NewReader(expected), "buddy_ready"))
	})

	t.Run("ready false", func(t *testing.T) {
		t.Parallel()
		m, reg := newTestMetrics(t)
		m.SetHealth(10, mood.MoodWilting, false)

		expected := `
# HELP buddy_ready 1 when the most recent readiness check passed, 0 otherwise.
# TYPE buddy_ready gauge
buddy_ready 0
`
		require.NoError(t, testutil.CollectAndCompare(reg, strings.NewReader(expected), "buddy_ready"))
	})
}

func TestSetHealth_MoodZeroing(t *testing.T) {
	t.Parallel()

	m, reg := newTestMetrics(t)
	active := mood.MoodThirsty
	m.SetHealth(65, active, true)

	activeCount := 0
	for _, candidate := range mood.AllMoods() {
		metric := findMetric(t, reg, "buddy_mood", map[string]string{"mood": string(candidate)})
		require.NotNil(t, metric.Gauge, "buddy_mood series for %q must be a gauge", candidate)

		if candidate == active {
			require.Equalf(t, 1.0, metric.GetGauge().GetValue(), "active mood %q should be 1", candidate)
			activeCount++
		} else {
			require.Equalf(t, 0.0, metric.GetGauge().GetValue(), "inactive mood %q should be 0", candidate)
		}
	}

	require.Equal(t, 1, activeCount, "exactly one mood series must be 1")
}

func TestSetHealth_MoodZeroing_PreviousMoodGoesStale(t *testing.T) {
	t.Parallel()

	// Regression guard for the exact bug the doc comment on SetHealth
	// warns about: switching the active mood must flip the previously
	// active series back to 0, not leave it stuck at 1.
	m, reg := newTestMetrics(t)

	m.SetHealth(100, mood.MoodLeafy, true)
	leafy := findMetric(t, reg, "buddy_mood", map[string]string{"mood": string(mood.MoodLeafy)})
	require.Equal(t, 1.0, leafy.GetGauge().GetValue())

	m.SetHealth(10, mood.MoodWilting, false)

	leafyAfter := findMetric(t, reg, "buddy_mood", map[string]string{"mood": string(mood.MoodLeafy)})
	require.Equal(t, 0.0, leafyAfter.GetGauge().GetValue(), "previously active mood must drop to 0")

	wilting := findMetric(t, reg, "buddy_mood", map[string]string{"mood": string(mood.MoodWilting)})
	require.Equal(t, 1.0, wilting.GetGauge().GetValue())
}

func TestNewMetrics_BuildInfo(t *testing.T) {
	t.Parallel()
	_, reg := newTestMetrics(t)

	metric := findMetric(t, reg, "buddy_build_info", map[string]string{
		"version":    testBuildInfo.Version,
		"commit":     testBuildInfo.Commit,
		"go_version": testBuildInfo.GoVersion,
	})

	require.NotNil(t, metric.Gauge)
	require.Equal(t, 1.0, metric.GetGauge().GetValue())
}

// TestNewMetrics_WorkSeriesPreInitializedToZero pins the fix for a
// staleness bug: a Prometheus *Vec exports nothing for a label combination
// it has never been handed, so before this pre-initialization a pod that
// had not yet served a /work request exported no buddy_work_requests_total
// series at all -- an alert on the failure rate would evaluate against "no
// data" instead of a truthful 0.
func TestNewMetrics_WorkSeriesPreInitializedToZero(t *testing.T) {
	t.Parallel()

	_, reg := newTestMetrics(t)

	// Deliberately no ObserveWork call anywhere in this test.
	for _, outcome := range telemetry.Outcomes() {
		counter := findMetric(t, reg, "buddy_work_requests_total", map[string]string{"outcome": outcome})
		require.NotNil(t, counter.Counter, "buddy_work_requests_total{outcome=%q} must exist before any observation", outcome)
		require.Equalf(t, 0.0, counter.GetCounter().GetValue(), "outcome %q must start at 0, not absent", outcome)

		histogram := findMetric(t, reg, "buddy_work_duration_seconds", map[string]string{"outcome": outcome})
		require.NotNil(t, histogram.Histogram, "buddy_work_duration_seconds{outcome=%q} must exist before any observation", outcome)
		require.EqualValuesf(t, 0, histogram.GetHistogram().GetSampleCount(), "outcome %q must start with 0 samples", outcome)
	}
}

// TestOutcomes_MatchesTheDocumentedVocabulary guards the single source of
// truth: these three strings are simultaneously the "outcome" metric label
// values and /work's response body values, so a change here is a breaking
// change to both contracts at once.
func TestOutcomes_MatchesTheDocumentedVocabulary(t *testing.T) {
	t.Parallel()

	require.Equal(t,
		[]string{"success", "warning", "failure"},
		telemetry.Outcomes(),
	)
}

func TestObserveWork_RecordsCounterAndHistogram(t *testing.T) {
	t.Parallel()
	m, reg := newTestMetrics(t)

	m.ObserveWork("success", 15*time.Millisecond)
	m.ObserveWork("success", 30*time.Millisecond)
	m.ObserveWork("failure", 5*time.Millisecond)

	successCounter := findMetric(t, reg, "buddy_work_requests_total", map[string]string{"outcome": "success"})
	require.Equal(t, 2.0, successCounter.GetCounter().GetValue())

	failureCounter := findMetric(t, reg, "buddy_work_requests_total", map[string]string{"outcome": "failure"})
	require.Equal(t, 1.0, failureCounter.GetCounter().GetValue())

	successHistogram := findMetric(t, reg, "buddy_work_duration_seconds", map[string]string{"outcome": "success"})
	require.EqualValues(t, 2, successHistogram.GetHistogram().GetSampleCount())

	failureHistogram := findMetric(t, reg, "buddy_work_duration_seconds", map[string]string{"outcome": "failure"})
	require.EqualValues(t, 1, failureHistogram.GetHistogram().GetSampleCount())
}

func TestObserveHTTP_RecordsCodeAsString(t *testing.T) {
	t.Parallel()
	m, reg := newTestMetrics(t)

	m.ObserveHTTP("/status", "GET", 200)
	m.ObserveHTTP("/status", "GET", 200)
	m.ObserveHTTP("/work", "GET", 500)

	statusOK := findMetric(t, reg, "buddy_http_requests_total", map[string]string{
		"path": "/status", "method": "GET", "code": "200",
	})
	require.Equal(t, 2.0, statusOK.GetCounter().GetValue())

	workError := findMetric(t, reg, "buddy_http_requests_total", map[string]string{
		"path": "/work", "method": "GET", "code": "500",
	})
	require.Equal(t, 1.0, workError.GetCounter().GetValue())
}

func TestNewMetrics_IndependentRegistries(t *testing.T) {
	t.Parallel()

	// Regression guard: NewMetrics must never touch a shared/global
	// registry. Two independent registries each getting their own
	// Metrics instance, both registering the identical metric names,
	// must not collide or panic.
	reg1 := prometheus.NewRegistry()
	reg2 := prometheus.NewRegistry()

	require.NotPanics(t, func() {
		telemetry.NewMetrics(reg1, testBuildInfo)
	})
	require.NotPanics(t, func() {
		telemetry.NewMetrics(reg2, testBuildInfo)
	})

	families1, err := reg1.Gather()
	require.NoError(t, err)
	families2, err := reg2.Gather()
	require.NoError(t, err)
	require.NotEmpty(t, families1)
	require.NotEmpty(t, families2)
}

func TestNewMetrics_DuplicateRegistrationPanics(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	telemetry.NewMetrics(reg, testBuildInfo)

	require.Panics(t, func() {
		telemetry.NewMetrics(reg, testBuildInfo)
	}, "registering the same metrics twice against one registry must panic, not fail silently")
}
