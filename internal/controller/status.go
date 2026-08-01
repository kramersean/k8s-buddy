// This file (status.go) contains only the "desired status" computation: a
// pure function from a *Plant and its observed Deployment to the PlantStatus
// the operator should write. Like resources.go's builders, it takes no
// client.Client and no context.Context — the one exception is the package
// clock, which is read through the unexported now indirection below so
// tests can stub it instead of racing the real clock. Keeping the
// arithmetic here, separate from plant_controller.go's I/O, is what lets
// every rule in the conditions table be asserted directly in status_test.go
// without a fake client or a running API server.
//
// See resources.go for the package's own doc comment.

package controller

import (
	"math"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	buddyv1alpha1 "github.com/sean-kramer/k8s-buddy/api/v1alpha1"
	"github.com/sean-kramer/k8s-buddy/internal/mood"
)

// Condition types and reasons for Plant status, per the operator plan's
// Global Constraints conditions table. Values are binding — copied
// verbatim, not paraphrased.
const (
	// ConditionReady reports whether all desired replicas are ready.
	ConditionReady = "Ready"
	// ConditionProgressing reports whether the Deployment's rollout is
	// still converging toward the desired state.
	ConditionProgressing = "Progressing"
	// ConditionDegraded reports whether the Plant has zero ready replicas
	// while replicas are desired.
	ConditionDegraded = "Degraded"

	// ReasonAllReplicasReady is Ready=True: ready == desired, desired > 0.
	ReasonAllReplicasReady = "AllReplicasReady"
	// ReasonReplicasNotReady is Ready=False: ready < desired.
	ReasonReplicasNotReady = "ReplicasNotReady"
	// ReasonRolloutInProgress is Progressing=True: the Deployment's
	// generation has not yet been observed, or ready != desired.
	ReasonRolloutInProgress = "RolloutInProgress"
	// ReasonRolloutComplete is Progressing=False: the Deployment is fully
	// rolled out.
	ReasonRolloutComplete = "RolloutComplete"
	// ReasonInsufficientReplicas is Degraded=True: ready == 0 and
	// desired > 0.
	ReasonInsufficientReplicas = "InsufficientReplicas"
	// ReasonPlantHealthy is Degraded=False: any state other than
	// InsufficientReplicas.
	ReasonPlantHealthy = "PlantHealthy"
)

// now returns the current time. It is a package-level indirection purely so
// tests can stub it out and assert a deterministic LastWatered instead of
// racing the real clock — the same pattern internal/mood uses for
// CheckedAt.
var now = time.Now

// computeStatus derives the PlantStatus that plant should carry, given the
// Deployment the reconciler observed for it. It is pure: the same (plant,
// deployment) pair — modulo the now() read for LastWatered — always
// produces the same result, which is what makes the change-comparison in
// statusChanged (see plant_controller.go) meaningful: two calls to
// computeStatus for an unchanged Plant and Deployment differ only in
// LastWatered.
//
// plant.Status.Conditions is read (not mutated) as the prior condition set
// so meta.SetStatusCondition can preserve LastTransitionTime across calls
// where the condition's Status hasn't changed; the returned PlantStatus
// carries a freshly built Conditions slice, never plant.Status.Conditions
// itself.
func computeStatus(plant *buddyv1alpha1.Plant, deployment *appsv1.Deployment) buddyv1alpha1.PlantStatus {
	ready := deployment.Status.ReadyReplicas
	desired := desiredReplicas(plant)
	generation := plant.Generation

	status := buddyv1alpha1.PlantStatus{
		Mood:               string(moodFor(ready, desired)),
		HealthPercent:      healthPercent(ready, desired),
		ReadyReplicas:      ready,
		DesiredReplicas:    desired,
		ObservedGeneration: generation,
		LastWatered:        &metav1.Time{Time: now()},
	}

	// meta.SetStatusCondition mutates the conditions slice it's given in
	// place (appending or updating), so it needs to start from a copy of
	// the Plant's current conditions to preserve LastTransitionTime for
	// conditions whose Status hasn't changed — not a nil slice, which
	// would report every condition as freshly transitioning on every
	// single reconcile.
	conditions := append([]metav1.Condition(nil), plant.Status.Conditions...)
	for _, c := range conditionsFor(ready, desired, deployment, generation) {
		meta.SetStatusCondition(&conditions, c)
	}
	status.Conditions = conditions

	return status
}

// desiredReplicas returns plant's desired replica count, defaulting to 3
// when Spec.Replicas is nil. Mirrors resources.go's replicasFor: a
// hand-constructed Plant (as Task 4's envtest suite and this package's own
// tests build) may bypass API-server defaulting, so this stays nil-safe
// rather than assuming the pointer is always set.
//
// This deliberately reads plant.Spec.Replicas rather than the observed
// Deployment's spec.replicas, even though the two agree once a reconcile
// has actually run. status.desiredReplicas is meant to answer "how many
// replicas does this Plant want," not "how many did the Deployment last
// report" — reading the Plant's own spec means a fresh status computed in
// the same pass that first creates the Deployment (whose spec.replicas the
// reconciler just set to this same value anyway) already reports the
// user's intent, rather than a value that could theoretically lag behind
// it if some other actor (an HPA, a manual scale) had changed the
// Deployment's replicas directly without the Plant's spec having changed.
func desiredReplicas(plant *buddyv1alpha1.Plant) int32 {
	if plant.Spec.Replicas == nil {
		return defaultReplicas
	}
	return *plant.Spec.Replicas
}

