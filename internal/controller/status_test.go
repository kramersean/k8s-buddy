// This test file lives in package controller (not controller_test) rather
// than resources_test.go's external package specifically so it can stub the
// unexported now indirection in status.go and assert a deterministic
// LastWatered instead of racing the real clock.
package controller

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	buddyv1alpha1 "github.com/sean-kramer/k8s-buddy/api/v1alpha1"
	"github.com/sean-kramer/k8s-buddy/internal/mood"
)

// stubNow replaces the package-level now indirection with a clock fixed at
// t for the duration of the calling test, restoring the real clock via
// t.Cleanup.
func stubNow(t *testing.T, at time.Time) {
	t.Helper()
	prev := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = prev })
}

// testPlant returns a fully-defaulted Plant, mirroring resources_test.go's
// own helper of the same name (kept package-local here since this file
// lives in package controller, not controller_test).
func testPlant(generation int64, replicas *int32) *buddyv1alpha1.Plant {
	return &buddyv1alpha1.Plant{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "fernie",
			Namespace:  "k8s-buddy",
			Generation: generation,
		},
		Spec: buddyv1alpha1.PlantSpec{
			Species:  "fern",
			Replicas: replicas,
		},
	}
}

// testDeployment returns a Deployment reporting readyReplicas ready Pods,
// fully rolled out (status.observedGeneration == metadata.generation).
func testDeployment(ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			ReadyReplicas:      ready,
		},
	}
}

func ptrInt32(v int32) *int32 { return &v }

func TestHealthPercent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ready   int32
		desired int32
		want    int32
	}{
		{"zero ready of three", 0, 3, 0},
		{"one third rounds to 33", 1, 3, 33},
		{"two thirds rounds to 67", 2, 3, 67},
		{"fully ready", 3, 3, 100},
		{"desired zero never divides by zero", 0, 0, 0},
		{"desired zero ignores nonzero ready", 5, 0, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, healthPercent(tc.ready, tc.desired))
		})
	}
}

// TestComputeStatus_MoodMatchesMoodPackage asserts the Mood computeStatus
// derives is exactly what internal/mood's own ladder produces for the
// equivalent Signals — computed independently in this test via
// mood.Signals{}.Score() and mood.FromScore, not copied from status.go's
// implementation. This is what proves status.go reuses the ladder instead
// of quietly reimplementing its thresholds.
//
// "none ready" is the one case that does NOT go through that generic
// Score-based computation, by design: moodFor special-cases ready == 0 &&
// desired > 0 to mood.MoodWilting directly, rather than deriving it from a
// score that (see moodFor's own comment) fabricates 30 free latency points
// this package has no data to justify. This test's expectation is built to
// match, so it keeps proving "computeStatus reuses the ladder" for every
// case where the ladder is actually the right tool, without silently
// regressing back to the bug TestMoodFor_ZeroReadyReportsWilting guards.
func TestComputeStatus_MoodMatchesMoodPackage(t *testing.T) {
	t.Parallel()
	stubNow(t, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))

	tests := []struct {
		name    string
		ready   int32
		desired int32
	}{
		{"none ready", 0, 3},
		{"partially ready", 1, 3},
		{"two thirds ready", 2, 3},
		{"fully ready", 3, 3},
		{"desired zero", 0, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plant := testPlant(1, ptrInt32(tc.desired))
			deployment := testDeployment(tc.ready)

			var wantMood mood.Mood
			if tc.desired > 0 && tc.ready == 0 {
				wantMood = mood.MoodWilting
			} else {
				errorRate := 0.0
				if tc.desired > 0 {
					errorRate = 1 - float64(tc.ready)/float64(tc.desired)
				}
				wantSignals := mood.Signals{Ready: tc.ready > 0, ErrorRate: errorRate}
				wantMood = mood.FromScore(wantSignals.Score())
			}

			status := computeStatus(plant, deployment)
			require.Equal(t, string(wantMood), status.Mood)
		})
	}
}

