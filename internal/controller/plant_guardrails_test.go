//go:build envtest

// This file holds the envtest cases for the reconciler's REFUSALS -- the
// inputs it must decline to act on rather than reconcile. plant_controller_test.go
// covers the happy path and drift correction; everything here is a case where
// the correct behavior is "do nothing to the cluster, and say why on the
// Plant."
//
// See suite_test.go for the shared control plane, manager, and helpers.
package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	buddyv1alpha1 "github.com/sean-kramer/k8s-buddy/api/v1alpha1"
)

// --- refusing to adopt a pre-existing, unowned object --------------------

// TestReconcile_RefusesToAdoptForeignDeployment is the adoption-hazard case.
//
// controllerutil.CreateOrUpdate is Get-then-mutate-then-Update, and
// controllerutil.SetControllerReference stamps ownership onto whatever object
// it is handed. Together, unguarded, they mean this operator ADOPTS any
// pre-existing object that merely happens to share a child's name. The
// concrete scenario that motivated the guard: a Plant named `buddy-api`
// created in namespace `k8s-buddy` would have seized Plan 1's own static
// Deployment, Service, and ServiceAccount -- and then wedged permanently,
// because mutateDeployment sets spec.selector only when nil, so the seized
// Deployment would keep Plan 1's three-key selector while its pod template
// got this operator's two-key label set, making every subsequent Update an
// illegal change to an immutable field.
//
// The assertion is deliberately two-sided: the Plant must go Degraded with
// reason ConflictingResource, AND the pre-existing object must come back out
// untouched -- no owner reference, no relabelling, original spec. Asserting
// only the condition would let through a version that seized the object and
// then complained about it.
func TestReconcile_RefusesToAdoptForeignDeployment(t *testing.T) {
	ns := newTestNamespace(t)

	// A plausible pre-existing workload: labelled the way a human or
	// kustomize would label it, and specifically NOT carrying
	// app.kubernetes.io/managed-by=plant-operator.
	foreign := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "squatter",
			Namespace: ns,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "buddy-api",
				"app.kubernetes.io/managed-by": "kustomize",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "squatter"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "squatter"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "squatter", Image: "registry.example/squatter:v1"}},
				},
			},
		},
	}
	require.NoError(t, testClient.Create(testCtx, foreign))

	plant := newTestPlant(ns, "squatter", 3)
	createPlant(t, plant)

	require.Eventually(t, func() bool {
		got := &buddyv1alpha1.Plant{}
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(plant), got); err != nil {
			return false
		}
		degraded := conditionsByType(got.Status.Conditions)[ConditionDegraded]
		return degraded.Status == metav1.ConditionTrue && degraded.Reason == "ConflictingResource"
	}, 15*time.Second, 100*time.Millisecond,
		"plant never went Degraded=True/ConflictingResource against a pre-existing, unowned Deployment of the same name")

	after := &appsv1.Deployment{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "squatter"}, after))
	require.Empty(t, after.OwnerReferences, "the pre-existing Deployment must not have been adopted")
	require.Equal(t, "kustomize", after.Labels["app.kubernetes.io/managed-by"],
		"the pre-existing Deployment's own labels must be left alone")
	require.NotNil(t, after.Spec.Replicas)
	require.EqualValues(t, 1, *after.Spec.Replicas, "the pre-existing Deployment's spec must be left alone")
	require.Equal(t, "registry.example/squatter:v1", after.Spec.Template.Spec.Containers[0].Image,
		"the pre-existing Deployment's container must be left alone")
}

