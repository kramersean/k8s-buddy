// This file (cache.go) holds the ONE definition of how the plant-operator's
// informer cache is scoped. Both the deployed binary
// (cmd/plant-operator/main.go) and the envtest suite (suite_test.go) build
// their manager from it.
//
// That sharing is not tidiness, it is the fix for a specific defect. The
// envtest suite used to construct its manager with no Cache option at all,
// so it cached everything unfiltered while the deployed operator cached only
// labelled objects — and a bug whose entire mechanism was "the cache cannot
// see this object" was therefore invisible to the repo's highest-signal test
// suite. A label-strip test written against that suite passed against a build
// that was demonstrably broken on a real cluster.
//
// Copying the selector into the test instead would have reproduced the
// problem on a delay: two definitions drift, and the drift is precisely what
// makes the suite stop resembling production. There is one function, and both
// callers call it.
//
// See resources.go for the package's own doc comment.

package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CacheOptions restricts what the manager's informer cache actually holds for
// each of the six child types PlantReconciler owns, to just those objects
// carrying app.kubernetes.io/managed-by=plant-operator.
//
// Without it, SetupWithManager's Owns(&corev1.ConfigMap{}) and
// Owns(&corev1.ServiceAccount{}) mean the operator establishes a cluster-wide
// LIST+WATCH on every ConfigMap and every ServiceAccount in the cluster and
// holds them all in memory — on a real cluster that is thousands of objects,
// including every ConfigMap belonging to every other team, none of which this
// operator will ever reconcile. The ClusterRole granting the read is genuinely
// required (a Plant may be created in any namespace, so the operator cannot be
// namespace-scoped); the memory footprint and the watch traffic are not.
//
// Two properties make this safe rather than merely smaller:
//
//   - Every child this operator creates carries the label, set by LabelsFor on
//     every build and re-asserted by mergeLabels on every reconcile. A child
//     cannot leave the filter through normal operation.
//   - Plant itself is deliberately ABSENT from ByObject. The filter applies
//     only to the types listed, so the Plant type keeps its full, unfiltered
//     cache and a Plant carrying no labels at all is still watched and still
//     reconciled. Filtering the primary resource by a label the USER would
//     have to remember to set is how an operator ends up silently ignoring the
//     objects it exists for.
//
// It does have one sharp edge, and the reconciler is built around it rather
// than around wishing it away: an object outside the filter — a foreign object
// of the same name, or one of this operator's own children whose label a human
// has stripped — is INVISIBLE through the cached client. Every read
// PlantReconciler makes about a child therefore goes through the uncached
// APIReader; see applyChild. Read through the cache instead and the operator
// either fails to notice a name collision or, worse, spins forever trying to
// create an object that already exists. Both were observed on a live cluster
// before applyChild existed.
func CacheOptions() cache.Options {
	managed := labels.SelectorFromSet(labels.Set{LabelManagedBy: appManagedBy})
	byLabel := cache.ByObject{Label: managed}

	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&appsv1.Deployment{}:            byLabel,
			&corev1.Service{}:               byLabel,
			&corev1.ConfigMap{}:             byLabel,
			&corev1.ServiceAccount{}:        byLabel,
			&policyv1.PodDisruptionBudget{}: byLabel,
			&networkingv1.NetworkPolicy{}:   byLabel,
		},
	}
}