// TestMoodFor_ZeroReadyReportsWilting is the regression test for the bug
// this file's own moodFor comment documents: with LatencyBudget always left
// at its zero value, internal/mood's "no budget configured, award full
// marks" rule handed every fully-unavailable Plant 30 free points,
// permanently capping it at mood.MoodNotTooHot and making
// mood.MoodWilting -- the ladder's own most severe state --
// UNREACHABLE from this operator. Swept across several desired counts
// (1, 3, and 10) specifically because the bug's arithmetic (score lands at
// exactly 30, comfortably under the 40 "lost-a-leaf" floor and over the 20
// "not-too-hot" floor) does not depend on desired's magnitude at all --
// this is a readiness bug, not a rounding one, and the sweep proves that.
func TestMoodFor_ZeroReadyReportsWilting(t *testing.T) {
	t.Parallel()

	for _, desired := range []int32{1, 3, 10} {
		t.Run(desiredCaseName(0, desired), func(t *testing.T) {
			t.Parallel()
			require.Equal(t, mood.MoodWilting, moodFor(0, desired))
		})
	}
}

// TestMoodFor_ExistingMappingsUnaffectedByFix asserts, with literal mood
// values (not re-derived through mood.Signals/FromScore the way
// TestComputeStatus_MoodMatchesMoodPackage does), that the fix in this file
// -- special-casing ready == 0 && desired > 0 -- left every other mapping
// exactly as it was: it changes what a totally-unavailable Plant reports,
// nothing else.
func TestMoodFor_ExistingMappingsUnaffectedByFix(t *testing.T) {
	t.Parallel()

	require.Equal(t, mood.MoodLeafy, moodFor(3, 3), "3/3 ready must still report leafy")
	require.Equal(t, mood.MoodSprouting, moodFor(2, 3), "2/3 ready must still report sprouting")
	require.Equal(t, mood.MoodThirsty, moodFor(1, 3), "1/3 ready must still report thirsty")
}

// TestMoodFor_ScaledToZeroIsNotWilting is the deliberate negative case
// alongside TestMoodFor_ZeroReadyReportsWilting: a Plant with desired == 0
// (e.g. freshly scaled down via `kubectl scale plant fernie --replicas=0`)
// has zero ready replicas for exactly the same surface reason a wilting
// Plant does, but it is idle by request, not sick -- moodFor's own
// ready==0&&desired>0 special case must not fire for it. What it SHOULD
// report is exactly what it reported before this fix (that path is
// untouched): the ordinary Score-based computation for Ready=false,
// ErrorRate=0, which lands on mood.MoodNotTooHot (Score computes to 35 --
// the not-Ready ceiling -- landing in the "not-too-hot" band, [20,40)).
// Asserted here as a literal value, not re-derived via mood.Signals/
// FromScore, specifically so a future change to either this file or
// internal/mood's ceiling/threshold constants that silently moved this
// case is caught by an assertion that doesn't move with it.
func TestMoodFor_ScaledToZeroIsNotWilting(t *testing.T) {
	t.Parallel()

	got := moodFor(0, 0)

	require.NotEqual(t, mood.MoodWilting, got, "a scaled-to-zero Plant (desired == 0) is idle, not sick, and must not report wilting")
	require.Equal(t, mood.MoodNotTooHot, got, "a scaled-to-zero Plant should report the same mood it always did: not-too-hot (idle, not wilting)")
}

// desiredCaseName names a moodFor sub-test by its (ready, desired) pair, for
// TestMoodFor_ZeroReadyReportsWilting and TestMoodFor_AllSixMoodsAreReachable
// below.
func desiredCaseName(ready, desired int32) string {
	return fmt.Sprintf("ready=%d/desired=%d", ready, desired)
}

