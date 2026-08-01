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
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	buddyv1alpha1 "github.com/sean-kramer/k8s-buddy/api/v1alpha1"
)

// --- case 1: creation owns all six children -----------------------------

func TestReconcile_CreatesAllSixChildrenWithOwnerReferences(t *testing.T) {
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

	serviceAccount := &corev1.ServiceAccount{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, serviceAccount))
	assertControllerOwnerRef(t, serviceAccount, owner)
	require.NotNil(t, serviceAccount.AutomountServiceAccountToken)
	require.False(t, *serviceAccount.AutomountServiceAccountToken)

	// The sixth child. Without it a Plant runs in a namespace with no
	// NetworkPolicy of its own, which left operator-managed pods strictly
	// less constrained than Plan 1's static ones.
	networkPolicy := &networkingv1.NetworkPolicy{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, networkPolicy))
	assertControllerOwnerRef(t, networkPolicy, owner)
	require.ElementsMatch(t,
		[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		networkPolicy.Spec.PolicyTypes,
		"both policy types must be declared, or the undeclared direction stays wide open")
	require.Equal(t, SelectorFor(owner), networkPolicy.Spec.PodSelector.MatchLabels,
		"the policy must select only this Plant's own pods, never the whole namespace")
}

// --- case 1b: the live Deployment actually runs as the Plant's SA --------

// TestReconcile_LiveDeploymentRunsAsPlantServiceAccount asserts against the
// child Deployment READ BACK FROM THE API SERVER, not against DeploymentFor's
// output.
//
// That distinction is the entire point of this test. DeploymentFor set
// ServiceAccountName from the day it was written, resources_test.go asserted
// it on the builder, and both stayed green for two whole tasks while
// mutateDeployment silently failed to copy the field onto the live object --
// so every Pod on the real cluster ran as the namespace's `default`
// ServiceAccount while the Plant's own ServiceAccount was created, owned, and
// garbage-collected purely for show. A builder assertion structurally cannot
// catch a mutate function that drops a field; only reading the live child can.
func TestReconcile_LiveDeploymentRunsAsPlantServiceAccount(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "fernie", 3)
	createPlant(t, plant)

	waitForChildrenExist(t, plant)

	deployment := &appsv1.Deployment{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, deployment))

	require.Equal(t, plant.Name, deployment.Spec.Template.Spec.ServiceAccountName,
		"the live Deployment's pod template must name the Plant's own ServiceAccount; an empty value "+
			"here means every pod silently runs as the namespace's `default` SA")

	// The account it names must actually exist, or the pods would be
	// unschedulable rather than merely mis-identified.
	serviceAccount := &corev1.ServiceAccount{}
	require.NoError(t, testClient.Get(testCtx,
		client.ObjectKey{Namespace: ns, Name: deployment.Spec.Template.Spec.ServiceAccountName}, serviceAccount))
}

// --- case 2: NO finalizer is ever added ---------------------------------