// TestReconcile_RestoresStrippedManagedByLabelWithoutDegrading is the
// regression case for the guard above, and it asserts the OPPOSITE outcome
// from its own test's namesake -- which is exactly why both have to exist.
//
// The first version of assertNotForeign checked the
// app.kubernetes.io/managed-by label BEFORE the controller owner reference,
// and refused unconditionally on a mismatch. That made one kubectl command --
//
//	kubectl label deploy fernie app.kubernetes.io/managed-by-
//
// -- wedge a Plant at Degraded/ConflictingResource permanently: the operator
// refused to touch its OWN child over a label mergeLabels would have restored
// on the very next pass. It was compounded by the informer cache being
// narrowed by that same label, so the stripped child stopped producing watch
// events and the wedge went unnoticed until the next watering interval.
//
// The fix is ordering, not leniency: a controller owner reference whose UID
// matches this Plant is definitive, unforgeable proof of ownership and is
// checked first; the label is consulted only when no such reference exists.
// So a stripped label is now ordinary drift, corrected like any other.
//
// TestReconcile_RefusesToAdoptForeignDeployment must keep passing alongside
// this: an object with NEITHER a matching owner reference NOR the label is
// still somebody else's, and is still refused.
func TestReconcile_RestoresStrippedManagedByLabelWithoutDegrading(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "relabelled", 3)
	createPlant(t, plant)

	waitForChildrenExist(t, plant)
	waitForStatusPopulated(t, plant)
	waitForReconcileQuiescence(t)

	key := client.ObjectKey{Namespace: ns, Name: "relabelled"}

	// Sanity-check the starting state, so a failure below cannot be blamed on
	// the label never having been there.
	before := &appsv1.Deployment{}
	require.NoError(t, testClient.Get(testCtx, key, before))
	require.Equal(t, ManagedByValue, before.Labels[LabelManagedBy])
	require.NotEmpty(t, metav1.GetControllerOf(before), "the child must be owned before the label is stripped")

	// The kubectl command, in Go: strip the label the guard used to key on.
	stripped := before.DeepCopy()
	delete(stripped.Labels, LabelManagedBy)
	require.NoError(t, testClient.Update(testCtx, stripped))

	// The operator must put it back rather than refuse to touch the object.
	require.Eventually(t, func() bool {
		got := &appsv1.Deployment{}
		if err := testClient.Get(testCtx, key, got); err != nil {
			return false
		}
		return got.Labels[LabelManagedBy] == ManagedByValue
	}, 15*time.Second, 100*time.Millisecond,
		"the operator never restored %s on its own child -- it is refusing to adopt an object it created", LabelManagedBy)

	// And it must not have complained about its own child on the way.
	got := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plant), got))
	degraded := conditionsByType(got.Status.Conditions)[ConditionDegraded]
	require.NotEqual(t, ReasonConflictingResource, degraded.Reason,
		"stripping a label from a child the Plant demonstrably owns must not raise ConflictingResource: %s", degraded.Message)
}

// TestReconcile_AdoptsOrphanedChildOfSameNamedPlant covers the second half of
// the reordering: an object carrying this operator's managed-by label whose
// controller owner reference does NOT point at the current Plant.
//
// The realistic way to reach that state is a delete-then-recreate of the same
// Plant name. The old Plant's children are orphaned the instant it is
// deleted, and the garbage collector is asynchronous -- so a replacement Plant
// created promptly enough finds a same-named child still on the cluster,
// carrying the label but owned by a UID that no longer exists. Refusing there
// would make a perfectly ordinary `kubectl delete && kubectl apply` cycle fail
// intermittently, depending on how fast GC happened to be.
//
// Adopting is safe: every child's name and spec.selector are derived from the
// PLANT'S NAME, so the stale object's immutable fields are identical to the
// ones the replacement would create, and there is no immutable-field conflict
// to hit. This test simulates the state directly (envtest runs no garbage
// collector, so a real delete-recreate could not be staged here) by pointing a
// child's controller reference at a foreign UID while leaving the label on.
func TestReconcile_AdoptsOrphanedChildOfSameNamedPlant(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "readopted", 3)
	createPlant(t, plant)

	waitForChildrenExist(t, plant)
	waitForStatusPopulated(t, plant)
	waitForReconcileQuiescence(t)

	key := client.ObjectKey{Namespace: ns, Name: "readopted"}
	current := &appsv1.Deployment{}
	require.NoError(t, testClient.Get(testCtx, key, current))

	// Re-point the controller reference at a Plant UID that does not exist,
	// exactly as an orphaned child of a deleted, same-named Plant would look.
	orphaned := current.DeepCopy()
	for i := range orphaned.OwnerReferences {
		if orphaned.OwnerReferences[i].Controller != nil && *orphaned.OwnerReferences[i].Controller {
			orphaned.OwnerReferences[i].UID = "00000000-0000-0000-0000-00000000dead"
		}
	}
	orphaned.Spec.Replicas = int32Ptr(99) // drift, so re-adoption is observable
	require.NoError(t, testClient.Update(testCtx, orphaned))

	// The operator must re-adopt it: owner reference back to the live Plant,
	// and the drift corrected.
	require.Eventually(t, func() bool {
		got := &appsv1.Deployment{}
		if err := testClient.Get(testCtx, key, got); err != nil {
			return false
		}
		owner := metav1.GetControllerOf(got)
		return owner != nil && owner.UID == plant.UID && got.Spec.Replicas != nil && *got.Spec.Replicas == 3
	}, 15*time.Second, 100*time.Millisecond,
		"the operator never re-adopted a labelled, orphaned child -- a delete-then-recreate of the same Plant name would fail while GC lags")

	got := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plant), got))
	degraded := conditionsByType(got.Status.Conditions)[ConditionDegraded]
	require.NotEqual(t, ReasonConflictingResource, degraded.Reason,
		"re-adopting an orphaned child this operator generated must not raise ConflictingResource: %s", degraded.Message)
}

// --- a name too long to be a label value ---------------------------------

