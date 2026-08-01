// Package controller implements the plant-operator's reconciliation logic:
// turning a Plant into the Deployment, Service, ConfigMap,
// PodDisruptionBudget, and ServiceAccount it should own, and keeping those
// children in sync with its spec.
//
// This file (resources.go) contains only the "desired state" builders: pure
// functions from a *Plant to the child object it implies. There is no
// client.Client, no context.Context, no clock read, and no randomness
// anywhere below — given the same Plant, every builder returns
// deeply-equal output on every call. That purity is deliberate: Task 3's
// reconciler diffs a builder's output against the live cluster object to
// detect drift, and Task 4's envtest suite (and this file's own tests) can
// exercise every rule without a client or a running API server. Owner
// references are the one thing intentionally left out — see DeploymentFor's
// comment for why.
package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	buddyv1alpha1 "github.com/sean-kramer/k8s-buddy/api/v1alpha1"
)

// The five standard app.kubernetes.io labels applied to every child this
// operator generates, plus the buddy.k8s-buddy.io/plant key that identifies
// which Plant owns the object. See LabelsFor and SelectorFor for how they
// combine, and the operator plan's Global Constraints for why these exact
// keys and values are binding.
const (
	LabelName      = "app.kubernetes.io/name"
	LabelInstance  = "app.kubernetes.io/instance"
	LabelComponent = "app.kubernetes.io/component"
	LabelPartOf    = "app.kubernetes.io/part-of"
	LabelManagedBy = "app.kubernetes.io/managed-by"

	// LabelPlant identifies which Plant a child object (or Pod) belongs to.
	// Combined with LabelInstance, it forms SelectorFor's immutable
	// selector.
	LabelPlant = "buddy.k8s-buddy.io/plant"
)

// Fixed label values: every Plant this operator manages runs the same
// buddy-api workload as the same logical component of the same larger
// project, so these three never vary per Plant. Only LabelInstance and
// LabelPlant (both set to the Plant's own name) differ from one Plant's
// children to another's.
const (
	appName      = "buddy-api"
	appComponent = "api"
	appPartOf    = "k8s-buddy"
	appManagedBy = "plant-operator"
)

// Fixed shape of the generated pod spec: the workload's container name and
// port, and the security/timing constants Plan 1's hardened deployment.yaml
// established. Kept as named constants so the handful of "8080" and "http"
// literals below (container port, probe ports, Service targetPort) can
// never drift out of sync with each other by a typo.
const (
	containerName     = "buddy-api"
	containerPortName = "http"
	containerPort     = 8080

	runAsUser = 65532

	// terminationGracePeriodSeconds must stay comfortably above buddy-api's
	// own shutdown sequence (BUDDY_SHUTDOWN_DELAY, default 5s, plus up to a
	// 15s connection-drain timeout) or the kubelet SIGKILLs the process
	// mid-drain. See docs/adr/0002.
	terminationGracePeriodSeconds = 30

	// defaultReplicas mirrors the CRD's own +kubebuilder:default=3 marker
	// on Plant.spec.replicas. It only matters for a hand-constructed Plant
	// that bypasses API-server defaulting (see replicasFor).
	defaultReplicas = 3
)

// LabelsFor returns the full label set applied to every child resource this
// operator generates for p, and to the Deployment's own pod template: the
// five standard app.kubernetes.io labels plus buddy.k8s-buddy.io/plant
// identifying p. It is always a strict superset of SelectorFor(p)'s keys —
// that containment is what lets the Deployment's pod template legally
// satisfy its own selector (Kubernetes requires a Deployment's selector to
// match its template's labels, or the object is rejected outright).
func LabelsFor(p *buddyv1alpha1.Plant) map[string]string {
	return map[string]string{
		LabelName:      appName,
		LabelInstance:  p.Name,
		LabelComponent: appComponent,
		LabelPartOf:    appPartOf,
		LabelManagedBy: appManagedBy,
		LabelPlant:     p.Name,
	}
}

// SelectorFor returns the two-key label subset used to select a Plant's own
// Pods: app.kubernetes.io/instance and buddy.k8s-buddy.io/plant, both set to
// p's name. Kubernetes treats a Deployment's spec.selector (and a Service's
// spec.selector, and a PDB's spec.selector) as immutable after creation, so
// this set deliberately excludes every other key in LabelsFor — name,
// component, part-of, managed-by are identical across every Plant this
// operator manages and would never help disambiguate one Plant's Pods from
// another's, so including them would only be a value the selector could
// never safely change later without recreating the object it's attached to.
// Two keys, both permanently tied to identity, is what keeps the selector
// itself permanently valid.
func SelectorFor(p *buddyv1alpha1.Plant) map[string]string {
	return map[string]string{
		LabelInstance: p.Name,
		LabelPlant:    p.Name,
	}
}

