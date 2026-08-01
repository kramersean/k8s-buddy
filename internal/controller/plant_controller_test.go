//go:build envtest

// This file holds the envtest suite's actual test cases -- see
// suite_test.go for the shared control plane / manager / helper machinery
// every test below relies on, and counting_client_test.go for why case 5
// needs more than a resourceVersion comparison.
package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	buddyv1alpha1 "github.com/sean-kramer/k8s-buddy/api/v1alpha1"
)

// --- case 1: creation owns all four children ----------------------------

func TestReconcile_CreatesAllFourChildrenWithOwnerReferences(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "fernie", 3)
	createPlant(t, plant)

	waitForChildrenExist(t, plant)

	owner := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plant), owner))

	deployment := &appsv1.Deployment{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, deployment))
	assertControllerOwnerRef(t, deployment, owner)

	service := &corev1.Service{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, service))
	assertControllerOwnerRef(t, service, owner)

	configMap := &corev1.ConfigMap{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, configMap))
	assertControllerOwnerRef(t, configMap, owner)

	pdb := &policyv1.PodDisruptionBudget{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie-pdb"}, pdb))
	assertControllerOwnerRef(t, pdb, owner)
}

// --- case 2: finalizer added on creation ---------------------------------

func TestReconcile_AddsFinalizerOnCreate(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "fernie", 3)
	createPlant(t, plant)

	waitForFinalizer(t, plant)
}

// --- case 3: honest not-ready status, plus a clearly-separate ready path ---

// TestReconcile_StatusReflectsNotReadyWithNoKubelet is the honest case:
// envtest's control plane runs no kubelet (and no kube-controller-manager),
// so a Deployment's Pods are never actually started anywhere and
// status.readyReplicas never leaves 0. That is not a limitation to route
// around -- it is exactly the state a freshly-created Plant is really in
// before anything has started serving traffic, and asserting it here proves
// the not-ready path (Ready=False/ReplicasNotReady) actually works, rather
// than assuming it does because the ready path (below) was never exercised.
func TestReconcile_StatusReflectsNotReadyWithNoKubelet(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "fernie", 4)
	createPlant(t, plant)

	got := waitForStatusPopulated(t, plant)

	require.EqualValues(t, 4, got.Status.DesiredReplicas)
	require.EqualValues(t, 0, got.Status.ReadyReplicas)

	conditions := conditionsByType(got.Status.Conditions)
	ready, ok := conditions[ConditionReady]
	require.True(t, ok, "no Ready condition present")
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	require.Equal(t, ReasonReplicasNotReady, ready.Reason)
}

// TestReconcile_StatusReflectsReadyWhenKubeletSimulated is the separate,
// clearly-named case the task brief explicitly sanctions: since nothing in
// this suite's control plane will ever move a Pod (or a Deployment's
// aggregate status) to Ready on its own, this test simulates what a real
// kubelet (and the Deployment controller rolling Pod readiness up into
// status.readyReplicas) would produce, by patching the Deployment's status
// subresource directly. This is legitimate -- it exercises computeStatus's
// Ready=True path against a real object read back from a real API server --
// it just skips the (out of scope for this operator) machinery that would
// normally produce that status on a live cluster.
func TestReconcile_StatusReflectsReadyWhenKubeletSimulated(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "fernie", 2)
	createPlant(t, plant)

	waitForChildrenExist(t, plant)

	deployment := &appsv1.Deployment{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, deployment))
	deployment.Status.Replicas = 2
	deployment.Status.ReadyReplicas = 2
	deployment.Status.AvailableReplicas = 2
	deployment.Status.ObservedGeneration = deployment.Generation
	require.NoError(t, testClient.Status().Update(testCtx, deployment))

	require.Eventually(t, func() bool {
		got := &buddyv1alpha1.Plant{}
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(plant), got); err != nil {
			return false
		}
		return got.Status.ReadyReplicas == 2
	}, 10*time.Second, 100*time.Millisecond, "status.readyReplicas never reflected the simulated-kubelet Deployment status")

	got := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plant), got))
	conditions := conditionsByType(got.Status.Conditions)
	ready, ok := conditions[ConditionReady]
	require.True(t, ok, "no Ready condition present")
	require.Equal(t, metav1.ConditionTrue, ready.Status)
	require.Equal(t, ReasonAllReplicasReady, ready.Reason)
}