// TestMoodFor_AllSixMoodsAreReachable is the reachability guard the task
// brief for this fix calls out by name: it sweeps a representative range of
// (ready, desired) pairs through moodFor and asserts every one of
// internal/mood.AllMoods() is produced by at least one combination.
// mood.MoodWilting was the mood this whole fix exists to make reachable
// again; this test is what stops a FUTURE change from silently orphaning
// any mood (this one or another) the same way, by failing loudly the
// moment the set of moods moodFor can actually produce stops being all
// six -- rather than requiring a human to notice a gap in
// `kubectl get plants` output on a live cluster, which is how this bug was
// actually found.
//
// The sweep is desired 0..10, ready 0..desired -- desired == 0 (a
// deliberately scaled-to-zero Plant) is included deliberately, not merely
// for completeness: it is, after this fix, the ONLY combination that still
// produces mood.MoodNotTooHot at all. Before this fix, ready == 0 with
// desired > 0 ALSO produced it (that was the bug); after the fix, every
// ready == 0 && desired > 0 combination reports mood.MoodWilting instead
// (see TestMoodFor_ZeroReadyReportsWilting), so mood.MoodNotTooHot's only
// remaining path is the idle, scaled-to-zero case
// (TestMoodFor_ScaledToZeroIsNotWilting asserts that specific combination
// directly). A sweep that excluded desired == 0 would report
// mood.MoodNotTooHot as unreachable and fail -- which would be a true
// statement about that narrower domain, but a misleading one about moodFor
// as a whole, and exactly the kind of surprising narrowing this test exists
// to surface rather than paper over with a smaller sweep.
func TestMoodFor_AllSixMoodsAreReachable(t *testing.T) {
	t.Parallel()

	produced := make(map[mood.Mood][]string)
	for desired := int32(0); desired <= 10; desired++ {
		for ready := int32(0); ready <= desired; ready++ {
			m := moodFor(ready, desired)
			produced[m] = append(produced[m], desiredCaseName(ready, desired))
		}
	}

	for _, m := range mood.AllMoods() {
		require.NotEmpty(t, produced[m], "mood %q is unreachable from moodFor across the whole ready=0..10/desired=0..10 sweep -- it has become dead code from the operator's perspective, the exact bug this test exists to catch", m)
	}

	// mood.MoodNotTooHot's only reachable combination in this sweep must be
	// the scaled-to-zero case, exactly -- if some OTHER combination started
	// producing it too, that would mean the ready == 0 && desired > 0
	// special case in moodFor stopped covering everything it should, which
	// is worth catching explicitly rather than only implicitly (this
	// sweep's own map would still look "non-empty" and pass the loop above
	// even if a stray, incorrect combination also produced it).
	require.Equal(t, []string{desiredCaseName(0, 0)}, produced[mood.MoodNotTooHot],
		"mood.MoodNotTooHot must be reachable from EXACTLY the scaled-to-zero case (ready=0/desired=0) after this fix -- any other combination producing it means the ready==0&&desired>0 special case in moodFor has a gap")

	// Documents exactly which combinations produce each mood, in the test
	// output, so a future reader can see the reachability proof directly
	// rather than only its pass/fail outcome.
	t.Logf("mood -> producing (ready/desired) combinations: %+v", produced)
}

// TestComputeStatus_NilReplicasDefaultsToThree asserts a Plant built
// without going through API-server defaulting (Spec.Replicas == nil) is
// still handled correctly, mirroring the CRD's own +kubebuilder:default=3.
func TestComputeStatus_NilReplicasDefaultsToThree(t *testing.T) {
	t.Parallel()
	stubNow(t, time.Now())

	plant := testPlant(1, nil)
	deployment := testDeployment(3)

	status := computeStatus(plant, deployment)

	require.Equal(t, int32(3), status.DesiredReplicas)
	require.Equal(t, int32(100), status.HealthPercent)
}

// TestComputeStatus_ObservedGenerationPropagates asserts
// metadata.generation flows into both PlantStatus.ObservedGeneration and
// every individual condition's ObservedGeneration.
func TestComputeStatus_ObservedGenerationPropagates(t *testing.T) {
	t.Parallel()
	stubNow(t, time.Now())

	const generation = int64(7)
	plant := testPlant(generation, ptrInt32(3))
	deployment := testDeployment(3)

	status := computeStatus(plant, deployment)

	require.Equal(t, generation, status.ObservedGeneration)
	require.NotEmpty(t, status.Conditions)
	for _, c := range status.Conditions {
		require.Equal(t, generation, c.ObservedGeneration, "condition %s", c.Type)
	}
}

