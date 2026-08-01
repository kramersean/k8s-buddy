package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PlantSpec describes the desired plant: what buddy-api workload the
// operator should run, and how it should behave. Every field has a default,
// so an empty PlantSpec{} (i.e. `spec: {}` in a manifest) is a valid,
// fully-defaulted plant.
type PlantSpec struct {
	// Species is the plant variety. Cosmetic: it flows into the workload's
	// BUDDY_SPECIES environment variable and shapes what the plant says
	// about itself in its own /status responses. It has no effect on
	// scheduling, resources, or health.
	// +kubebuilder:validation:Enum=fern;cactus;succulent;orchid;fiddle-leaf
	// +kubebuilder:default=fern
	Species string `json:"species,omitempty"`

	// Replicas is how many leaves this plant grows: the desired replica
	// count for the generated Deployment. It is a pointer so an unset field
	// (use the default of 3) is distinguishable from an explicit 0 — the
	// Minimum marker below makes `replicas: 0` a rejected write rather than
	// a silent scale-to-zero.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=3
	Replicas *int32 `json:"replicas,omitempty"`

	// Image is the buddy-api container image the generated Deployment runs,
	// including its tag.
	// +kubebuilder:default="ghcr.io/sean-kramer/k8s-buddy/buddy-api:dev"
	Image string `json:"image,omitempty"`

	// ResourceProfile selects the CPU and memory envelope applied to the
	// generated container: small, medium, or large. Each profile maps to a
	// fixed requests/limits pair chosen by the operator — this field picks
	// among them rather than setting raw quantities, so a Plant can never
	// end up with requests greater than limits or other malformed
	// combinations.
	// +kubebuilder:validation:Enum=small;medium;large
	// +kubebuilder:default=small
	ResourceProfile string `json:"resourceProfile,omitempty"`

	// WateringInterval is how often the operator re-reconciles this plant
	// even when nothing external has changed, so status — mood, health,
	// readiness — keeps refreshing instead of only updating in reaction to
	// spec edits or child-resource drift.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:default="30s"
	WateringInterval metav1.Duration `json:"wateringInterval,omitempty"`

	// LatencyBudget is passed to the workload as BUDDY_LATENCY_BUDGET: the
	// p95 request latency at or above which the workload's own health
	// scoring bottoms out its latency component at zero.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:default="150ms"
	LatencyBudget metav1.Duration `json:"latencyBudget,omitempty"`
}

// PlantStatus is the observed state of a Plant, computed by the operator on
// every reconcile from the generated Deployment's own status. Unlike
// PlantSpec, nothing here is user-editable through kubectl edit's default
// view — it is only writable through the status subresource.
type PlantStatus struct {
	// Mood is the plant's derived emotional state (e.g. "leafy", "thirsty",
	// "wilting"), computed from HealthPercent using the same mood ladder the
	// buddy-api workload itself uses to describe its own health. Empty
	// until the first successful reconcile has observed the Deployment.
	Mood string `json:"mood,omitempty"`

	// HealthPercent is ReadyReplicas as a percentage of DesiredReplicas,
	// rounded to the nearest whole percent. It is 0 whenever DesiredReplicas
	// is 0, rather than an undefined division.
	HealthPercent int32 `json:"healthPercent"`

	// ReadyReplicas is the number of Pods in the generated Deployment
	// currently reporting Ready, mirrored from the Deployment's own status.
	ReadyReplicas int32 `json:"readyReplicas"`

	// DesiredReplicas is the replica count the operator is currently
	// reconciling towards, mirrored from the generated Deployment's spec
	// (and therefore reflects Plant.spec.replicas once a reconcile has run).
	DesiredReplicas int32 `json:"desiredReplicas"`

	// ObservedGeneration is the value of .metadata.generation the operator
	// last acted on. Compare it against the Plant's own .metadata.generation
	// to tell whether the rest of this status reflects the most recent spec
	// change or an older one still being processed.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastWatered is the timestamp of the most recent successful reconcile,
	// updated whether or not that reconcile changed anything.
	LastWatered *metav1.Time `json:"lastWatered,omitempty"`

	// Conditions are the standard Kubernetes conditions describing this
	// Plant's Ready, Progressing, and Degraded state, managed with
	// meta.SetStatusCondition so transition timestamps stay accurate.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pl,categories=all
// +kubebuilder:printcolumn:name="Species",type="string",JSONPath=".spec.species"
// +kubebuilder:printcolumn:name="Mood",type="string",JSONPath=".status.mood"
// +kubebuilder:printcolumn:name="Health",type="integer",JSONPath=".status.healthPercent"
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Desired",type="integer",JSONPath=".status.desiredReplicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Plant is the Schema for the plants API. A Plant is a self-contained,
// self-reporting buddy-api workload: the plant-operator watches it and
// continuously reconciles a Deployment, Service, ConfigMap, and
// PodDisruptionBudget to match its spec, then reports the workload's
// aggregate health back onto Plant.status — turning a plain Deployment into
// a small demonstration of the Kubernetes control plane.
type Plant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the desired state of the plant, as set by its owner.
	Spec PlantSpec `json:"spec,omitempty"`
	// Status is the most recently observed state of the plant, as computed
	// by the operator. Consumers should treat it as read-only.
	Status PlantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PlantList contains a list of Plant. It exists so client-go's generated
// (and controller-runtime's dynamic) list operations have a concrete type to
// deserialize into.
type PlantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Plant `json:"items"`
}
