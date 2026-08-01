// This file (plant_controller.go) contains the reconciler itself: the only
// place in this package that talks to the Kubernetes API. Everything it
// needs to compute — desired children, desired status — is delegated to the
// pure functions in resources.go and status.go, so the I/O here stays a
// thin, readable loop: fetch, build, apply, write status, requeue.
//
// See resources.go for the package's own doc comment.

package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	buddyv1alpha1 "github.com/sean-kramer/k8s-buddy/api/v1alpha1"
)

// plantFinalizer is added to every Plant this operator observes and removed
// only once cleanup has run, so the API server defers actual deletion of
// the Plant until the operator has had a chance to react to it (see
// Reconcile's deletion branch).
const plantFinalizer = "buddy.k8s-buddy.io/finalizer"

// Event reasons emitted against a Plant. Emitted sparingly — see Reconcile
// for exactly when each fires — because an Event on every no-op reconcile
// is the Event-spam version of the write-loop bug status.go's
// statusChanged guards against: `kubectl describe` on a healthy Plant
// should show a handful of Events across its lifetime, not one per
// WateringInterval forever.
const (
	eventPlantCreated   = "PlantCreated"
	eventPlantUpdated   = "PlantUpdated"
	eventPlantDegraded  = "PlantDegraded"
	eventPlantRecovered = "PlantRecovered"
	eventPlantDeleting  = "PlantDeleting"
)

// PlantReconciler reconciles a Plant object: it drives the generated
// Deployment, Service, ConfigMap, and PodDisruptionBudget toward the state
// resources.go's builders describe, and reports their aggregate health back
// onto Plant.status.
type PlantReconciler struct {
	client.Client
	// Scheme is used to set the controller owner reference on every child
	// this reconciler creates, so Kubernetes garbage collection can find
	// and remove them when their owning Plant is deleted.
	Scheme *runtime.Scheme
	// Recorder emits the Events described above against the Plant being
	// reconciled.
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=buddy.k8s-buddy.io,resources=plants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=buddy.k8s-buddy.io,resources=plants/status,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=buddy.k8s-buddy.io,resources=plants/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=create;get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=create;get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=create;get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=create;get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives a single Plant toward its desired state. See the file
// comment and this package's task brief for the exact six-step flow;
// summarized inline at each step below.
func (r *PlantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Step 1: fetch the Plant. A NotFound here means it was already
	// deleted (and, since the finalizer is gone by the time that happens,
	// there is nothing left for this reconciler to do) — not an error
	// worth returning or logging as one.
	plant := &buddyv1alpha1.Plant{}
	if err := r.Get(ctx, req.NamespacedName, plant); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching plant %s: %w", req.NamespacedName, err)
	}

	// Step 2: deletion in progress. Kubernetes sets DeletionTimestamp and
	// then blocks the actual delete until every finalizer is removed, so
	// this branch is where the operator gets a guaranteed last chance to
	// react before the Plant disappears.
	if plant.DeletionTimestamp != nil {
		return r.reconcileDelete(ctx, plant)
	}

	// Step 3: ensure the finalizer is present before this reconciler ever
	// creates a child for this Plant. Returning immediately after adding
	// it (rather than falling through to step 4 in the same pass) means
	// every subsequent reconcile — including the very next one — always
	// observes a Plant that already carries the finalizer, which keeps
	// the "does this Plant have owned children yet" question answered by
	// a single, uniform check instead of a first-reconcile special case.
	if !controllerutil.ContainsFinalizer(plant, plantFinalizer) {
		controllerutil.AddFinalizer(plant, plantFinalizer)
		if err := r.Update(ctx, plant); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer to plant %s: %w", req.NamespacedName, err)
		}
		return ctrl.Result{}, nil
	}

	// Step 4: reconcile every owned child toward its desired state.
	deployment, created, updated, err := r.reconcileChildren(ctx, plant)
	if err != nil {
		return ctrl.Result{}, err
	}
	if created {
		r.Recorder.Event(plant, corev1.EventTypeNormal, eventPlantCreated, "created Deployment, Service, ConfigMap, and PodDisruptionBudget")
	} else if updated {
		r.Recorder.Event(plant, corev1.EventTypeNormal, eventPlantUpdated, "corrected drift on one or more owned children")
	}

	// Step 5: recompute status and write it through the status
	// subresource only — never via a plain Update on the main object,
	// which would let the operator race a user's own concurrent edits
	// to spec/metadata instead of touching only the fields it owns.
	if err := r.reconcileStatus(ctx, plant, deployment, log); err != nil {
		return ctrl.Result{}, err
	}

	// Step 6: come back in WateringInterval regardless of whether
	// anything changed this pass, so status — mood, health, readiness —
	// keeps refreshing even when nothing external has changed.
	return ctrl.Result{RequeueAfter: plant.Spec.WateringInterval.Duration}, nil
}