// --- case 4: drift correction --------------------------------------------

func TestReconcile_DriftCorrection_DeploymentReplicasRestored(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "fernie", 3)
	createPlant(t, plant)

	waitForChildrenExist(t, plant)

	key := client.ObjectKey{Namespace: ns, Name: "fernie"}
	deployment := &appsv1.Deployment{}
	require.NoError(t, testClient.Get(testCtx, key, deployment))
	deployment.Spec.Replicas = int32Ptr(99)
	require.NoError(t, testClient.Update(testCtx, deployment))

	require.Eventually(t, func() bool {
		got := &appsv1.Deployment{}
		if err := testClient.Get(testCtx, key, got); err != nil {
			return false
		}
		return got.Spec.Replicas != nil && *got.Spec.Replicas == 3
	}, 10*time.Second, 100*time.Millisecond, "reconciler never corrected drifted Deployment.spec.replicas back to 3")
}

// --- case 5: idempotence --------------------------------------------------

// TestReconcile_Idempotence_SteadyStateReconcileWritesNothing is the
// important one: see counting_client_test.go's doc comment for why
// resourceVersion cannot be trusted here. This lets the Plant settle,
// resets the counting client's tallies, triggers exactly one more
// reconcile, and asserts it performed zero Create/Update/Patch calls
// against all four children AND zero status-subresource writes.
func TestReconcile_Idempotence_SteadyStateReconcileWritesNothing(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "idempotent", 3)
	createPlant(t, plant)

	waitForFinalizer(t, plant)
	waitForChildrenExist(t, plant)
	waitForStatusPopulated(t, plant)

	testCounting.reset()

	triggerReconcile(t, plant)

	writes := testCounting.snapshot()
	for _, gvk := range childGVKs() {
		wc := writes[gvk]
		require.Zero(t, wc.total(),
			"expected zero Create/Update/Patch against %s on a steady-state reconcile, got %+v", gvk, wc)
	}

	statusWrites := testCounting.statusSnapshot()
	var statusTotal int
	for _, wc := range statusWrites {
		statusTotal += wc.total()
	}
	require.Zero(t, statusTotal, "expected zero status-subresource writes on a steady-state reconcile, got %+v", statusWrites)
}

// TestReconcile_Idempotence_CreateOrUpdateReturnsOperationResultNone is
// case 5's second required assertion: that controllerutil.CreateOrUpdate
// itself reports OperationResultNone (not merely "we observed zero writes
// from outside") on a steady-state pass. It calls the reconciler's own
// unexported mutate* methods directly -- reachable because this file lives
// in package controller, not controller_test -- against the live objects
// the suite has already reconciled to steady state, which is the most
// direct way to inspect CreateOrUpdate's actual return value without
// modifying plant_controller.go to expose it.
func TestReconcile_Idempotence_CreateOrUpdateReturnsOperationResultNone(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "steady", 3)
	createPlant(t, plant)

	waitForFinalizer(t, plant)
	waitForChildrenExist(t, plant)
	waitForStatusPopulated(t, plant)
	waitForReconcileQuiescence(t)

	fresh := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plant), fresh))

	// A throwaway reconciler sharing the real test scheme -- mutateDeployment
	// et al. only ever read r.Scheme (for SetControllerReference), never
	// r.Client, so this needs no client of its own at all.
	reconciler := &PlantReconciler{Scheme: testScheme}

	deployment := &appsv1.Deployment{ObjectMeta: objectMeta(fresh.Name, fresh.Namespace)}
	op, err := controllerutil.CreateOrUpdate(testCtx, testClient, deployment, func() error {
		return reconciler.mutateDeployment(fresh, deployment)
	})
	require.NoError(t, err)
	require.Equal(t, controllerutil.OperationResultNone, op, "Deployment CreateOrUpdate must be a no-op at steady state")

	service := &corev1.Service{ObjectMeta: objectMeta(fresh.Name, fresh.Namespace)}
	op, err = controllerutil.CreateOrUpdate(testCtx, testClient, service, func() error {
		return reconciler.mutateService(fresh, service)
	})
	require.NoError(t, err)
	require.Equal(t, controllerutil.OperationResultNone, op, "Service CreateOrUpdate must be a no-op at steady state")

	configMap := &corev1.ConfigMap{ObjectMeta: objectMeta(fresh.Name, fresh.Namespace)}
	op, err = controllerutil.CreateOrUpdate(testCtx, testClient, configMap, func() error {
		return reconciler.mutateConfigMap(fresh, configMap)
	})
	require.NoError(t, err)
	require.Equal(t, controllerutil.OperationResultNone, op, "ConfigMap CreateOrUpdate must be a no-op at steady state")

	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: objectMeta(fresh.Name+"-pdb", fresh.Namespace)}
	op, err = controllerutil.CreateOrUpdate(testCtx, testClient, pdb, func() error {
		return reconciler.mutatePodDisruptionBudget(fresh, pdb)
	})
	require.NoError(t, err)
	require.Equal(t, controllerutil.OperationResultNone, op, "PodDisruptionBudget CreateOrUpdate must be a no-op at steady state")
}