// TestReconcile_DegradesOnOverLongName covers the silent-forever failure.
//
// LabelsFor puts the Plant's own name into two label VALUES, and Kubernetes
// caps a label value at 63 characters -- while metadata.name for a namespaced
// object may be up to 253. A 64-character Plant therefore produced children
// the API server rejected on every single create attempt, forever, with the
// only signal a repeating error in the operator's log. The reconciler now
// refuses up front and says so on the object itself.
func TestReconcile_DegradesOnOverLongName(t *testing.T) {
	ns := newTestNamespace(t)

	// Exactly one character past the label-value limit: the boundary is the
	// interesting input, not an absurdly long string a sloppier check would
	// also reject.
	name := strings.Repeat("a", maxPlantNameLength+1)
	plant := newTestPlant(ns, name, 3)
	createPlant(t, plant)

	require.Eventually(t, func() bool {
		got := &buddyv1alpha1.Plant{}
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(plant), got); err != nil {
			return false
		}
		degraded := conditionsByType(got.Status.Conditions)[ConditionDegraded]
		return degraded.Status == metav1.ConditionTrue && degraded.Reason == "InvalidName"
	}, 15*time.Second, 100*time.Millisecond, "plant with a 64-character name never went Degraded=True/InvalidName")

	// And it refused up front rather than half-creating: no children at all.
	require.True(t, apierrors.IsNotFound(
		testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: name}, &appsv1.Deployment{})),
		"no child should have been attempted for a Plant whose name cannot be a label value")
}

// --- the API server itself rejects a pathological spec -------------------

// TestPlantAdmission_RejectsOutOfRangeWateringInterval exercises the OTHER
// layer of the sub-second-requeue fix: not the reconciler's clamp (see
// TestRequeueIntervalFor, which covers that directly and without a control
// plane) but the CRD's own schema, enforced here by a real API server. The
// two layers guard different populations -- the schema catches every Plant a
// user can create, the clamp catches a Plant constructed directly in Go that
// never passed through admission -- and neither subsumes the other, so both
// are tested.
func TestPlantAdmission_RejectsOutOfRangeWateringInterval(t *testing.T) {
	ns := newTestNamespace(t)

	tests := []struct {
		name     string
		interval time.Duration
	}{
		// The original defect, verbatim: a perfectly valid Plant that
		// produced a 1ms requeue loop against the API server for as long as
		// the object existed.
		{"one millisecond", time.Millisecond},
		{"one second", time.Second},
		{"just under the floor", 29 * time.Second},
		{"absurdly long", 72 * time.Hour},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plant := newTestPlant(ns, "rejected", 3)
			plant.Spec.WateringInterval = metav1.Duration{Duration: tc.interval}
			err := testClient.Create(testCtx, plant)
			require.Error(t, err, "the API server must reject wateringInterval=%s", tc.interval)
			require.True(t, apierrors.IsInvalid(err), "expected an Invalid error, got %v", err)
		})
	}

	// The boundary value itself must still be accepted, or the bound would
	// be off by one in the direction that breaks legitimate Plants.
	accepted := newTestPlant(ns, "accepted", 3)
	accepted.Spec.WateringInterval = metav1.Duration{Duration: minRequeueInterval}
	createPlant(t, accepted)
}

// TestPlantAdmission_RejectsMalformedImage covers the Image field's Pattern.
// Unvalidated free text there is a Plant that looks fine in `kubectl get
// plant` while its pods sit in ImagePullBackOff, with the real cause three
// `kubectl describe` calls away.
func TestPlantAdmission_RejectsMalformedImage(t *testing.T) {
	ns := newTestNamespace(t)

	for _, image := range []string{
		"not a valid image",   // spaces
		"ghcr.io/foo:bad tag", // space in the tag
		"-leading-dash/foo",   // must start alphanumeric
		"foo//bar",            // empty path component
	} {
		t.Run("rejects "+image, func(t *testing.T) {
			plant := newTestPlant(ns, "badimage", 3)
			plant.Spec.Image = image
			err := testClient.Create(testCtx, plant)
			require.Error(t, err, "the API server must reject image=%q", image)
			require.True(t, apierrors.IsInvalid(err), "expected an Invalid error, got %v", err)
		})
	}

	// Shapes that must keep working: the project's own default, a bare name,
	// a registry with a port, and a digest reference.
	for _, image := range []string{
		"ghcr.io/sean-kramer/k8s-buddy/buddy-api:dev",
		"buddy-api",
		"localhost:5000/buddy-api:v1.2.3",
		"ghcr.io/sean-kramer/k8s-buddy/buddy-api@sha256:" + strings.Repeat("a", 64),
	} {
		t.Run("accepts "+image, func(t *testing.T) {
			plant := newTestPlant(ns, "goodimage", 3)
			plant.Spec.Image = image
			createPlant(t, plant)
		})
	}
}
