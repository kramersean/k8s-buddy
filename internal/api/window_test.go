package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sean-kramer/k8s-buddy/internal/mood"
)

// -- the window itself ------------------------------------------------------

func TestWorkWindow_Empty_ReturnsZerosNotNaN(t *testing.T) {
	t.Parallel()

	var w workWindow
	errorRate, p95, n := w.stats()

	require.Equal(t, 0, n)
	require.Equal(t, 0.0, errorRate, "an empty window must report 0, not a 0/0 NaN")
	require.False(t, isNaN(errorRate), "0/0 must never reach the caller as NaN")
	require.Equal(t, time.Duration(0), p95)
}

func isNaN(f float64) bool { return f != f }

func TestWorkWindow_ErrorRateAndP95(t *testing.T) {
	t.Parallel()

	var w workWindow
	// 10 observations: 1..10 ms, two of which failed.
	for i := 1; i <= 10; i++ {
		w.observe(time.Duration(i)*time.Millisecond, i <= 2)
	}

	errorRate, p95, n := w.stats()
	require.Equal(t, 10, n)
	require.InDelta(t, 0.2, errorRate, 1e-9)
	// Nearest-rank p95 over 10 sorted samples is ceil(0.95*10)=10th, i.e.
	// index 9 -- the 10ms observation.
	require.Equal(t, 10*time.Millisecond, p95)
}

func TestWorkWindow_IsBounded_AndEvictsOldest(t *testing.T) {
	t.Parallel()

	var w workWindow

	// Fill the window with failures, then overwrite it entirely with
	// successes. A bounded ring must end up reporting 0% errors; an
	// unbounded accumulator would still be dragging the old failures along.
	for range workWindowSize {
		w.observe(500*time.Millisecond, true)
	}
	errorRate, p95, n := w.stats()
	require.Equal(t, workWindowSize, n)
	require.Equal(t, 1.0, errorRate)
	require.Equal(t, 500*time.Millisecond, p95)

	for range workWindowSize * 3 {
		w.observe(time.Millisecond, false)
	}

	errorRate, p95, n = w.stats()
	require.Equal(t, workWindowSize, n, "the window must never grow past its fixed size")
	require.Equal(t, 0.0, errorRate, "evicted failures must stop counting")
	require.Equal(t, time.Millisecond, p95)
}

// TestWorkWindow_ConcurrentObserveAndStats exercises the mutex the way
// /work and /status actually hit it: many writers and many readers at
// once. The race detector is what would prove the absence of a data race,
// and it is not available on this box (no cgo), so this test asserts the
// property logically instead -- every concurrently-observed snapshot must
// be internally consistent (an error rate in [0,1] computed against a
// sample count that matches, never a torn read of a half-updated buffer),
// and the final state must reflect exactly a full window.
func TestWorkWindow_ConcurrentObserveAndStats(t *testing.T) {
	t.Parallel()

	const (
		writers           = 8
		readers           = 8
		opsPerWriter      = 250
		opsPerReaderCheck = 250
	)

	var (
		w  workWindow
		wg sync.WaitGroup
	)

	wg.Add(writers)
	for i := range writers {
		go func(i int) {
			defer wg.Done()
			for j := range opsPerWriter {
				w.observe(time.Duration(i+j)*time.Millisecond, (i+j)%3 == 0)
			}
		}(i)
	}

	errs := make(chan string, readers*opsPerReaderCheck)
	wg.Add(readers)
	for range readers {
		go func() {
			defer wg.Done()
			for range opsPerReaderCheck {
				errorRate, p95, n := w.stats()
				switch {
				case n < 0 || n > workWindowSize:
					errs <- "sample count outside [0, workWindowSize]"
				case errorRate < 0 || errorRate > 1:
					errs <- "error rate outside [0,1]"
				case p95 < 0:
					errs <- "negative p95"
				case n == 0 && (errorRate != 0 || p95 != 0):
					errs <- "non-zero stats from an empty window"
				}
			}
		}()
	}

	wg.Wait()
	close(errs)

	for msg := range errs {
		t.Fatalf("concurrent observe/stats produced an inconsistent snapshot: %s", msg)
	}

	_, _, n := w.stats()
	require.Equal(t, workWindowSize, n,
		"after %d concurrent observations the window must be exactly full", writers*opsPerWriter)
}

// -- the window driving /status --------------------------------------------