// TestComputeStatus_LastTransitionTimePreservedAcrossNoOpCall is case 5's
// other required assertion: the property Task 3's unit tests could only
// establish by inspection. It calls computeStatus directly (no envtest
// objects needed at all -- this is exactly the pure function status_test.go
// already exercises) twice, feeding the first call's result back in as
// plant.Status exactly the way reconcileStatus does on every subsequent
// pass, and asserts every condition's LastTransitionTime is identical
// between the two calls: proof that meta.SetStatusCondition is doing the
// preservation, not some code path minting a fresh timestamp on a no-op
// pass.
func TestComputeStatus_LastTransitionTimePreservedAcrossNoOpCall(t *testing.T) {
	replicas := int32(3)
	plant := &buddyv1alpha1.Plant{
		ObjectMeta: metav1.ObjectMeta{Name: "fernie", Namespace: "default", Generation: 1},
		Spec:       buddyv1alpha1.PlantSpec{Replicas: &replicas},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 1},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 0},
	}

	first := computeStatus(plant, deployment)
	require.NotEmpty(t, first.Conditions)

	plant.Status = first
	second := computeStatus(plant, deployment)

	firstByType := conditionsByType(first.Conditions)
	secondByType := conditionsByType(second.Conditions)
	require.Len(t, secondByType, len(firstByType))

	for condType, c1 := range firstByType {
		c2, ok := secondByType[condType]
		require.True(t, ok, "condition %s missing from the second computeStatus call", condType)
		require.Equal(t, c1.Status, c2.Status, "condition %s Status changed on a no-op call", condType)
		require.Equal(t, c1.Reason, c2.Reason, "condition %s Reason changed on a no-op call", condType)
		require.True(t, c1.LastTransitionTime.Equal(&c2.LastTransitionTime),
			"condition %s LastTransitionTime changed on a no-op computeStatus call: %v -> %v",
			condType, c1.LastTransitionTime, c2.LastTransitionTime)
	}
}

// --- case 6: spec.replicas propagates --------------------------------------

func TestReconcile_ReplicasUpdatePropagates(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "fernie", 3)
	createPlant(t, plant)

	waitForChildrenExist(t, plant)
	waitForStatusPopulated(t, plant)

	fresh := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plant), fresh))
	fresh.Spec.Replicas = int32Ptr(6)
	require.NoError(t, testClient.Update(testCtx, fresh))

	deploymentKey := client.ObjectKey{Namespace: ns, Name: "fernie"}
	require.Eventually(t, func() bool {
		d := &appsv1.Deployment{}
		if err := testClient.Get(testCtx, deploymentKey, d); err != nil {
			return false
		}
		return d.Spec.Replicas != nil && *d.Spec.Replicas == 6
	}, 10*time.Second, 100*time.Millisecond, "Deployment.spec.replicas never updated to 6")

	require.Eventually(t, func() bool {
		p := &buddyv1alpha1.Plant{}
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(plant), p); err != nil {
			return false
		}
		return p.Status.DesiredReplicas == 6
	}, 10*time.Second, 100*time.Millisecond, "status.desiredReplicas never updated to 6")
}