// replicasFor returns p's desired replica count, defaulting to 3 when
// Replicas is nil. The CRD's +kubebuilder:default=3 marker normally fills
// this in before the operator ever observes the object through the API
// server, but Task 4 (and this package's own tests) construct Plants
// directly in Go without going through API-server defaulting — a builder
// that panics on that input is a bad building block, so this stays
// defensive rather than assuming the pointer is always set.
func replicasFor(p *buddyv1alpha1.Plant) int32 {
	if p.Spec.Replicas == nil {
		return defaultReplicas
	}
	return *p.Spec.Replicas
}

// DeploymentFor builds the Deployment a Plant should own: the hardened,
// probed, topology-spread buddy-api workload described by Plan 1's
// deploy/kustomize/base/deployment.yaml, reproduced here in Go so the
// operator can reconcile it instead of applying a static manifest. It is
// named exactly p.Name, in p.Namespace.
//
// DeploymentFor does not set an owner reference on the object it returns.
// Doing so correctly requires calling controllerutil.SetControllerReference
// with a *runtime.Scheme, and threading a scheme through here would make
// this function's output depend on more than just p — the one thing every
// builder in this file must not do. The reconciler sets ownership
// afterward, once it actually has a scheme to call it with; see Task 3.
func DeploymentFor(p *buddyv1alpha1.Plant) *appsv1.Deployment {
	labels := LabelsFor(p)
	selector := SelectorFor(p)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicasFor(p)),
			// Set once at creation and immutable thereafter — Kubernetes
			// rejects any later change to spec.selector — so this
			// deliberately uses only SelectorFor's two keys. See
			// SelectorFor's doc comment.
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					// The full label set, a strict superset of
					// Selector.MatchLabels above. Kubernetes requires the
					// selector to match the template's labels; anything
					// less than a superset here makes the Deployment
					// invalid.
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					// Named explicitly after the Plant (ServiceAccountFor
					// builds the object itself) rather than left unset,
					// which would silently bind these Pods to the
					// namespace's own "default" ServiceAccount instead.
					// AutomountServiceAccountToken: false below still
					// means no token is ever mounted — these workload
					// pods call nothing in the Kubernetes API — but
					// naming the account is what makes that a stated
					// posture rather than an inherited default.
					ServiceAccountName: p.Name,
					// No token for these workload pods to use — they call
					// nothing in the Kubernetes API.
					AutomountServiceAccountToken: ptr.To(false),
					// Pod Security Admission's `restricted` profile checks
					// for seccompProfile at both pod and container scope;
					// setting it only on the container below would leave
					// the pod itself without a default profile.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true),
						RunAsUser:    ptr.To[int64](runAsUser),
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					// Spread this Plant's replicas across nodes so a
					// single node failure can never take out more than
					// about 1/N of them. ScheduleAnyway (not
					// DoNotSchedule) means the scheduler still places
					// every replica even when perfect spread isn't
					// achievable — e.g. fewer nodes than replicas, or a
					// Plant with more replicas than the cluster has
					// nodes — because this is a demo constraint, not a
					// guarantee the scheduler should refuse to place pods
					// under.
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
						{
							MaxSkew:           1,
							TopologyKey:       "kubernetes.io/hostname",
							WhenUnsatisfiable: corev1.ScheduleAnyway,
							LabelSelector:     &metav1.LabelSelector{MatchLabels: selector},
						},
					},
					// No lifecycle.preStop hook. buddy-api's distroless
					// base image (see ADR 0005) has no shell, so an exec
					// preStop would fail on every termination rather than
					// run; an httpGet variant would run but buys nothing,
					// since it can't extend the grace period below. The
					// delay between "not ready" and "stops accepting
					// connections" instead runs in-process, driven by
					// BUDDY_SHUTDOWN_DELAY — see docs/adr/0002.
					// terminationGracePeriodSeconds below gives that
					// in-process sequence (5s default delay + up to 15s
					// drain + margin) room to finish before the kubelet
					// sends SIGKILL.
					TerminationGracePeriodSeconds: ptr.To[int64](terminationGracePeriodSeconds),
					Containers: []corev1.Container{
						{
							Name:  containerName,
							Image: p.Spec.Image,
							// IfNotPresent, not Always: these images are
							// loaded directly into the demo cluster's node
							// containerd and are never pushed anywhere the
							// configured image reference could actually
							// resolve. Always would make every pod fail
							// its pull.
							ImagePullPolicy: corev1.PullIfNotPresent,
							Ports: []corev1.ContainerPort{
								{
									Name:          containerPortName,
									ContainerPort: containerPort,
									Protocol:      corev1.ProtocolTCP,
								},
							},
							// Config, not secrets: every value the
							// container needs beyond the image itself
							// comes from the generated ConfigMap.
							EnvFrom: []corev1.EnvFromSource{
								{
									ConfigMapRef: &corev1.ConfigMapEnvSource{
										LocalObjectReference: corev1.LocalObjectReference{Name: p.Name},
									},
								},
							},
							SecurityContext: &corev1.SecurityContext{
								RunAsNonRoot:             ptr.To(true),
								RunAsUser:                ptr.To[int64](runAsUser),
								AllowPrivilegeEscalation: ptr.To(false),
								ReadOnlyRootFilesystem:   ptr.To(true),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
								},
								SeccompProfile: &corev1.SeccompProfile{
									Type: corev1.SeccompProfileTypeRuntimeDefault,
								},
							},
							Resources: ResourcesFor(p.Spec.ResourceProfile),
							// TimeoutSeconds: 1, SuccessThreshold: 1, and
							// HTTPGet.Scheme: URISchemeHTTP are set
							// explicitly on all three probes below even
							// though they equal what the API server would
							// default anyway if left unset. Task 3's
							// reconciler diffs this builder's output against
							// the live cluster object on every reconcile; if
							// these fields were left unset here, the desired
							// probe would never equal the server's own
							// (defaulted) stored probe, and the reconciler
							// would treat that permanent mismatch as drift
							// to correct on every single pass — an
							// unnecessary write, forever. Matching the
							// server's defaults exactly is what keeps an
							// unchanged Plant idempotent.
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/healthz",
										Port:   intstr.FromString(containerPortName),
										Scheme: corev1.URISchemeHTTP,
									},
								},
								TimeoutSeconds:   1,
								PeriodSeconds:    10,
								SuccessThreshold: 1,
								FailureThreshold: 3,
							},
							// periodSeconds: 2 / failureThreshold: 2 here
							// (tighter than the liveness probe above)
							// surfaces a Pod falling out of, or back into,
							// Service rotation within ~4s instead of the
							// 30s a default probe cadence would take —
							// the difference between a demo that's
							// watchable in real time and one that needs
							// narrating.
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/readyz",
										Port:   intstr.FromString(containerPortName),
										Scheme: corev1.URISchemeHTTP,
									},
								},
								TimeoutSeconds:   1,
								PeriodSeconds:    2,
								SuccessThreshold: 1,
								FailureThreshold: 2,
							},
							StartupProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/healthz",
										Port:   intstr.FromString(containerPortName),
										Scheme: corev1.URISchemeHTTP,
									},
								},
								TimeoutSeconds:   1,
								PeriodSeconds:    2,
								SuccessThreshold: 1,
								FailureThreshold: 15,
							},
						},
					},
				},
			},
		},
	}
}