// TestReconcile_AddsNoFinalizer is the inverse of the test it replaces.
//
// The old behavior added "buddy.k8s-buddy.io/finalizer" to every Plant and
// removed it again on deletion, and that removal path performed no cleanup at
// all -- correctly, because all six children are removed by garbage
// collection via the owner references case 1 asserts. What the finalizer
// actually bought was a Plant that cannot be deleted while the operator is
// down: `kubectl delete plant` returns, the object sits with a
// DeletionTimestamp indefinitely, and the only fix is hand-editing
// metadata.finalizers. Pure availability liability in exchange for nothing.
// See docs/adr/0007-no-finalizer-on-plant.md.
//
// The assertion is deliberately "no finalizers at all" rather than "not that
// specific string": reintroducing the same liability under a new name must
// fail this test too.
func TestReconcile_AddsNoFinalizer(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "fernie", 3)
	createPlant(t, plant)

	// Wait for a reconcile that has definitely run to completion before
	// asserting an absence -- otherwise this would pass trivially against a
	// Plant the operator has not looked at yet.
	waitForChildrenExist(t, plant)
	waitForStatusPopulated(t, plant)
	waitForReconcileQuiescence(t)

	requireNoFinalizers(t, plant)
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
//
// The condition Type and Reason are checked against the literal strings
// "Ready" and "ReplicasNotReady", not the ConditionReady/ReasonReplicasNotReady
// source constants every other test uses: both are user-visible API surface
// the operator plan specifies verbatim, so a test that only ever compares
// against the source constants would keep passing even if those constants'
// VALUES silently drifted.
func TestReconcile_StatusReflectsNotReadyWithNoKubelet(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "fernie", 4)
	createPlant(t, plant)

	got := waitForStatusPopulated(t, plant)

	require.EqualValues(t, 4, got.Status.DesiredReplicas)
	require.EqualValues(t, 0, got.Status.ReadyReplicas)

	conditions := conditionsByType(got.Status.Conditions)
	ready, ok := conditions["Ready"]
	require.True(t, ok, "no Ready condition present")
	require.Equal(t, metav1.ConditionFalse, ready.Status)
	require.Equal(t, "ReplicasNotReady", ready.Reason)
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
// against all six children AND zero status-subresource writes.
func TestReconcile_Idempotence_SteadyStateReconcileWritesNothing(t *testing.T) {
	ns := newTestNamespace(t)
	plant := newTestPlant(ns, "idempotent", 3)
	createPlant(t, plant)

	waitForChildrenExist(t, plant)
	waitForStatusPopulated(t, plant)

	// Quiesce BEFORE resetting the counters, not after: triggerReconcile
	// itself also quiesces first, but a still-settling creation reconcile
	// (finalizer-add, children-create, status-write are three separate
	// Reconcile passes) can still be in flight right up until this point.
	// Resetting first and quiescing second would let that trailing
	// reconcile's writes land AFTER the reset and go uncounted -- exactly
	// the hole this suite exists to close.
	waitForReconcileQuiescence(t)
	testCounting.reset()

	triggerReconcile(t, plant)

	// Sum every GVK the counting client has EVER seen a write against, not
	// just the six expected children: a write storm on the Plant object
	// itself (e.g. a status-condition loop) or a write bucketed under some
	// unexpected GVK would be invisible to a loop that only inspects
	// childGVKs(). The full map is still printed on failure so a non-zero
	// total remains diagnosable.
	writes := testCounting.snapshot()
	var writeTotal int
	for _, wc := range writes {
		writeTotal += wc.total()
	}
	require.Zero(t, writeTotal,
		"expected zero Create/Update/Patch writes anywhere on a steady-state reconcile, got %+v", writes)

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

	serviceAccount := &corev1.ServiceAccount{ObjectMeta: objectMeta(fresh.Name, fresh.Namespace)}
	op, err = controllerutil.CreateOrUpdate(testCtx, testClient, serviceAccount, func() error {
		return reconciler.mutateServiceAccount(fresh, serviceAccount)
	})
	require.NoError(t, err)
	require.Equal(t, controllerutil.OperationResultNone, op, "ServiceAccount CreateOrUpdate must be a no-op at steady state")

	networkPolicy := &networkingv1.NetworkPolicy{ObjectMeta: objectMeta(fresh.Name, fresh.Namespace)}
	op, err = controllerutil.CreateOrUpdate(testCtx, testClient, networkPolicy, func() error {
		return reconciler.mutateNetworkPolicy(fresh, networkPolicy)
	})
	require.NoError(t, err)
	require.Equal(t, controllerutil.OperationResultNone, op, "NetworkPolicy CreateOrUpdate must be a no-op at steady state")
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

	updatePlant(t, client.ObjectKeyFromObject(plant), func(p *buddyv1alpha1.Plant) {
		p.Spec.Replicas = int32Ptr(6)
	})

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

			updatePlant(t, client.ObjectKeyFromObject(plant), func(p *buddyv1alpha1.Plant) {
				p.Spec.ResourceProfile = tc.profile
			})

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

	fresh := updatePlant(t, client.ObjectKeyFromObject(plant), func(p *buddyv1alpha1.Plant) {
		p.Spec.Replicas = int32Ptr(5)
	})
	require.Greater(t, fresh.Generation, first.Generation, "a spec change must bump metadata.generation")

	require.Eventually(t, func() bool {
		p := &buddyv1alpha1.Plant{}
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(plant), p); err != nil {
			return false
		}
		return p.Generation > first.Generation && p.Status.ObservedGeneration == p.Generation
	}, 10*time.Second, 100*time.Millisecond, "status.observedGeneration never caught up to the new metadata.generation")
}