// --- case 7: spec.resourceProfile propagates -------------------------------

func TestReconcile_ResourceProfileUpdatePropagates(t *testing.T) {
	tests := []struct {
		name    string
		profile string
	}{
		{"medium", "medium"},
		{"large", "large"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ns := newTestNamespace(t)
			plant := newTestPlant(ns, "fernie", 3) // starts "small"
			createPlant(t, plant)

			waitForChildrenExist(t, plant)

			fresh := &buddyv1alpha1.Plant{}
			require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plant), fresh))
			fresh.Spec.ResourceProfile = tc.profile
			require.NoError(t, testClient.Update(testCtx, fresh))

			want := ResourcesFor(tc.profile)
			key := client.ObjectKey{Namespace: ns, Name: "fernie"}
			require.Eventually(t, func() bool {
				d := &appsv1.Deployment{}
				if err := testClient.Get(testCtx, key, d); err != nil || len(d.Spec.Template.Spec.Containers) == 0 {
					return false
				}
				got := d.Spec.Template.Spec.Containers[0].Resources
				return got.Requests.Cpu().Equal(*want.Requests.Cpu()) &&
					got.Requests.Memory().Equal(*want.Requests.Memory()) &&
					got.Limits.Cpu().Equal(*want.Limits.Cpu()) &&
					got.Limits.Memory().Equal(*want.Limits.Memory())
			}, 10*time.Second, 100*time.Millisecond, "container resources never updated to the %s profile", tc.profile)
		})
	}
}

// --- case 8: observedGeneration tracking -----------------------------------

func TestReconcile_ObservedGenerationTracksMetadataGeneration(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "fernie", 3)
	createPlant(t, plant)

	first := waitForStatusPopulated(t, plant)
	require.Equal(t, first.Generation, first.Status.ObservedGeneration)

	fresh := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plant), fresh))
	fresh.Spec.Replicas = int32Ptr(5)
	require.NoError(t, testClient.Update(testCtx, fresh))
	require.Greater(t, fresh.Generation, first.Generation, "a spec change must bump metadata.generation")

	require.Eventually(t, func() bool {
		p := &buddyv1alpha1.Plant{}
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(plant), p); err != nil {
			return false
		}
		return p.Generation > first.Generation && p.Status.ObservedGeneration == p.Generation
	}, 10*time.Second, 100*time.Millisecond, "status.observedGeneration never caught up to the new metadata.generation")
}

// --- case 9: deletion removes the finalizer; owner refs are the GC proof --

// TestReconcile_DeleteRemovesFinalizerAndOwnerReferencesAreCorrect covers
// deletion honestly: envtest runs no garbage-collector controller, so a
// Plant's owned children are never actually removed here when the Plant is
// -- only a real cluster's garbage collector does that, and it does it by
// walking exactly the owner references this test asserts are correct
// before deleting. Asserting the children vanish under envtest would be
// asserting something this suite's control plane cannot actually
// demonstrate; Task 5's live-cluster verification is where cascading
// deletion is proven for real.
func TestReconcile_DeleteRemovesFinalizerAndOwnerReferencesAreCorrect(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "fernie", 3)
	createPlant(t, plant)

	waitForFinalizer(t, plant)
	waitForChildrenExist(t, plant)

	owner := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plant), owner))

	deployment := &appsv1.Deployment{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, deployment))
	assertControllerOwnerRef(t, deployment, owner)

	service := &corev1.Service{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, service))
	assertControllerOwnerRef(t, service, owner)

	configMap := &corev1.ConfigMap{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, configMap))
	assertControllerOwnerRef(t, configMap, owner)

	pdb := &policyv1.PodDisruptionBudget{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie-pdb"}, pdb))
	assertControllerOwnerRef(t, pdb, owner)

	require.NoError(t, testClient.Delete(testCtx, owner))

	key := client.ObjectKeyFromObject(plant)
	require.Eventually(t, func() bool {
		got := &buddyv1alpha1.Plant{}
		err := testClient.Get(testCtx, key, got)
		return client.IgnoreNotFound(err) == nil && err != nil
	}, 10*time.Second, 100*time.Millisecond, "plant %s was never actually deleted", key)
}

