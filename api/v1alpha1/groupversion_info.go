// Package v1alpha1 contains the v1alpha1 API group for K8s Buddy: the Plant
// custom resource and its supporting types. Everything in this package is
// pure data — no client calls, no reconcile logic — built on nothing beyond
// the standard library and k8s.io/apimachinery. It deliberately does not
// import sigs.k8s.io/controller-runtime (whose own scheme.Builder helper
// would otherwise be the obvious choice here): an api package is meant to be
// cheap to import from anywhere, including client tooling that has no
// business linking in a whole controller runtime.
//
// +kubebuilder:object:generate=true
// +groupName=buddy.k8s-buddy.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the API group and version (buddy.k8s-buddy.io/v1alpha1)
	// that every type in this package registers under.
	GroupVersion = schema.GroupVersion{Group: "buddy.k8s-buddy.io", Version: "v1alpha1"}

	// SchemeBuilder collects the functions that add this group-version's
	// types to a runtime.Scheme. addKnownTypes below is its only member.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds every type in this group-version to a given scheme.
	// Callers building a manager or client pass this to a scheme builder
	// alongside clientgoscheme.AddToScheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes registers Plant and PlantList against scheme under
// GroupVersion, and registers the group-version's List options/metadata
// types via metav1.AddToGroupVersion. SchemeBuilder above calls it once,
// through AddToScheme, rather than each type registering itself from its
// own init — one call site to update if a future type is added.
func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, &Plant{}, &PlantList{})
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
