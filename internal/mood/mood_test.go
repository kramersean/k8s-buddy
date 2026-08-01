package mood_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/kramersean/k8s-buddy/internal/mood"
)

func TestFromScore_Boundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		score float64
		want  mood.Mood
	}{
		{"100 is leafy", 100, mood.MoodLeafy},
		{"95 is leafy (inclusive lower edge)", 95, mood.MoodLeafy},
		{"94.9 is sprouting", 94.9, mood.MoodSprouting},
		{"80 is sprouting (inclusive lower edge)", 80, mood.MoodSprouting},
		{"79.9 is thirsty", 79.9, mood.MoodThirsty},
		{"60 is thirsty (inclusive lower edge)", 60, mood.MoodThirsty},
		{"59.9 is lost-a-leaf", 59.9, mood.MoodLostALeaf},
		{"40 is lost-a-leaf (inclusive lower edge)", 40, mood.MoodLostALeaf},
		{"39.9 is not-too-hot", 39.9, mood.MoodNotTooHot},
		{"20 is not-too-hot (inclusive lower edge)", 20, mood.MoodNotTooHot},
		{"19.9 is wilting", 19.9, mood.MoodWilting},
		{"0 is wilting", 0, mood.MoodWilting},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, mood.FromScore(tt.score))
		})
	}
}

func TestScore_PerfectSignalsIsOneHundred(t *testing.T) {
	t.Parallel()

	s := mood.Signals{
		Ready:         true,
		ErrorRate:     0,
		P95Latency:    0,
		LatencyBudget: 100 * time.Millisecond,
		RestartCount:  0,
	}
	require.InDelta(t, 100.0, s.Score(), 0.0001)
}

func TestScore_ClampsAboveOneHundred(t *testing.T) {
	t.Parallel()

	// A negative ErrorRate pushes the error component above its 50-point
	// max; the final result must still clamp to 100, not overshoot.
	s := mood.Signals{
		Ready:         true,
		ErrorRate:     -1,
		P95Latency:    0,
		LatencyBudget: 100 * time.Millisecond,
		RestartCount:  0,
	}
	require.InDelta(t, 100.0, s.Score(), 0.0001)
}

func TestScore_ClampsBelowZero(t *testing.T) {
	t.Parallel()

	// An ErrorRate above 1 drives the error component deeply negative; the
	// final result must clamp to 0, not go negative.
	s := mood.Signals{
		Ready:         false,
		ErrorRate:     3,
		P95Latency:    time.Second,
		LatencyBudget: 100 * time.Millisecond,
		RestartCount:  0,
	}
	require.InDelta(t, 0.0, s.Score(), 0.0001)
}

func TestScore_NotReadyCeiling(t *testing.T) {
	t.Parallel()

	// Perfect latency, zero errors, no restarts, but not ready: the ceiling
	// caps the score at exactly 35, applied after summing the components
	// (50 error + 30 latency + 0 readiness = 80, capped to 35).
	s := mood.Signals{
		Ready:         false,
		ErrorRate:     0,
		P95Latency:    0,
		LatencyBudget: 100 * time.Millisecond,
		RestartCount:  0,
	}
	require.InDelta(t, 35.0, s.Score(), 0.0001)
}

func TestScore_NotReadyCeilingDoesNotRaiseAnAlreadyLowerScore(t *testing.T) {
	t.Parallel()

	// A not-ready plant whose other signals already sum below 35 must stay
	// at that lower value; the ceiling is a cap, not a floor.
	s := mood.Signals{
		Ready:         false,
		ErrorRate:     1, // error component = 0
		P95Latency:    time.Second,
		LatencyBudget: 100 * time.Millisecond, // latency component = 0
		RestartCount:  0,
	}
	require.InDelta(t, 0.0, s.Score(), 0.0001)
}

func TestScore_ZeroLatencyBudgetAwardsFullLatencyPoints(t *testing.T) {
	t.Parallel()

	s := mood.Signals{
		Ready:         true,
		ErrorRate:     0,
		P95Latency:    time.Hour, // would be far beyond any real budget
		LatencyBudget: 0,
		RestartCount:  0,
	}
	require.InDelta(t, 100.0, s.Score(), 0.0001)
}

