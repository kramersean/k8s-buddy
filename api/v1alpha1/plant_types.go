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
	//
	// The CEL rule below is additive, not a replacement for Minimum=1: it
	// changes nothing about which values are accepted (Minimum=1 already
	// rejects 0 on its own), it only attaches this project's own
	// human-readable message to that specific rejection. Structural schema
	// validation (both the Minimum keyword and this CEL rule) always runs
	// before any admission webhook -- see api/v1alpha1/plant_webhook.go's
	// own validate() comment and docs/adr/0009 -- so a plain `Minimum=1`
	// alone would surface only the API server's generic "should be greater
	// than or equal to 1" text, never PlantCustomValidator's own
	// "plants need at least one leaf", for a bare `kubectl apply` of
	// `replicas: 0`. Both messages now appear together in the rejection (the
	// API server aggregates every failing rule into one response); Task 4's
	// webhook re-asserts the same rule for defense in depth exactly the way
	// it re-asserts the wateringInterval floor, for the same reason -- see
	// that file's own comment.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:validation:XValidation:rule="self >= 1",message="plants need at least one leaf"
	// +kubebuilder:default=3
	Replicas *int32 `json:"replicas,omitempty"`

	// Image is the buddy-api container image the generated Deployment runs,
	// including its tag.
	//
	// The Pattern accepts the shape of a real image reference — an optional
	// registry host with an optional port, slash-separated path components,
	// an optional tag, and an optional @sha256 digest — and nothing else. It
	// is not a full distribution-spec grammar and does not try to be; its job
	// is to stop `image: ""`, `image: "my image"`, and a pasted YAML block
	// from becoming a Deployment that the kubelet rejects at pull time, when
	// the only symptom is ImagePullBackOff on a Plant whose spec looked fine.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:validation:Pattern="^[a-zA-Z0-9][a-zA-Z0-9._-]*(:[0-9]+)?(/[a-zA-Z0-9][a-zA-Z0-9._-]*)*(:[a-zA-Z0-9][a-zA-Z0-9._-]*)?(@sha256:[a-f0-9]{64})?$"
	// +kubebuilder:default="ghcr.io/kramersean/k8s-buddy/buddy-api:dev"
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
	//
	// Bounded at the API level, in two layers, because an unbounded requeue
	// interval is a denial-of-service knob wearing a friendly name:
	// `wateringInterval: 1ms` was previously a perfectly valid Plant that
	// pinned a reconcile worker in a 1ms loop against the API server for as
	// long as the object existed.
	//
	//   - The Pattern admits only whole seconds, minutes, and hours (`45s`,
	//     `5m`, `1h30m`). Milliseconds, microseconds, and nanoseconds are not
	//     expressible at all, so the pathological case is rejected by the
	//     schema itself with a message a user can act on.
	//   - The CEL rule then bounds the parsed value on both ends: at least
	//     30s (the operator's own minRequeueInterval floor, so an admitted
	//     Plant is never silently clamped to something other than what it
	//     asked for) and at most 24h (beyond which "watering" has stopped
	//     meaning anything and a stale status looks like a broken operator).
	//
	// internal/controller's minRequeueInterval remains the second line of
	// defence, for a Plant constructed directly in Go that never passed
	// through admission at all.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Pattern="^([0-9]+(s|m|h))+$"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('30s')",message="wateringInterval must be at least 30s"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('24h')",message="wateringInterval must be at most 24h"
	// +kubebuilder:default="30s"
	WateringInterval metav1.Duration `json:"wateringInterval,omitempty"`

	// LatencyBudget is passed to the workload as BUDDY_LATENCY_BUDGET: the
	// p95 request latency at or above which the workload's own health
	// scoring bottoms out its latency component at zero.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:default="150ms"
	LatencyBudget metav1.Duration `json:"latencyBudget,omitempty"`

	// Chaos configures optional chaos-injection affordances for this Plant.
	// +optional
	Chaos ChaosSpec `json:"chaos,omitempty"`
}

// ChaosSpec controls whether this Plant exposes chaos-injection endpoints.
type ChaosSpec struct {
	// EnableEndpoints exposes POST /chaos/readiness on this Plant's pods so
	// chaos-buddy's readiness-flap mode can reach them. Defaults to false:
	// a chaos endpoint reachable in an ordinary deployment is a security
	// finding, so it is opt-in per Plant rather than on by default.
	// +kubebuilder:default=false
	EnableEndpoints bool `json:"enableEndpoints,omitempty"`
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

	// LastWatered is the timestamp of the most recent reconcile that
	// changed status. The operator computes a fresh value on every
	// reconcile, but only writes status — including this field — through
	// the status subresource when something other than the timestamp
	// itself actually changed, so a Plant with nothing new to report does
	// not receive a write merely because a WateringInterval elapsed.
	LastWatered *metav1.Time `json:"lastWatered,omitempty"`

	// Conditions are the standard Kubernetes conditions describing this
	// Plant's Ready, Progressing, and Degraded state, managed with
	// meta.SetStatusCondition so transition timestamps stay accurate.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// The scale subresource makes `kubectl scale plant fernie --replicas=5` (and
// `kubectl autoscale`, and any HPA pointed at a Plant) work against a Plant
// exactly the way they work against a Deployment, rather than requiring
// `kubectl patch` or `kubectl edit` and a reviewer knowing that spec.replicas
// happens to be the field to reach for.
//
// selectorpath is deliberately EMPTY. It is only consulted by an HPA doing
// CPU/memory-based autoscaling, which needs a label selector to find the Pods
// whose metrics it should read — and this project has no metrics-server and
// no HPA (both are Plan 3, see ADR 0008). Pointing it at a path now would be
// declaring support for something that cannot work; `kubectl scale` needs
// only specpath and statuspath. When Plan 3 adds the HPA, this gains
// selectorpath=.status.selector and PlantStatus gains a string field
// carrying SelectorFor's serialized form.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.readyReplicas,selectorpath=
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