// TestStatus_EmptyWindow_ReportsHealthy pins the "no data means healthy"
// contract: a pod that has just started and served no /work must report a
// perfect score, not a NaN, a zero, or a divide-by-zero panic.
func TestStatus_EmptyWindow_ReportsHealthy(t *testing.T) {
	t.Parallel()

	s, _ := newTestServer(t, Config{LatencyBudget: 150 * time.Millisecond})
	s.SetReady(true)

	rec := doRequest(s, http.MethodGet, "/status", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var report mood.Report
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&report))

	require.Equal(t, 100.0, report.HealthScore)
	require.Equal(t, mood.MoodLeafy, report.Mood)
}

// TestStatus_WorkObservationsDriveTheMoodDownTheLadder is the regression
// guard for the defect this window exists to fix: before it, /status
// always answered leafy/100 because nothing in the HTTP layer ever fed
// ErrorRate or P95Latency into mood.Signals, leaving five of the six moods
// unreachable in the shipped binary. Each case below feeds a window of
// real observations and asserts the mood the ladder should produce, so the
// engine is verifiably live end to end rather than merely well-tested in
// isolation.
func TestStatus_WorkObservationsDriveTheMoodDownTheLadder(t *testing.T) {
	t.Parallel()

	const budget = 100 * time.Millisecond

	cases := []struct {
		name      string
		ready     bool
		errorRate float64 // fraction of the 100 observations that failed
		latency   time.Duration
		want      mood.Mood
	}{
		{"fast and clean is leafy", true, 0, 0, mood.MoodLeafy},
		{"half-budget latency is sprouting", true, 0, budget / 2, mood.MoodSprouting},
		{"at-budget latency is thirsty", true, 0, budget, mood.MoodThirsty},
		{"at-budget latency plus 40% errors loses a leaf", true, 0.4, budget, mood.MoodLostALeaf},
		{"at-budget latency plus 80% errors is not too hot", true, 0.8, budget, mood.MoodNotTooHot},
		{"unready and failing everything is wilting", false, 1.0, budget, mood.MoodWilting},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s, _ := newTestServer(t, Config{LatencyBudget: budget})
			s.SetReady(tc.ready)

			failures := int(tc.errorRate * float64(workWindowSize))
			for i := range workWindowSize {
				s.work.observe(tc.latency, i < failures)
			}

			rec := doRequest(s, http.MethodGet, "/status", nil)
			var report mood.Report
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&report))

			require.Equalf(t, tc.want, report.Mood,
				"score %.1f (ready=%t, errorRate=%.2f, p95=%s, budget=%s)",
				report.HealthScore, tc.ready, tc.errorRate, tc.latency, budget)
			require.Equal(t, mood.FromScore(report.HealthScore), report.Mood)
		})
	}
}

// TestStatus_RealWorkRequestsMoveTheMood closes the loop through the
// actual HTTP handlers -- no direct poking at s.work. Real GET /work
// requests, then a real GET /status, and the mood must have moved off
// leafy because those requests were genuinely slow relative to the budget.
func TestStatus_RealWorkRequestsMoveTheMood(t *testing.T) {
	t.Parallel()

	// Every /work request sleeps exactly 20ms against a 10ms budget, so
	// the p95 lands at twice the budget and the latency component of the
	// score must bottom out at 0.
	s, _ := newTestServer(t, Config{
		WorkErrorRate: 0,
		LatencyBudget: 10 * time.Millisecond,
		WorkMinDelay:  20 * time.Millisecond,
		WorkMaxDelay:  20 * time.Millisecond,
	})
	s.SetReady(true)

	before := doRequest(s, http.MethodGet, "/status", nil)
	var beforeReport mood.Report
	require.NoError(t, json.NewDecoder(before.Body).Decode(&beforeReport))
	require.Equal(t, mood.MoodLeafy, beforeReport.Mood, "a pod that has served no work starts leafy")

	for range 5 {
		rec := doRequest(s, http.MethodGet, "/work", nil)
		require.Equal(t, http.StatusOK, rec.Code)
	}

	after := doRequest(s, http.MethodGet, "/status", nil)
	var afterReport mood.Report
	require.NoError(t, json.NewDecoder(after.Body).Decode(&afterReport))

	require.Less(t, afterReport.HealthScore, beforeReport.HealthScore,
		"real /work traffic must actually move the health score -- this is the whole point of the window")
	require.Equal(t, mood.MoodThirsty, afterReport.Mood,
		"50 (no errors) + 0 (latency at 2x budget) + 20 (ready) = 70, which is thirsty")
}
