package mood

// This file holds the tests that need access to the unexported `now`
// indirection (to stub the clock deterministically). Everything else lives
// in the black-box mood_test.go, which only exercises the exported surface;
// this file is kept as small as possible so most of the package's tests
// still prove they work through the public API the way Tasks 3/4 will use
// it.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewReport_DerivesConsistentMoodMessageAndScore(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		s    Signals
	}{
		{
			name: "leafy",
			s: Signals{
				Ready:         true,
				ErrorRate:     0,
				P95Latency:    0,
				LatencyBudget: 100 * time.Millisecond,
				RestartCount:  0,
			},
		},
		{
			name: "thirsty",
			s: Signals{
				Ready:         true,
				ErrorRate:     0,
				P95Latency:    time.Second, // 10x the budget
				LatencyBudget: 100 * time.Millisecond,
				RestartCount:  0,
			},
		},
		{
			name: "not-ready, capped below not-too-hot's floor",
			s: Signals{
				Ready:         false,
				ErrorRate:     0,
				P95Latency:    0,
				LatencyBudget: 100 * time.Millisecond,
				RestartCount:  0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wantScore := tt.s.Score()
			wantMood := FromScore(wantScore)
			wantMessage := wantMood.Message()

			r := NewReport(tt.s, "ficus", "Ficus lyrata", time.Hour)

			require.InDelta(t, wantScore, r.HealthScore, 0.0001)
			require.Equal(t, wantMood, r.Mood)
			require.Equal(t, wantMessage, r.Message)
			require.NotEmpty(t, r.Message)
		})
	}

	// Sanity-check the three cases really do span different moods; if they
	// ever collapsed onto the same mood, the test above would stop proving
	// the derivations agree across a *range* of moods.
	require.Equal(t, MoodLeafy, FromScore(tests[0].s.Score()))
	require.Equal(t, MoodThirsty, FromScore(tests[1].s.Score()))
	require.Equal(t, MoodNotTooHot, FromScore(tests[2].s.Score()))
}

func TestNewReport_ReadyMirrorsSignals(t *testing.T) {
	t.Parallel()

	ready := Signals{Ready: true, LatencyBudget: time.Second}
	notReady := Signals{Ready: false, LatencyBudget: time.Second}

	require.True(t, NewReport(ready, "buddy", "Pothos", 0).Ready)
	require.False(t, NewReport(notReady, "buddy", "Pothos", 0).Ready)
}

func TestNewReport_UptimeFormatsWithDurationString(t *testing.T) {
	t.Parallel()

	d := time.Hour + 2*time.Minute + 3*time.Second
	r := NewReport(Signals{Ready: true, LatencyBudget: time.Second}, "buddy", "Pothos", d)

	require.Equal(t, "1h2m3s", r.Uptime)
	require.Equal(t, d.String(), r.Uptime)
}

func TestNewReport_CheckedAtUsesStubbedClock(t *testing.T) {
	// Deliberately not t.Parallel(): this test mutates the package-level
	// `now` var and must fully run (including the deferred restore) before
	// any other test that might rely on the real clock.
	original := now
	defer func() { now = original }()

	stub := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return stub }

	r := NewReport(Signals{Ready: true, LatencyBudget: time.Second}, "buddy", "Pothos", time.Minute)

	require.True(t, r.CheckedAt.Equal(stub))
	require.Equal(t, stub, r.CheckedAt)
}

func TestReport_JSONTagsMatchDocumentedKeys(t *testing.T) {
	t.Parallel()

	r := Report{
		Mood:        MoodLeafy,
		Message:     "I'm feeling leafy and stable",
		HealthScore: 97.5,
		Ready:       true,
		Species:     "Ficus lyrata",
		Name:        "buddy",
		Uptime:      "1h2m3s",
		CheckedAt:   time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(r)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &raw))

	wantKeys := []string{
		"mood", "message", "healthScore", "ready", "species", "name", "uptime", "checkedAt",
	}
	require.Len(t, raw, len(wantKeys), "Report must serialize to exactly these keys, nothing added or missing")
	for _, k := range wantKeys {
		require.Contains(t, raw, k)
	}
}