// reconcileDelete runs finalizer cleanup for a Plant whose DeletionTimestamp
// is set: it emits the PlantDeleting event, logs, and removes the
// finalizer so the API server can complete the delete.
//
// It deliberately does NOT delete the Deployment, Service, ConfigMap, or
// PodDisruptionBudget by hand. Every one of them carries a controller owner
// reference back to this Plant (set in reconcileChildren via
// controllerutil.SetControllerReference), and Kubernetes' own garbage
// collector removes owned objects automatically once their owner is gone.
// Deleting them here would be redundant at best; at worst, doing it
// unconditionally on every deletion — including ones that fail partway
// through, or race with the garbage collector — is exactly the kind of
// manual cleanup logic that owner references exist to make unnecessary.
// Trusting garbage collection, not reimplementing it, is the point.
func (r *PlantReconciler) reconcileDelete(ctx context.Context, plant *buddyv1alpha1.Plant) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(plant, plantFinalizer) {
		// Finalizer already removed by a previous pass; nothing left to
		// do while the API server finishes the delete.
		return ctrl.Result{}, nil
	}

	log.Info("plant deletion in progress; owned children will be garbage-collected via owner references", "plant", plant.Name)
	r.Recorder.Event(plant, corev1.EventTypeNormal, eventPlantDeleting, "plant is being deleted; owned children will be garbage-collected")

	controllerutil.RemoveFinalizer(plant, plantFinalizer)
	if err := r.Update(ctx, plant); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer from plant %s/%s: %w", plant.Namespace, plant.Name, err)
	}
	return ctrl.Result{}, nil
}

// reconcileChildren applies the Deployment, Service, ConfigMap, and
// PodDisruptionBudget resources.go's builders describe for plant, setting a
// controller owner reference on each so Kubernetes garbage collection can
// find them later. It returns the (now-current) Deployment for status.go's
// computeStatus to read, plus whether any child was newly created or
// updated so Reconcile can decide which Event, if any, to emit.
//
// Every mutate function below assigns only the fields the operator owns.
// That is deliberate and load-bearing: controllerutil.CreateOrUpdate
// compares the object before and after the mutate function runs, and
// issues a write whenever they differ. A mutate function that copies a
// whole desired Spec over the fetched object would also copy back every
// zero-valued field resources.go's builders never set — an immutable
// selector, a server-assigned ClusterIP, a defaulted
// terminationMessagePath — clobbering values this operator does not own
// and, for the immutable ones, getting the write rejected outright. Copying
// field-by-field instead means an unchanged Plant produces a byte-for-byte
// unchanged child on every pass, and CreateOrUpdate correctly reports
// OperationResultNone instead of a phantom update.
func (r *PlantReconciler) reconcileChildren(ctx context.Context, plant *buddyv1alpha1.Plant) (*appsv1.Deployment, bool, bool, error) {
	created := false
	updated := false

	note := func(op controllerutil.OperationResult) {
		switch op {
		case controllerutil.OperationResultCreated:
			created = true
		case controllerutil.OperationResultUpdated, controllerutil.OperationResultUpdatedStatus, controllerutil.OperationResultUpdatedStatusOnly:
			updated = true
		}
	}

	deployment := &appsv1.Deployment{ObjectMeta: objectMeta(plant.Name, plant.Namespace)}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		return r.mutateDeployment(plant, deployment)
	})
	if err != nil {
		return nil, false, false, fmt.Errorf("reconciling deployment for plant %s/%s: %w", plant.Namespace, plant.Name, err)
	}
	note(op)

	service := &corev1.Service{ObjectMeta: objectMeta(plant.Name, plant.Namespace)}
	op, err = controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		return r.mutateService(plant, service)
	})
	if err != nil {
		return nil, false, false, fmt.Errorf("reconciling service for plant %s/%s: %w", plant.Namespace, plant.Name, err)
	}
	note(op)

	configMap := &corev1.ConfigMap{ObjectMeta: objectMeta(plant.Name, plant.Namespace)}
	op, err = controllerutil.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		return r.mutateConfigMap(plant, configMap)
	})
	if err != nil {
		return nil, false, false, fmt.Errorf("reconciling configmap for plant %s/%s: %w", plant.Namespace, plant.Name, err)
	}
	note(op)

	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: objectMeta(plant.Name+"-pdb", plant.Namespace)}
	op, err = controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		return r.mutatePodDisruptionBudget(plant, pdb)
	})
	if err != nil {
		return nil, false, false, fmt.Errorf("reconciling poddisruptionbudget for plant %s/%s: %w", plant.Namespace, plant.Name, err)
	}
	note(op)

	return deployment, created, updated, nil
}