// --- case 10: two Plants in the same namespace don't interfere ------------

func TestReconcile_TwoPlantsSameNamespaceDoNotInterfere(t *testing.T) {
	ns := newTestNamespace(t)
	plantA := newTestPlant(ns, "fernie", 3)
	plantB := newTestPlant(ns, "spike", 2)
	createPlant(t, plantA)
	createPlant(t, plantB)

	waitForChildrenExist(t, plantA)
	waitForChildrenExist(t, plantB)

	ownerA := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plantA), ownerA))
	ownerB := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plantB), ownerB))

	deploymentA := &appsv1.Deployment{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, deploymentA))
	assertControllerOwnerRef(t, deploymentA, ownerA)

	deploymentB := &appsv1.Deployment{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "spike"}, deploymentB))
	assertControllerOwnerRef(t, deploymentB, ownerB)

	beforeB := deploymentB.ResourceVersion

	ownerA.Spec.Replicas = int32Ptr(7)
	require.NoError(t, testClient.Update(testCtx, ownerA))

	require.Eventually(t, func() bool {
		d := &appsv1.Deployment{}
		if err := testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, d); err != nil {
			return false
		}
		return d.Spec.Replicas != nil && *d.Spec.Replicas == 7
	}, 10*time.Second, 100*time.Millisecond, "fernie's Deployment never scaled to 7")

	afterB := &appsv1.Deployment{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "spike"}, afterB))
	require.Equal(t, beforeB, afterB.ResourceVersion,
		"spike's Deployment was modified by a reconcile that should only ever have touched fernie's children")
	require.EqualValues(t, 2, *afterB.Spec.Replicas)
}

// --- case 11: condition stability across a real (non-skipped) status write -

// TestReconcile_ConditionLastTransitionTimeStableAcrossRealStatusWrite is
// case 11, and deliberately distinct from the idempotence test above: a
// no-op reconcile trivially "preserves" LastTransitionTime by never writing
// status at all, which doesn't exercise meta.SetStatusCondition's
// preservation logic in the least. This test instead forces a SECOND,
// genuine status write -- changing spec.replicas from 3 to 5, which changes
// status.desiredReplicas -- while every condition's Status and Reason stay
// identical before and after (readyReplicas is 0 both times, so Ready stays
// False/ReplicasNotReady, Progressing stays True/RolloutInProgress, Degraded
// stays True/InsufficientReplicas), and asserts LastTransitionTime is
// unchanged despite a write actually landing.
func TestReconcile_ConditionLastTransitionTimeStableAcrossRealStatusWrite(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "fernie", 3)
	createPlant(t, plant)

	before := waitForStatusPopulated(t, plant)
	require.NotEmpty(t, before.Status.Conditions)
	beforeByType := conditionsByType(before.Status.Conditions)

	fresh := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plant), fresh))
	fresh.Spec.Replicas = int32Ptr(5)
	require.NoError(t, testClient.Update(testCtx, fresh))

	require.Eventually(t, func() bool {
		p := &buddyv1alpha1.Plant{}
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(plant), p); err != nil {
			return false
		}
		return p.Status.DesiredReplicas == 5
	}, 10*time.Second, 100*time.Millisecond, "status.desiredReplicas never advanced to 5 -- no real status write happened to compare against")

	after := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plant), after))
	afterByType := conditionsByType(after.Status.Conditions)

	for condType, beforeCond := range beforeByType {
		afterCond, ok := afterByType[condType]
		require.True(t, ok, "condition %s missing after the status write", condType)
		require.Equal(t, beforeCond.Status, afterCond.Status, "condition %s Status changed unexpectedly", condType)
		require.Equal(t, beforeCond.Reason, afterCond.Reason, "condition %s Reason changed unexpectedly", condType)
		require.True(t, beforeCond.LastTransitionTime.Equal(&afterCond.LastTransitionTime),
			"condition %s LastTransitionTime changed on a status write that did not change its Status: %v -> %v",
			condType, beforeCond.LastTransitionTime, afterCond.LastTransitionTime)
	}
}