// ServiceAccountFor builds the ServiceAccount a Plant's Pods run under: named
// exactly p.Name, in p.Namespace, with AutomountServiceAccountToken set to
// false. It does not set an owner reference — see DeploymentFor's comment for
// why.
//
// The workload gets zero API access either way — DeploymentFor's pod spec
// also sets AutomountServiceAccountToken: false, so no token is mounted even
// if this ServiceAccount's own setting were somehow bypassed — but naming the
// account explicitly, rather than leaving DeploymentFor's pod spec to
// silently inherit the namespace's "default" ServiceAccount, is the point:
// this manifest states its posture ("this workload has been given its own
// identity, and that identity carries no token") instead of leaving a reader
// to infer it from an absence. See config/rbac/service_account.yaml's own
// comment for the contrasting operator-pod posture, which does need its
// token mounted.
func ServiceAccountFor(p *buddyv1alpha1.Plant) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    LabelsFor(p),
		},
		AutomountServiceAccountToken: ptr.To(false),
	}
}

// ServiceFor builds the ClusterIP Service that fronts a Plant's Pods: port
// 80, forwarded to the container's named "http" port (containerPort 8080).
// It is named exactly p.Name, in p.Namespace, and does not set an owner
// reference — see DeploymentFor's comment for why.
func ServiceFor(p *buddyv1alpha1.Plant) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    LabelsFor(p),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: SelectorFor(p),
			Ports: []corev1.ServicePort{
				{
					Name:       containerPortName,
					Port:       80,
					TargetPort: intstr.FromString(containerPortName),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

// ConfigMapFor builds the non-secret runtime configuration the generated
// Deployment consumes via envFrom: Plan 1's buddy-api-config defaults, with
// BUDDY_NAME, BUDDY_SPECIES, and BUDDY_LATENCY_BUDGET filled in from p.
// BUDDY_LATENCY_BUDGET is rendered with time.Duration's own String method
// (e.g. "150ms"), the same format the field already carries as a
// metav1.Duration. It is named exactly p.Name, in p.Namespace, and does not
// set an owner reference — see DeploymentFor's comment for why.
//
// BUDDY_PORT is deliberately absent, mirroring Plan 1's ConfigMap exactly.
// The port is independently pinned in four other places in this file's
// output — the container's ContainerPort, the Service's targetPort, and all
// three probes' Port — none of which read this ConfigMap. Adding BUDDY_PORT
// here would advertise a tunable that, if edited, moves the workload to a
// new port while the container port, the Service, and every probe keep
// targeting 8080 — a ConfigMap edit that silently breaks the Plant instead
// of changing anything it claims to control. cmd/buddy-api still defaults
// its own listen port to 8080 independently of this ConfigMap.
func ConfigMapFor(p *buddyv1alpha1.Plant) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    LabelsFor(p),
		},
		Data: map[string]string{
			"BUDDY_NAME":                   p.Name,
			"BUDDY_SPECIES":                p.Spec.Species,
			"BUDDY_LOG_LEVEL":              "info",
			"BUDDY_LATENCY_BUDGET":         p.Spec.LatencyBudget.Duration.String(),
			"BUDDY_WORK_ERROR_RATE":        "0.05",
			"BUDDY_WORK_MIN_DELAY":         "10ms",
			"BUDDY_WORK_MAX_DELAY":         "200ms",
			"BUDDY_ENABLE_CHAOS_ENDPOINTS": "false",
			"BUDDY_SHUTDOWN_DELAY":         "5s",
		},
	}
}

