// Package mood turns raw health signals into the plant's mood.
//
// The package is intentionally dependency-free: no Kubernetes, no HTTP, no
// I/O. It exists so that the health-and-mood rules can be read, tested, and
// reasoned about in isolation from everything that calls into it.
package mood

import "time"

// Mood is the plant's emotional state, derived from a health score. It is a
// small closed set of string constants rather than an int enum so that it
// serializes directly to a stable, human-readable JSON value and Prometheus
// label.
type Mood string

// The complete set of moods, ordered from healthiest to least healthy. See
// AllMoods for the same ordering as a slice, and FromScore for how a health
// score maps onto one of these values.
const (
	// MoodLeafy is the healthiest mood: score >= 95.
	MoodLeafy Mood = "leafy"
	// MoodSprouting is a healthy mood: 80 <= score < 95.
	MoodSprouting Mood = "sprouting"
	// MoodThirsty is a mildly degraded mood: 60 <= score < 80.
	MoodThirsty Mood = "thirsty"
	// MoodLostALeaf is a degraded mood: 40 <= score < 60.
	MoodLostALeaf Mood = "lost-a-leaf"
	// MoodNotTooHot is a poor mood: 20 <= score < 40.
	MoodNotTooHot Mood = "not-too-hot"
	// MoodWilting is the least healthy mood: score < 20.
	MoodWilting Mood = "wilting"
)

// notReadyCeiling is the highest health score a Signals value can produce
// when Ready is false. It keeps an unready plant from ever reporting itself
// as anything better than "lost-a-leaf" territory, no matter how good its
// other signals look.
const notReadyCeiling = 35.0

// scoreLatencyPoints, scoreErrorPoints, and scoreReadyPoints are the maximum
// number of points each component of Score contributes to the 100-point
// total. They must sum to 100.
const (
	scoreErrorPoints   = 50.0
	scoreLatencyPoints = 30.0
	scoreReadyPoints   = 20.0
)

// restartPenaltyPerRestart is how many points Score deducts for each
// observed restart, and restartPenaltyCap is the most that penalty can ever
// total, regardless of how many restarts occurred.
const (
	restartPenaltyPerRestart = 2.0
	restartPenaltyCap        = 10.0
)

// AllMoods lists every mood, healthiest first. Each call returns a new
// slice, so callers may freely mutate the result without affecting other
// callers or future calls.
func AllMoods() []Mood {
	return []Mood{
		MoodLeafy,
		MoodSprouting,
		MoodThirsty,
		MoodLostALeaf,
		MoodNotTooHot,
		MoodWilting,
	}
}

// Message returns the plant-themed message for a mood. Every value in
// AllMoods has a distinct, non-empty message. An unrecognized Mood
// (including the zero value "") is not a value FromScore or the constants
// above ever produce, but Message still returns a sensible non-empty
// fallback for it rather than an empty string, so callers never need to
// special-case an unknown mood before displaying it.
func (m Mood) Message() string {
	switch m {
	case MoodLeafy:
		return "I'm feeling leafy and stable"
	case MoodSprouting:
		return "I'm ready to rock and roll"
	case MoodThirsty:
		return "Could use a drink, but I'm managing."
	case MoodLostALeaf:
		return "Lost a leaf, but I'm recovering."
	case MoodNotTooHot:
		return "I'm not feeling too hot."
	case MoodWilting:
		return "I'm wilting. Send help."
	default:
		return "I'm not sure how I'm feeling right now."
	}
}

// Signals are the inputs to the health calculation. They are the small set
// of raw observations Score needs to compute a single health number; nothing
// in this package knows or cares where they came from.
type Signals struct {
	// Ready is the outcome of the most recent readiness check.
	Ready bool
	// ErrorRate is the fraction of recent requests that failed, in the
	// range 0..1.
	ErrorRate float64
	// P95Latency is the observed 95th-percentile request latency over the
	// recent window.
	P95Latency time.Duration
	// LatencyBudget is the P95Latency at or above which the latency
	// component of Score bottoms out at zero. A value <= 0 disables the
	// latency penalty entirely (Score awards full latency marks).
	LatencyBudget time.Duration
	// RestartCount is the number of restarts observed for the component
	// being scored.
	RestartCount int
}

// Score returns a health score in [0,100] derived from s. It is a weighted
// composite of four components, plus a restart penalty:
//
//   - Error rate: up to 50 points, linearly, favoring a lower ErrorRate.
//   - Latency: up to 30 points, scaled linearly down to 0 as P95Latency
//     goes from 0 to LatencyBudget. Latency at or beyond the budget
//     contributes 0, never a negative amount. A non-positive LatencyBudget
//     is treated as "no budget configured" and awards full marks.
//   - Readiness: 20 points when Ready is true, else 0.
//   - Restart penalty: 2 points subtracted per restart, capped at 10 points
//     total, regardless of RestartCount.
//
// If Ready is false, the summed score is additionally capped at 35 before
// the final clamp: an unready component can never score as anything but
// unhealthy, even with perfect latency and zero errors. The result is
// clamped to [0,100] as the last step, after every component and cap has
// been applied.
func (s Signals) Score() float64 {
	errorComponent := (1 - s.ErrorRate) * scoreErrorPoints

	latencyComponent := scoreLatencyPoints
	if s.LatencyBudget > 0 {
		ratio := float64(s.P95Latency) / float64(s.LatencyBudget)
		latencyComponent = scoreLatencyPoints * (1 - ratio)
		if latencyComponent < 0 {
			latencyComponent = 0
		}
	}

	readinessComponent := 0.0
	if s.Ready {
		readinessComponent = scoreReadyPoints
	}

	restartPenalty := float64(s.RestartCount) * restartPenaltyPerRestart
	if restartPenalty > restartPenaltyCap {
		restartPenalty = restartPenaltyCap
	}
	if restartPenalty < 0 {
		restartPenalty = 0
	}

	score := errorComponent + latencyComponent + readinessComponent - restartPenalty

	if !s.Ready && score > notReadyCeiling {
		score = notReadyCeiling
	}

	switch {
	case score > 100:
		score = 100
	case score < 0:
		score = 0
	}

	return score
}

// FromScore maps a health score to its Mood. Thresholds are inclusive at
// the lower edge: a score of exactly 95 is MoodLeafy, while 94.999 is
// MoodSprouting. Scores are not clamped here; callers that want a value
// confined to [0,100] should use Signals.Score, which already clamps.
func FromScore(score float64) Mood {
	switch {
	case score >= 95:
		return MoodLeafy
	case score >= 80:
		return MoodSprouting
	case score >= 60:
		return MoodThirsty
	case score >= 40:
		return MoodLostALeaf
	case score >= 20:
		return MoodNotTooHot
	default:
		return MoodWilting
	}
}