// healthPercent returns ready as a percentage of desired, rounded to the
// nearest whole percent, or 0 when desired is 0 rather than an undefined
// division.
func healthPercent(ready, desired int32) int32 {
	if desired == 0 {
		return 0
	}
	pct := math.Round(float64(ready) / float64(desired) * 100)
	return int32(pct)
}

// moodFor derives a Mood from ready/desired by reusing internal/mood's
// scoring ladder instead of reimplementing thresholds here. Latency inputs
// are left at their zero value (P95Latency: 0, LatencyBudget: 0) so the
// latency component of Score always awards full marks; the mood ends up
// driven entirely by readiness and the derived error rate, which is the
// only signal computeStatus has available from a Deployment's status.
func moodFor(ready, desired int32) mood.Mood {
	errorRate := 0.0
	if desired > 0 {
		errorRate = 1 - float64(ready)/float64(desired)
	}

	signals := mood.Signals{
		Ready:     ready > 0,
		ErrorRate: errorRate,
	}
	return mood.FromScore(signals.Score())
}

// conditionsFor returns the Ready, Progressing, and Degraded conditions for
// the given ready/desired counts and observed Deployment, per the operator
// plan's conditions table. Every condition carries ObservedGeneration set
// to generation (the Plant's own metadata.generation), so a consumer can
// tell whether a condition reflects the most recent spec change or an
// older one still being processed.
func conditionsFor(ready, desired int32, deployment *appsv1.Deployment, generation int64) []metav1.Condition {
	transitionTime := metav1.NewTime(now())

	readyCondition := metav1.Condition{
		Type:               ConditionReady,
		ObservedGeneration: generation,
		LastTransitionTime: transitionTime,
	}
	if desired > 0 && ready == desired {
		readyCondition.Status = metav1.ConditionTrue
		readyCondition.Reason = ReasonAllReplicasReady
		readyCondition.Message = "all desired replicas are ready"
	} else {
		readyCondition.Status = metav1.ConditionFalse
		readyCondition.Reason = ReasonReplicasNotReady
		readyCondition.Message = "fewer replicas are ready than desired"
	}

	// "Deployment generation not yet observed" means the Deployment
	// controller hasn't caught up to the latest spec write yet —
	// status.observedGeneration on the Deployment itself lags
	// metadata.generation on the Deployment while a rollout is still
	// being processed.
	generationObserved := deployment.Status.ObservedGeneration >= deployment.Generation
	rolledOut := generationObserved && ready == desired

	progressingCondition := metav1.Condition{
		Type:               ConditionProgressing,
		ObservedGeneration: generation,
		LastTransitionTime: transitionTime,
	}
	if rolledOut {
		progressingCondition.Status = metav1.ConditionFalse
		progressingCondition.Reason = ReasonRolloutComplete
		progressingCondition.Message = "the Deployment is fully rolled out"
	} else {
		progressingCondition.Status = metav1.ConditionTrue
		progressingCondition.Reason = ReasonRolloutInProgress
		progressingCondition.Message = "the Deployment's rollout has not yet converged"
	}

	degradedCondition := metav1.Condition{
		Type:               ConditionDegraded,
		ObservedGeneration: generation,
		LastTransitionTime: transitionTime,
	}
	if ready == 0 && desired > 0 {
		degradedCondition.Status = metav1.ConditionTrue
		degradedCondition.Reason = ReasonInsufficientReplicas
		degradedCondition.Message = "no replicas are ready"
	} else {
		degradedCondition.Status = metav1.ConditionFalse
		degradedCondition.Reason = ReasonPlantHealthy
		degradedCondition.Message = "the Plant has at least one ready replica, or none are desired"
	}

	return []metav1.Condition{readyCondition, progressingCondition, degradedCondition}
}

// statusChanged reports whether newStatus differs from oldStatus in any way
// that should actually be written to the API server.
//
// BUG A (the infinite write loop): every reconcile computes a fresh
// LastWatered timestamp — that field changes on literally every call by
// construction, requeue after requeue, forever. If LastWatered took part in
// this comparison, statusChanged would report "changed" on every single
// pass regardless of whether Mood, HealthPercent, or any condition actually
// moved, and the operator would issue a status-subresource write every
// WateringInterval for the lifetime of every Plant — a write storm that
// looks like "the operator is busy" rather than a bug. So LastWatered is
// deliberately excluded from the comparison below: two statuses that are
// identical except for LastWatered compare as unchanged, and the reconciler
// (plant_controller.go) skips the write. LastWatered itself is still
// updated in memory before this comparison runs; it just isn't why a write
// happens.
func statusChanged(oldStatus, newStatus buddyv1alpha1.PlantStatus) bool {
	old := oldStatus
	old.LastWatered = nil
	next := newStatus
	next.LastWatered = nil

	return !apiequality.Semantic.DeepEqual(old, next)
}