// objectMeta returns the minimal ObjectMeta CreateOrUpdate needs before its
// initial Get: just enough identity (name, namespace) to look the object up
// or create it. Everything else is filled in by the corresponding mutate*
// function.
func objectMeta(name, namespace string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace}
}

// mutateDeployment sets deployment's fields to those DeploymentFor(plant)
// describes, owning only the fields the operator manages.
//
// deployment.Spec.Selector is set only when nil — i.e. only on create.
// Kubernetes rejects any change to a Deployment's spec.selector after
// creation, so assigning it unconditionally would work on the first
// reconcile and then fail every one after a Plant's replicas or any other
// field changed and CreateOrUpdate issued its next Update call. See
// SelectorFor's own comment in resources.go for why the selector's two
// keys never change for a given Plant across its lifetime, which is what
// makes "set once, never touch again" correct rather than merely
// convenient.
func (r *PlantReconciler) mutateDeployment(plant *buddyv1alpha1.Plant, deployment *appsv1.Deployment) error {
	desired := DeploymentFor(plant)

	if err := controllerutil.SetControllerReference(plant, deployment, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on deployment: %w", err)
	}

	deployment.Labels = desired.Labels

	if deployment.Spec.Selector == nil {
		deployment.Spec.Selector = desired.Spec.Selector
	}
	deployment.Spec.Replicas = desired.Spec.Replicas
	deployment.Spec.Template.Labels = desired.Spec.Template.Labels

	podSpec := &deployment.Spec.Template.Spec
	desiredPodSpec := desired.Spec.Template.Spec
	podSpec.AutomountServiceAccountToken = desiredPodSpec.AutomountServiceAccountToken
	podSpec.SecurityContext = desiredPodSpec.SecurityContext
	podSpec.TopologySpreadConstraints = desiredPodSpec.TopologySpreadConstraints
	podSpec.TerminationGracePeriodSeconds = desiredPodSpec.TerminationGracePeriodSeconds
	podSpec.Containers = mergeContainers(podSpec.Containers, desiredPodSpec.Containers)

	return nil
}

// mergeContainers applies desired's fields onto existing's containers,
// matched by name, leaving any container-level field neither builder sets
// (e.g. TerminationMessagePath, TerminationMessagePolicy — both defaulted
// by the API server) untouched on a container that already exists. A
// container present in desired but not yet in existing (the create path,
// or a hypothetical future second container) is appended as-is: there is
// nothing previously defaulted to preserve for a container the API server
// has never seen.
func mergeContainers(existing, desired []corev1.Container) []corev1.Container {
	byName := make(map[string]int, len(existing))
	for i, c := range existing {
		byName[c.Name] = i
	}

	merged := make([]corev1.Container, 0, len(desired))
	for _, want := range desired {
		if i, ok := byName[want.Name]; ok {
			have := existing[i]
			have.Image = want.Image
			have.ImagePullPolicy = want.ImagePullPolicy
			have.Ports = want.Ports
			have.EnvFrom = want.EnvFrom
			have.SecurityContext = want.SecurityContext
			have.Resources = want.Resources
			have.LivenessProbe = want.LivenessProbe
			have.ReadinessProbe = want.ReadinessProbe
			have.StartupProbe = want.StartupProbe
			merged = append(merged, have)
			continue
		}
		merged = append(merged, want)
	}
	return merged
}

// mutateService sets service's fields to those ServiceFor(plant) describes,
// owning Type, Selector, Ports, and Labels only. ClusterIP, ClusterIPs,
// IPFamilies, IPFamilyPolicy, and SessionAffinity are left untouched: the
// API server assigns or defaults every one of them, and ServiceFor never
// sets them, so copying a whole ServiceSpec over the fetched object would
// null out an already-assigned ClusterIP on every single reconcile —
// exactly the phantom-drift bug this operator guards against.
func (r *PlantReconciler) mutateService(plant *buddyv1alpha1.Plant, service *corev1.Service) error {
	desired := ServiceFor(plant)

	if err := controllerutil.SetControllerReference(plant, service, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on service: %w", err)
	}

	service.Labels = desired.Labels
	service.Spec.Type = desired.Spec.Type
	service.Spec.Selector = desired.Spec.Selector
	service.Spec.Ports = desired.Spec.Ports

	return nil
}