func TestScore_NegativeLatencyBudgetAwardsFullLatencyPoints(t *testing.T) {
	t.Parallel()

	s := mood.Signals{
		Ready:         true,
		ErrorRate:     0,
		P95Latency:    time.Hour,
		LatencyBudget: -1 * time.Second,
		RestartCount:  0,
	}
	require.InDelta(t, 100.0, s.Score(), 0.0001)
}

func TestScore_LatencyFarBeyondBudgetFloorsAtZero(t *testing.T) {
	t.Parallel()

	s := mood.Signals{
		Ready:         true,
		ErrorRate:     0,
		P95Latency:    1 * time.Second, // 10x the budget
		LatencyBudget: 100 * time.Millisecond,
		RestartCount:  0,
	}
	// error (50) + latency (0, floored) + readiness (20) - restart (0) = 70.
	require.InDelta(t, 70.0, s.Score(), 0.0001)
}

func TestScore_RestartPenalty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		restartCount int
		want         float64
	}{
		{"one restart deducts two points", 1, 98},
		{"three restarts deduct six points", 3, 94},
		{"five restarts hit the ten point cap", 5, 90},
		{"far more restarts stay capped at ten points", 50, 90},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := mood.Signals{
				Ready:         true,
				ErrorRate:     0,
				P95Latency:    0,
				LatencyBudget: 100 * time.Millisecond,
				RestartCount:  tt.restartCount,
			}
			require.InDelta(t, tt.want, s.Score(), 0.0001)
		})
	}
}

func TestScore_HalfwayLatencyIsHalfPoints(t *testing.T) {
	t.Parallel()

	s := mood.Signals{
		Ready:         true,
		ErrorRate:     0,
		P95Latency:    50 * time.Millisecond,
		LatencyBudget: 100 * time.Millisecond,
		RestartCount:  0,
	}
	// error (50) + latency (15, half of 30) + readiness (20) - restart (0) = 85.
	require.InDelta(t, 85.0, s.Score(), 0.0001)
}

func TestAllMoods_HealthiestFirst(t *testing.T) {
	t.Parallel()

	want := []mood.Mood{
		mood.MoodLeafy,
		mood.MoodSprouting,
		mood.MoodThirsty,
		mood.MoodLostALeaf,
		mood.MoodNotTooHot,
		mood.MoodWilting,
	}
	require.Equal(t, want, mood.AllMoods())
}

func TestAllMoods_ReturnsAFreshSliceEachCall(t *testing.T) {
	t.Parallel()

	first := mood.AllMoods()
	first[0] = "tampered"

	second := mood.AllMoods()
	require.Equal(t, mood.MoodLeafy, second[0], "mutating a previously returned slice must not affect later calls")
	require.NotEqual(t, "tampered", second[0])
}

func TestMessage_ExactStrings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		m    mood.Mood
		want string
	}{
		{mood.MoodLeafy, "I'm feeling leafy and stable"},
		{mood.MoodSprouting, "I'm ready to rock and roll"},
		{mood.MoodThirsty, "Could use a drink, but I'm managing."},
		{mood.MoodLostALeaf, "Lost a leaf, but I'm recovering."},
		{mood.MoodNotTooHot, "I'm not feeling too hot."},
		{mood.MoodWilting, "I'm wilting. Send help."},
	}

	for _, tt := range tests {
		t.Run(string(tt.m), func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, tt.m.Message())
		})
	}
}

func TestMessage_AllMoodsHaveNonEmptyDistinctMessages(t *testing.T) {
	t.Parallel()

	seen := make(map[string]mood.Mood)
	for _, m := range mood.AllMoods() {
		msg := m.Message()
		require.NotEmpty(t, msg, "mood %q must have a non-empty message", m)

		if owner, ok := seen[msg]; ok {
			t.Fatalf("message %q is shared by both %q and %q, but all messages must be distinct", msg, owner, m)
		}
		seen[msg] = m
	}
	require.Len(t, seen, len(mood.AllMoods()))
}

func TestMessage_UnknownMoodHasNonEmptyFallback(t *testing.T) {
	t.Parallel()

	var zero mood.Mood
	require.NotEmpty(t, zero.Message())

	unknown := mood.Mood("some-mood-that-does-not-exist")
	require.NotEmpty(t, unknown.Message())
	require.Equal(t, zero.Message(), unknown.Message(), "the fallback message should be consistent across unknown moods")
}