// PodDisruptionBudgetFor builds the PDB that bounds voluntary disruption of
// a Plant's Pods: minAvailable = replicas - 1, floored at 1. It is named
// p.Name + "-pdb", in p.Namespace, and does not set an owner reference — see
// DeploymentFor's comment for why.
//
// The floor at 1 looks like an off-by-one until you notice what it prevents:
// at replicas=1, replicas-1 is 0, and minAvailable: 0 would let Kubernetes
// voluntarily evict a single-replica Plant's only Pod — permitting exactly
// the outage a PDB exists to block. Flooring at 1 instead means a
// single-replica Plant blocks all voluntary disruption of its one Pod,
// which is the correct behavior for something with no spare capacity to
// give up.
func PodDisruptionBudgetFor(p *buddyv1alpha1.Plant) *policyv1.PodDisruptionBudget {
	minAvailable := replicasFor(p) - 1
	if minAvailable < 1 {
		minAvailable = 1
	}

	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name + "-pdb",
			Namespace: p.Namespace,
			Labels:    LabelsFor(p),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr.To(intstr.FromInt32(minAvailable)),
			Selector: &metav1.LabelSelector{
				MatchLabels: SelectorFor(p),
			},
		},
	}
}

// ResourcesFor returns the CPU/memory requests and limits for a resource
// profile — small, medium, or large — per the table in the operator plan's
// Global Constraints. Any other value, including an empty string (which a
// hand-constructed Plant may carry before the CRD's own default marker has
// ever applied), falls back to small rather than an empty
// ResourceRequirements{}: a container with no requests set is
// unschedulable-by-surprise — the scheduler treats it as needing nothing
// and may pack it onto a node with no real headroom — which would silently
// violate this project's stated resource posture rather than loudly
// rejecting an invalid profile.
func ResourcesFor(profile string) corev1.ResourceRequirements {
	switch profile {
	case "medium":
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		}
	case "large":
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("250m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1000m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		}
	default: // "small", "", and any unrecognized value
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("200m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		}
	}
}
