// This test file lives in package controller (not controller_test) rather
// than resources_test.go's external package specifically so it can stub the
// unexported now indirection in status.go and assert a deterministic
// LastWatered instead of racing the real clock.
package controller

import (
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

			errorRate := 0.0
			if tc.desired > 0 {
				errorRate = 1 - float64(tc.ready)/float64(tc.desired)
			}
			wantSignals := mood.Signals{Ready: tc.ready > 0, ErrorRate: errorRate}
			wantMood := mood.FromScore(wantSignals.Score())

			status := computeStatus(plant, deployment)
			require.Equal(t, string(wantMood), status.Mood)
		})
	}
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