// --- case 9: deletion is immediate; owner refs are the GC proof ----------

// TestReconcile_DeleteIsImmediateAndOwnerReferencesAreCorrect covers deletion
// honestly: envtest runs no garbage-collector controller, so a Plant's owned
// children are never actually removed here when the Plant is -- only a real
// cluster's garbage collector does that, and it does it by walking exactly
// the owner references this test asserts are correct, on all SIX children,
// before deleting. Asserting the children vanish under envtest would be
// asserting something this suite's control plane cannot demonstrate; CI's
// live-cluster e2e job is where cascading deletion is proven for real.
//
// What this test CAN prove, and now does, is that deletion completes without
// the operator having to do anything: with no finalizer on the object, the
// API server removes the Plant on the delete call itself rather than parking
// it with a DeletionTimestamp until a reconcile gets around to unblocking it.
// The Eventually below used to be waiting for the operator; it now succeeds
// on its first poll.
func TestReconcile_DeleteIsImmediateAndOwnerReferencesAreCorrect(t *testing.T) {
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

	serviceAccount := &corev1.ServiceAccount{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, serviceAccount))
	assertControllerOwnerRef(t, serviceAccount, owner)

	networkPolicy := &networkingv1.NetworkPolicy{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, networkPolicy))
	assertControllerOwnerRef(t, networkPolicy, owner)

	require.NoError(t, testClient.Delete(testCtx, owner))

	// No Eventually: with no finalizer to block it, the delete above has
	// already completed by the time it returns. A require.Eventually here
	// would still pass if a finalizer were reintroduced and the operator
	// happened to be up to remove it -- which is exactly the availability
	// dependency ADR 0007 removes, so the assertion is deliberately the
	// stricter, non-polling one.
	key := client.ObjectKeyFromObject(plant)
	got := &buddyv1alpha1.Plant{}
	require.True(t, apierrors.IsNotFound(testClient.Get(testCtx, key, got)),
		"plant %s must be gone the moment Delete returns; a Plant that lingers means something is holding a finalizer", key)
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

	// Non-interference is proven with the counting client, not a
	// resourceVersion comparison -- counting_client_test.go's own doc
	// comment is why resourceVersion is the wrong tool: a byte-identical
	// write would leave it unchanged even though a write happened. Both
	// Plants are quiesced first, then only fernie's spec is changed, so
	// exactly ONE write to the Deployment GVK (fernie's own) is expected;
	// two or more would mean spike's Deployment was touched too, which is
	// exactly the leak this test exists to catch.
	waitForReconcileQuiescence(t)
	testCounting.reset()

	updatePlant(t, client.ObjectKeyFromObject(plantA), func(p *buddyv1alpha1.Plant) {
		p.Spec.Replicas = int32Ptr(7)
	})

	require.Eventually(t, func() bool {
		d := &appsv1.Deployment{}
		if err := testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "fernie"}, d); err != nil {
			return false
		}
		return d.Spec.Replicas != nil && *d.Spec.Replicas == 7
	}, 10*time.Second, 100*time.Millisecond, "fernie's Deployment never scaled to 7")
	waitForReconcileQuiescence(t)

	deploymentGVK := appsv1.SchemeGroupVersion.WithKind("Deployment")
	writes := testCounting.snapshot()
	require.Equal(t, 1, writes[deploymentGVK].total(),
		"expected exactly one Deployment write (fernie's own) while only fernie's spec changed, got %+v -- "+
			"more than one would mean spike's Deployment was written too", writes[deploymentGVK])

	afterB := &appsv1.Deployment{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKey{Namespace: ns, Name: "spike"}, afterB))
	require.EqualValues(t, 2, *afterB.Spec.Replicas, "spike's Deployment replicas must be untouched by fernie's reconcile")
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

	updatePlant(t, client.ObjectKeyFromObject(plant), func(p *buddyv1alpha1.Plant) {
		p.Spec.Replicas = int32Ptr(5)
	})

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