// TestConditionsFor covers every row of the conditions table in the
// operator plan's Global Constraints, asserting Type, Status, and Reason
// for each.
func TestConditionsFor(t *testing.T) {
	t.Parallel()

	rolledOutDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 1},
	}
	notObservedDeployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 1},
	}

	tests := []struct {
		name       string
		ready      int32
		desired    int32
		deployment *appsv1.Deployment
		wantType   string
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{"ready true when fully ready", 3, 3, rolledOutDeployment, ConditionReady, metav1.ConditionTrue, ReasonAllReplicasReady},
		{"ready false when under-ready", 1, 3, rolledOutDeployment, ConditionReady, metav1.ConditionFalse, ReasonReplicasNotReady},
		{"ready false when zero desired", 0, 0, rolledOutDeployment, ConditionReady, metav1.ConditionFalse, ReasonReplicasNotReady},

		{"progressing true when generation not observed", 3, 3, notObservedDeployment, ConditionProgressing, metav1.ConditionTrue, ReasonRolloutInProgress},
		{"progressing true when ready != desired", 1, 3, rolledOutDeployment, ConditionProgressing, metav1.ConditionTrue, ReasonRolloutInProgress},
		{"progressing false when fully rolled out", 3, 3, rolledOutDeployment, ConditionProgressing, metav1.ConditionFalse, ReasonRolloutComplete},

		{"degraded true when zero ready and desired positive", 0, 3, rolledOutDeployment, ConditionDegraded, metav1.ConditionTrue, ReasonInsufficientReplicas},
		{"degraded false when at least one ready", 1, 3, rolledOutDeployment, ConditionDegraded, metav1.ConditionFalse, ReasonPlantHealthy},
		{"degraded false when zero desired", 0, 0, rolledOutDeployment, ConditionDegraded, metav1.ConditionFalse, ReasonPlantHealthy},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			conditions := conditionsFor(tc.ready, tc.desired, tc.deployment, 1)

			var found *metav1.Condition
			for i := range conditions {
				if conditions[i].Type == tc.wantType {
					found = &conditions[i]
					break
				}
			}
			require.NotNil(t, found, "no %s condition returned", tc.wantType)
			require.Equal(t, tc.wantStatus, found.Status)
			require.Equal(t, tc.wantReason, found.Reason)
		})
	}
}

// TestStatusChanged_LastWateredAloneIsNotAChange is the direct regression
// test for Bug A, the infinite write loop: LastWatered advances on every
// reconcile by construction (computeStatus always sets it to now()), so if
// statusChanged treated a LastWatered-only difference as "changed", every
// single reconcile of every Plant would trigger a status-subresource write,
// forever. This test asserts two statuses identical in every field except
// LastWatered compare as UNCHANGED.
func TestStatusChanged_LastWateredAloneIsNotAChange(t *testing.T) {
	t.Parallel()

	base := buddyv1alpha1.PlantStatus{
		Mood:               string(mood.MoodLeafy),
		HealthPercent:      100,
		ReadyReplicas:      3,
		DesiredReplicas:    3,
		ObservedGeneration: 1,
		LastWatered:        &metav1.Time{Time: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
		Conditions: []metav1.Condition{
			{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: ReasonAllReplicasReady, ObservedGeneration: 1},
		},
	}

	later := base
	laterWatered := &metav1.Time{Time: base.LastWatered.Add(30 * time.Second)}
	later.LastWatered = laterWatered

	require.False(t, statusChanged(base, later),
		"a LastWatered-only difference must compare as unchanged, or every WateringInterval requeue writes status unconditionally")
}

// TestStatusChanged_RealChangeIsDetected is statusChanged's complementary
// case: a genuine change (here, HealthPercent) must still be detected even
// though LastWatered is excluded from the comparison, proving the guard
// above doesn't silently swallow real updates along with the clock.
func TestStatusChanged_RealChangeIsDetected(t *testing.T) {
	t.Parallel()

	base := buddyv1alpha1.PlantStatus{
		HealthPercent:   100,
		ReadyReplicas:   3,
		DesiredReplicas: 3,
		LastWatered:     &metav1.Time{Time: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
	}

	changed := base
	changed.HealthPercent = 67
	changed.ReadyReplicas = 2
	changed.LastWatered = &metav1.Time{Time: base.LastWatered.Add(30 * time.Second)}

	require.True(t, statusChanged(base, changed))
}
