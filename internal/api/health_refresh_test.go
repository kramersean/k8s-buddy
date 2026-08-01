package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kramersean/k8s-buddy/internal/mood"
)

// TestRunHealthRefresher_PopulatesHealthGaugesWithoutStatusCall is the
// direct regression test for the bug this file exists to close:
// buddy_health_score, buddy_mood, and buddy_ready used to be set ONLY by
// statusHandler (and SetReady), so a perfectly healthy pod that nothing had
// ever curled /status on reported health 0, ready 0, and no buddy_mood
// series at all -- indistinguishable from a dead plant to Prometheus, which
// only ever scrapes /metrics, never calls /status itself.
//
// This test deliberately never calls doRequest(s, "GET", "/status", ...)
// and never calls s.SetReady (which would itself push a value via
// syncMetrics, muddying what's actually being proven). The server starts
// ready via New() alone; RunHealthRefresher, running on a short interval
// against a fresh registry, is the ONLY thing that ever touches these three
// gauges in this test.
func TestRunHealthRefresher_PopulatesHealthGaugesWithoutStatusCall(t *testing.T) {
	t.Parallel()

	s, reg := newTestServer(t, Config{})

	// Confirm the bug's own precondition first: before the refresher has
	// ever ticked, buddy_health_score has never been Set (New does not call
	// syncMetrics), and buddy_mood has no series at all (telemetry's mood
	// gauges are lazily created inside SetHealth, never pre-initialized the
	// way the outcome-labelled counters are). If either of these ever stops
	// being true, this test's own premise is gone.
	_, ok := findSeries(t, reg, "buddy_mood", map[string]string{"mood": "leafy"})
	require.False(t, ok, "buddy_mood must not exist before anything has called SetHealth")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = s.RunHealthRefresher(ctx, 5*time.Millisecond)
	}()

	require.Eventually(t, func() bool {
		metric, ok := findSeries(t, reg, "buddy_health_score", nil)
		return ok && metric.GetGauge().GetValue() == 100
	}, time.Second, 5*time.Millisecond,
		"buddy_health_score never reached 100 (a freshly-started, ready, error-free server's "+
			"correct score) without any /status call -- the refresher is not populating it")

	readyMetric, ok := findSeries(t, reg, "buddy_ready", nil)
	require.True(t, ok, "buddy_ready must be set by the refresher alone")
	require.Equal(t, 1.0, readyMetric.GetGauge().GetValue())

	// Exactly one buddy_mood series active (leafy, score 100), every other
	// mood explicitly zero -- proving SetHealth's full zeroing pass ran via
	// the refresher, not a partial write.
	for _, m := range mood.AllMoods() {
		series, ok := findSeries(t, reg, "buddy_mood", map[string]string{"mood": string(m)})
		require.True(t, ok, "buddy_mood{mood=%q} must exist", m)
		want := 0.0
		if m == mood.MoodLeafy {
			want = 1.0
		}
		require.Equal(t, want, series.GetGauge().GetValue(), "buddy_mood{mood=%q}", m)
	}
}

// TestRunHealthRefresher_StopsOnContextCancel asserts the refresher goroutine
// actually stops (no leak) when its context is cancelled, and -- more
// specifically than "it returned" -- that it performs NO FURTHER WRITES to
// the gauges after that point, even though the underlying signals it would
// otherwise pick up on its next tick have changed.
func TestRunHealthRefresher_StopsOnContextCancel(t *testing.T) {
	t.Parallel()

	s, reg := newTestServer(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- s.RunHealthRefresher(ctx, 5*time.Millisecond) }()

	// Wait for at least one refresh to have landed before cancelling, so
	// the test isn't racing the very first tick.
	require.Eventually(t, func() bool {
		_, ok := findSeries(t, reg, "buddy_health_score", nil)
		return ok
	}, time.Second, 5*time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "RunHealthRefresher must return nil, not an error, on context cancellation")
	case <-time.After(time.Second):
		t.Fatal("RunHealthRefresher did not return within 1s of its context being cancelled -- goroutine leak")
	}

	before, ok := findSeries(t, reg, "buddy_health_score", nil)
	require.True(t, ok)
	beforeValue := before.GetGauge().GetValue()

	// Mutate the signal the refresher would read on its next tick, had it
	// still been running: 50 recorded failures would tank the score from
	// its current (healthy) value.
	for range 50 {
		s.work.observe(0, true)
	}

	// Several ticks' worth of time, so a still-running goroutine would have
	// had every opportunity to observe the change and write it.
	time.Sleep(50 * time.Millisecond)

	after, ok := findSeries(t, reg, "buddy_health_score", nil)
	require.True(t, ok)
	require.Equal(t, beforeValue, after.GetGauge().GetValue(),
		"buddy_health_score changed after the refresher's context was cancelled -- it kept writing")
}

// TestRunHealthRefresher_AgreesWithStatusHandler asserts the gauge the
// refresher publishes and the JSON /status itself returns can never
// disagree, because both are derived from the exact same currentReport call
// -- there is one place in this package that turns this Server's state into
// a mood.Report, never two independently-maintained ones.
func TestRunHealthRefresher_AgreesWithStatusHandler(t *testing.T) {
	t.Parallel()

	s, reg := newTestServer(t, Config{LatencyBudget: 250 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = s.RunHealthRefresher(ctx, 5*time.Millisecond) }()

	require.Eventually(t, func() bool {
		_, ok := findSeries(t, reg, "buddy_health_score", nil)
		return ok
	}, time.Second, 5*time.Millisecond)

	rec := doRequest(s, http.MethodGet, "/status", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var report mood.Report
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&report))

	metric, ok := findSeries(t, reg, "buddy_health_score", nil)
	require.True(t, ok)
	require.InDelta(t, report.HealthScore, metric.GetGauge().GetValue(), 0.001)

	moodMetric, ok := findSeries(t, reg, "buddy_mood", map[string]string{"mood": string(report.Mood)})
	require.True(t, ok)
	require.Equal(t, 1.0, moodMetric.GetGauge().GetValue())
}

// TestRunHealthRefresher_RejectsNonPositiveInterval asserts a non-positive
// interval is a config error RunHealthRefresher itself refuses to run
// with, rather than a busy loop (a <=0 time.Ticker panics) or a silently
// disabled refresher (returning nil without ever ticking).
func TestRunHealthRefresher_RejectsNonPositiveInterval(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, Config{})

	require.Error(t, s.RunHealthRefresher(context.Background(), 0))
	require.Error(t, s.RunHealthRefresher(context.Background(), -time.Second))
}