// mutateConfigMap sets configMap's fields to those ConfigMapFor(plant)
// describes. Unlike the Deployment and Service, a ConfigMap has no
// server-defaulted fields for this operator's builder to accidentally
// clobber, so Data and Labels are safe to assign wholesale.
func (r *PlantReconciler) mutateConfigMap(plant *buddyv1alpha1.Plant, configMap *corev1.ConfigMap) error {
	desired := ConfigMapFor(plant)

	if err := controllerutil.SetControllerReference(plant, configMap, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on configmap: %w", err)
	}

	configMap.Labels = desired.Labels
	configMap.Data = desired.Data

	return nil
}

// mutatePodDisruptionBudget sets pdb's fields to those
// PodDisruptionBudgetFor(plant) describes, owning MinAvailable and Labels.
// Selector is set only when nil (create-only), matching mutateDeployment's
// reasoning: a PDB's spec.selector is immutable after creation.
func (r *PlantReconciler) mutatePodDisruptionBudget(plant *buddyv1alpha1.Plant, pdb *policyv1.PodDisruptionBudget) error {
	desired := PodDisruptionBudgetFor(plant)

	if err := controllerutil.SetControllerReference(plant, pdb, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on poddisruptionbudget: %w", err)
	}

	pdb.Labels = desired.Labels
	pdb.Spec.MinAvailable = desired.Spec.MinAvailable
	if pdb.Spec.Selector == nil {
		pdb.Spec.Selector = desired.Spec.Selector
	}

	return nil
}

// reconcileStatus computes plant's new status from deployment and writes it
// through the status subresource, but only when something other than
// LastWatered actually changed.
//
// BUG A guard: see statusChanged's own comment in status.go for the full
// reasoning. In short, LastWatered is excluded from that comparison
// specifically because it changes on every call by construction; if it
// were included, every reconcile would look "changed" and this operator
// would write to the API server every WateringInterval, forever, for every
// Plant it manages — the single most common operator defect. The
// `if !statusChanged(...) { return nil }` below is the line that turns that
// guard into an actual skipped write.
func (r *PlantReconciler) reconcileStatus(ctx context.Context, plant *buddyv1alpha1.Plant, deployment *appsv1.Deployment, log logr.Logger) error {
	oldStatus := plant.Status
	newStatus := computeStatus(plant, deployment)

	if !statusChanged(oldStatus, newStatus) {
		log.V(1).Info("status unchanged; skipping status write", "plant", plant.Name)
		return nil
	}

	r.emitHealthEvents(plant, oldStatus, newStatus)

	plant.Status = newStatus
	if err := r.Status().Update(ctx, plant); err != nil {
		return fmt.Errorf("updating status for plant %s/%s: %w", plant.Namespace, plant.Name, err)
	}
	return nil
}

// emitHealthEvents fires PlantDegraded or PlantRecovered when the Degraded
// condition's Status actually transitions between old and new status —
// never on every reconcile, only on the edges, so a Plant stuck in either
// state produces one Event at the transition rather than one per requeue.
func (r *PlantReconciler) emitHealthEvents(plant *buddyv1alpha1.Plant, oldStatus, newStatus buddyv1alpha1.PlantStatus) {
	wasDegraded := conditionTrue(oldStatus.Conditions, ConditionDegraded)
	isDegraded := conditionTrue(newStatus.Conditions, ConditionDegraded)

	switch {
	case isDegraded && !wasDegraded:
		r.Recorder.Event(plant, corev1.EventTypeWarning, eventPlantDegraded, "plant has no ready replicas")
	case wasDegraded && !isDegraded:
		r.Recorder.Event(plant, corev1.EventTypeNormal, eventPlantRecovered, "plant has at least one ready replica again")
	}
}

// conditionTrue reports whether conditions contains a condition of the
// given type with Status True.
func conditionTrue(conditions []metav1.Condition, conditionType string) bool {
	for _, c := range conditions {
		if c.Type == conditionType {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

// SetupWithManager wires PlantReconciler into mgr: it reconciles on changes
// to Plant itself, plus any change to a Deployment, Service, ConfigMap, or
// PodDisruptionBudget that this reconciler owns, so drift a human (or
// another controller) introduces on a child gets corrected without waiting
// for the next WateringInterval.
func (r *PlantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&buddyv1alpha1.Plant{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("plant").
		Complete(r)
}
