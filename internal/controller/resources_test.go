package controller_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	buddyv1alpha1 "github.com/sean-kramer/k8s-buddy/api/v1alpha1"
	"github.com/sean-kramer/k8s-buddy/internal/controller"
)

// testPlant returns a fully-populated, fully-defaulted Plant, as if the CRD's
// own default markers had already applied. Individual tests mutate a copy of
// its Spec where they need a different value, rather than constructing a
// fresh literal each time.
func testPlant() *buddyv1alpha1.Plant {
	replicas := int32(3)
	return &buddyv1alpha1.Plant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fernie",
			Namespace: "k8s-buddy",
		},
		Spec: buddyv1alpha1.PlantSpec{
			Species:          "fern",
			Replicas:         &replicas,
			Image:            "ghcr.io/sean-kramer/k8s-buddy/buddy-api:dev",
			ResourceProfile:  "small",
			WateringInterval: metav1.Duration{Duration: 30 * time.Second},
			LatencyBudget:    metav1.Duration{Duration: 150 * time.Millisecond},
		},
	}
}

func TestLabelsFor(t *testing.T) {
	t.Parallel()

	p := testPlant()
	got := controller.LabelsFor(p)

	require.Equal(t, map[string]string{
		"app.kubernetes.io/name":       "buddy-api",
		"app.kubernetes.io/instance":   "fernie",
		"app.kubernetes.io/component":  "api",
		"app.kubernetes.io/part-of":    "k8s-buddy",
		"app.kubernetes.io/managed-by": "plant-operator",
		"buddy.k8s-buddy.io/plant":     "fernie",
	}, got)
}

func TestSelectorFor(t *testing.T) {
	t.Parallel()

	p := testPlant()
	got := controller.SelectorFor(p)

	// Exactly the two immutable keys, nothing else -- an extra key here
	// would be a value baked into an immutable selector that this operator
	// could never safely change later.
	require.Len(t, got, 2)
	require.Equal(t, map[string]string{
		"app.kubernetes.io/instance": "fernie",
		"buddy.k8s-buddy.io/plant":   "fernie",
	}, got)
}

func TestSelectorFor_SubsetOfLabelsFor(t *testing.T) {
	t.Parallel()

	p := testPlant()
	labels := controller.LabelsFor(p)
	selector := controller.SelectorFor(p)

	for k, v := range selector {
		require.Equal(t, v, labels[k], "selector key %q must carry the same value in LabelsFor", k)
	}
}

func TestResourcesFor(t *testing.T) {
	t.Parallel()

	small := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
	medium := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	large := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1000m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}

	cases := []struct {
		name    string
		profile string
		want    corev1.ResourceRequirements
	}{
		{"small", "small", small},
		{"medium", "medium", medium},
		{"large", "large", large},
		{"unknown profile falls back to small", "gigantic", small},
		{"empty profile falls back to small", "", small},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := controller.ResourcesFor(tc.profile)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestPodDisruptionBudgetFor_MinAvailable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		replicas int32
		want     int
	}{
		{1, 1}, // floored: replicas-1 would be 0, which must not be permitted
		{2, 1},
		{3, 2},
		{10, 9},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("replicas=%d", tc.replicas), func(t *testing.T) {
			t.Parallel()
			p := testPlant()
			p.Spec.Replicas = &tc.replicas

			pdb := controller.PodDisruptionBudgetFor(p)

			require.Equal(t, "fernie-pdb", pdb.Name)
			require.Equal(t, "k8s-buddy", pdb.Namespace)
			require.NotNil(t, pdb.Spec.MinAvailable)
			require.Equal(t, intstr.FromInt32(int32(tc.want)), *pdb.Spec.MinAvailable)
			require.Equal(t, controller.SelectorFor(p), pdb.Spec.Selector.MatchLabels)
			require.Equal(t, controller.LabelsFor(p), pdb.Labels)
		})
	}
}

func TestServiceFor(t *testing.T) {
	t.Parallel()

	p := testPlant()
	svc := controller.ServiceFor(p)

	require.Equal(t, "fernie", svc.Name)
	require.Equal(t, "k8s-buddy", svc.Namespace)
	require.Equal(t, controller.LabelsFor(p), svc.Labels)
	require.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)
	require.Equal(t, controller.SelectorFor(p), svc.Spec.Selector)
	require.Equal(t, []corev1.ServicePort{
		{
			Name:       "http",
			Port:       80,
			TargetPort: intstr.FromString("http"),
			Protocol:   corev1.ProtocolTCP,
		},
	}, svc.Spec.Ports)
}

func TestConfigMapFor(t *testing.T) {
	t.Parallel()

	p := testPlant()
	p.Spec.Species = "cactus"
	p.Spec.LatencyBudget = metav1.Duration{Duration: 250 * time.Millisecond}
	cm := controller.ConfigMapFor(p)

	require.Equal(t, "fernie", cm.Name)
	require.Equal(t, "k8s-buddy", cm.Namespace)
	require.Equal(t, controller.LabelsFor(p), cm.Labels)

	require.Equal(t, "fernie", cm.Data["BUDDY_NAME"])
	require.Equal(t, "cactus", cm.Data["BUDDY_SPECIES"])
	require.Equal(t, "250ms", cm.Data["BUDDY_LATENCY_BUDGET"])
	require.Equal(t, "info", cm.Data["BUDDY_LOG_LEVEL"])
	require.Equal(t, "0.05", cm.Data["BUDDY_WORK_ERROR_RATE"])
	require.Equal(t, "10ms", cm.Data["BUDDY_WORK_MIN_DELAY"])
	require.Equal(t, "200ms", cm.Data["BUDDY_WORK_MAX_DELAY"])
	require.Equal(t, "false", cm.Data["BUDDY_ENABLE_CHAOS_ENDPOINTS"])
	require.Equal(t, "5s", cm.Data["BUDDY_SHUTDOWN_DELAY"])

	// BUDDY_PORT must never appear: the port is independently pinned in the
	// container port, the Service's targetPort, and all three probes. A
	// ConfigMap key here would let an edit silently desync from all four.
	_, ok := cm.Data["BUDDY_PORT"]
	require.False(t, ok, "ConfigMap must not carry BUDDY_PORT")
}

// TestConfigMapFor_ChaosEndpointsFollowsField asserts
// BUDDY_ENABLE_CHAOS_ENDPOINTS tracks p.Spec.Chaos.EnableEndpoints in BOTH
// states, not just the false default TestConfigMapFor already checks. A
// Plant that leaves spec.chaos unset (testPlant()'s own zero value) must
// still render "false" -- the CRD's own +kubebuilder:default=false on
// ChaosSpec.EnableEndpoints only fills in the field through API-server
// defaulting, so a hand-built Plant that bypasses admission (every test in
// this file) must get the same safe answer from the builder itself.
func TestConfigMapFor_ChaosEndpointsFollowsField(t *testing.T) {
	t.Parallel()

	disabled := testPlant()
	require.Equal(t, "false", controller.ConfigMapFor(disabled).Data["BUDDY_ENABLE_CHAOS_ENDPOINTS"],
		"a Plant that leaves spec.chaos unset must render BUDDY_ENABLE_CHAOS_ENDPOINTS=false")

	enabled := testPlant()
	enabled.Spec.Chaos.EnableEndpoints = true
	require.Equal(t, "true", controller.ConfigMapFor(enabled).Data["BUDDY_ENABLE_CHAOS_ENDPOINTS"],
		"a Plant with spec.chaos.enableEndpoints=true must render BUDDY_ENABLE_CHAOS_ENDPOINTS=true")
}

func TestDeploymentFor(t *testing.T) {
	t.Parallel()

	p := testPlant()
	dep := controller.DeploymentFor(p)

	require.Equal(t, "fernie", dep.Name)
	require.Equal(t, "k8s-buddy", dep.Namespace)
	require.Equal(t, controller.LabelsFor(p), dep.Labels)

	require.NotNil(t, dep.Spec.Replicas)
	require.Equal(t, int32(3), *dep.Spec.Replicas)

	// Selector/template-labels relationship: the selector must be exactly
	// SelectorFor's two keys, and the template's labels must be a superset
	// of it -- if the selector isn't a subset of the template labels,
	// Kubernetes rejects the Deployment outright.
	require.NotNil(t, dep.Spec.Selector)
	require.Equal(t, controller.SelectorFor(p), dep.Spec.Selector.MatchLabels)
	require.Equal(t, controller.LabelsFor(p), dep.Spec.Template.Labels)
	for k, v := range dep.Spec.Selector.MatchLabels {
		require.Equal(t, v, dep.Spec.Template.Labels[k], "template labels must be a superset of the selector")
	}

	podSpec := dep.Spec.Template.Spec

	require.Equal(t, "fernie", podSpec.ServiceAccountName)

	require.NotNil(t, podSpec.AutomountServiceAccountToken)
	require.False(t, *podSpec.AutomountServiceAccountToken)

	require.NotNil(t, podSpec.SecurityContext)
	require.NotNil(t, podSpec.SecurityContext.RunAsNonRoot)
	require.True(t, *podSpec.SecurityContext.RunAsNonRoot)
	require.NotNil(t, podSpec.SecurityContext.RunAsUser)
	require.EqualValues(t, 65532, *podSpec.SecurityContext.RunAsUser)
	require.NotNil(t, podSpec.SecurityContext.SeccompProfile)
	require.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, podSpec.SecurityContext.SeccompProfile.Type)

	require.NotNil(t, podSpec.TerminationGracePeriodSeconds)
	require.EqualValues(t, 30, *podSpec.TerminationGracePeriodSeconds)

	require.Equal(t, []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           1,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector:     &metav1.LabelSelector{MatchLabels: controller.SelectorFor(p)},
		},
	}, podSpec.TopologySpreadConstraints)

	require.Len(t, podSpec.Containers, 1)
	c := podSpec.Containers[0]

	require.Equal(t, "buddy-api", c.Name)
	require.Equal(t, p.Spec.Image, c.Image)
	require.Equal(t, corev1.PullIfNotPresent, c.ImagePullPolicy)

	require.Equal(t, []corev1.ContainerPort{
		{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
	}, c.Ports)

	require.Equal(t, []corev1.EnvFromSource{
		{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "fernie"}}},
	}, c.EnvFrom)

	// No preStop hook: the distroless image has no shell for an exec hook
	// to run in (ADR 0002). The Deployment must not carry one.
	require.Nil(t, c.Lifecycle)

	// Container-level security context, field by field, matching PodSecurity
	// `restricted`.
	require.NotNil(t, c.SecurityContext)
	sc := c.SecurityContext
	require.NotNil(t, sc.RunAsNonRoot)
	require.True(t, *sc.RunAsNonRoot)
	require.NotNil(t, sc.RunAsUser)
	require.EqualValues(t, 65532, *sc.RunAsUser)
	require.NotNil(t, sc.AllowPrivilegeEscalation)
	require.False(t, *sc.AllowPrivilegeEscalation)
	require.NotNil(t, sc.ReadOnlyRootFilesystem)
	require.True(t, *sc.ReadOnlyRootFilesystem)
	require.NotNil(t, sc.Capabilities)
	require.Equal(t, []corev1.Capability{"ALL"}, sc.Capabilities.Drop)
	require.NotNil(t, sc.SeccompProfile)
	require.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, sc.SeccompProfile.Type)

	require.Equal(t, controller.ResourcesFor(p.Spec.ResourceProfile), c.Resources)

	require.Equal(t, &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("http"), Scheme: corev1.URISchemeHTTP},
		},
		TimeoutSeconds:   1,
		PeriodSeconds:    10,
		SuccessThreshold: 1,
		FailureThreshold: 3,
	}, c.LivenessProbe)

	require.Equal(t, &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromString("http"), Scheme: corev1.URISchemeHTTP},
		},
		TimeoutSeconds:   1,
		PeriodSeconds:    2,
		SuccessThreshold: 1,
		FailureThreshold: 2,
	}, c.ReadinessProbe)

	require.Equal(t, &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("http"), Scheme: corev1.URISchemeHTTP},
		},
		TimeoutSeconds:   1,
		PeriodSeconds:    2,
		SuccessThreshold: 1,
		FailureThreshold: 15,
	}, c.StartupProbe)
}

// TestDeploymentFor_ProbesMatchServerDefaults is a regression test for a
// real bug caught in Task 3 review: the API server defaults an HTTPGet
// probe's TimeoutSeconds, SuccessThreshold, and HTTPGet.Scheme to 1, 1, and
// "HTTP" respectively whenever they're left unset. Task 3's reconciler
// diffs this builder's output against the live cluster object on every
// reconcile; if these three fields were ever left zero-valued here again,
// the comparison would never match a server-defaulted object and every
// single reconcile of every Plant would issue an unnecessary update --
// silently reintroducing the write-storm bug the fix in DeploymentFor's own
// probe comment describes. This test exists so a future edit that drops
// one of these fields fails loudly instead of merely regressing
// idempotence in a way only Task 4's write-counting envtest would catch.
func TestDeploymentFor_ProbesMatchServerDefaults(t *testing.T) {
	t.Parallel()

	p := testPlant()
	dep := controller.DeploymentFor(p)
	c := dep.Spec.Template.Spec.Containers[0]

	for _, probe := range []*corev1.Probe{c.LivenessProbe, c.ReadinessProbe, c.StartupProbe} {
		require.NotNil(t, probe)
		require.EqualValues(t, 1, probe.TimeoutSeconds, "TimeoutSeconds must be set explicitly to match the API server's default")
		require.EqualValues(t, 1, probe.SuccessThreshold, "SuccessThreshold must be set explicitly to match the API server's default")
		require.NotNil(t, probe.HTTPGet)
		require.Equal(t, corev1.URISchemeHTTP, probe.HTTPGet.Scheme, "HTTPGet.Scheme must be set explicitly to match the API server's default")
	}
}

func TestDeploymentFor_ResourceProfilePropagates(t *testing.T) {
	t.Parallel()

	p := testPlant()
	p.Spec.ResourceProfile = "large"
	dep := controller.DeploymentFor(p)

	require.Equal(t, controller.ResourcesFor("large"), dep.Spec.Template.Spec.Containers[0].Resources)
}

func TestDeploymentFor_NilReplicas(t *testing.T) {
	t.Parallel()

	p := testPlant()
	p.Spec.Replicas = nil

	require.NotPanics(t, func() {
		dep := controller.DeploymentFor(p)
		require.NotNil(t, dep.Spec.Replicas)
		require.Equal(t, int32(3), *dep.Spec.Replicas)
	})

	pdb := controller.PodDisruptionBudgetFor(p)
	require.Equal(t, intstr.FromInt32(2), *pdb.Spec.MinAvailable)
}

// TestDeterminism protects Task 3's reconciler from an infinite reconcile
// loop: if a builder ever produced different output for the same input --
// through unsorted map iteration, a stray clock read, or similar -- the
// reconciler's drift detection would flag a difference that isn't real and
// write to the API server on every single reconcile.
func TestDeterminism(t *testing.T) {
	t.Parallel()

	p := testPlant()

	require.Equal(t, controller.DeploymentFor(p), controller.DeploymentFor(p))
	require.Equal(t, controller.ServiceFor(p), controller.ServiceFor(p))
	require.Equal(t, controller.ConfigMapFor(p), controller.ConfigMapFor(p))
	require.Equal(t, controller.PodDisruptionBudgetFor(p), controller.PodDisruptionBudgetFor(p))
	require.Equal(t, controller.ServiceAccountFor(p), controller.ServiceAccountFor(p))
	require.Equal(t, controller.NetworkPolicyFor(p), controller.NetworkPolicyFor(p))
	require.Equal(t, controller.LabelsFor(p), controller.LabelsFor(p))
	require.Equal(t, controller.SelectorFor(p), controller.SelectorFor(p))

	// Same check again with spec.chaos.enableEndpoints set: ChaosSpec is a
	// new field on PlantSpec, and ConfigMapFor is the one builder whose
	// output actually varies with it (see
	// TestConfigMapFor_ChaosEndpointsFollowsField) -- so it is the one
	// builder this determinism guard would fail to protect if it only ever
	// exercised the zero-valued Chaos field above.
	chaosEnabled := testPlant()
	chaosEnabled.Spec.Chaos.EnableEndpoints = true
	require.Equal(t, controller.ConfigMapFor(chaosEnabled), controller.ConfigMapFor(chaosEnabled))
}

// TestNetworkPolicyFor is the sixth child. Plan 1's static path gets Pod
// Security Admission's `restricted` profile AND a default-deny NetworkPolicy;
// before this builder existed, a Plant got neither, which made
// operator-managed workloads strictly LESS constrained than the static ones
// they were meant to supersede -- while the project's own spec claimed the
// security posture applied "to every workload without exception".
func TestNetworkPolicyFor(t *testing.T) {
	t.Parallel()

	p := testPlant()
	np := controller.NetworkPolicyFor(p)

	require.Equal(t, "fernie", np.Name)
	require.Equal(t, "k8s-buddy", np.Namespace)
	require.Equal(t, controller.LabelsFor(p), np.Labels)

	// Scoped to this Plant's own pods, NOT `podSelector: {}`. A
	// namespace-wide policy would be a claim over every pod in the
	// namespace, including other Plants' -- two Plants would fight over an
	// identical object forever, and deleting either would tear down the
	// other's default-deny with it.
	require.Equal(t, controller.SelectorFor(p), np.Spec.PodSelector.MatchLabels)

	// BOTH policy types. A NetworkPolicy is deny-by-default only for the
	// directions it declares; omitting Egress here would leave outbound
	// traffic completely unrestricted while looking like a lockdown.
	require.ElementsMatch(t,
		[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		np.Spec.PolicyTypes)

	// Ingress: exactly one rule, TCP 8080, no From. The missing From is
	// deliberate and is the subject of ADR 0003 -- kube-proxy SNATs NodePort
	// traffic to the node IP, so a podSelector-scoped From black-holes the
	// demo's only external traffic path.
	require.Len(t, np.Spec.Ingress, 1)
	require.Empty(t, np.Spec.Ingress[0].From, "ADR 0003: an unqualified ingress rule is load-bearing here, not an oversight")
	require.Len(t, np.Spec.Ingress[0].Ports, 1)
	require.Equal(t, corev1.ProtocolTCP, *np.Spec.Ingress[0].Ports[0].Protocol)
	require.Equal(t, intstr.FromInt32(8080), *np.Spec.Ingress[0].Ports[0].Port)

	// Egress: DNS to kube-system only, on UDP AND TCP. TCP/53 is not
	// optional -- Go's resolver falls back to it for truncated responses, so
	// a UDP-only hole produces intermittent, size-dependent resolution
	// failures rather than a clean break.
	require.Len(t, np.Spec.Egress, 1)
	require.Len(t, np.Spec.Egress[0].To, 1)
	require.Equal(t, map[string]string{"kubernetes.io/metadata.name": "kube-system"},
		np.Spec.Egress[0].To[0].NamespaceSelector.MatchLabels)
	require.Len(t, np.Spec.Egress[0].Ports, 2)
	require.Equal(t, corev1.ProtocolUDP, *np.Spec.Egress[0].Ports[0].Protocol)
	require.Equal(t, intstr.FromInt32(53), *np.Spec.Egress[0].Ports[0].Port)
	require.Equal(t, corev1.ProtocolTCP, *np.Spec.Egress[0].Ports[1].Protocol)
	require.Equal(t, intstr.FromInt32(53), *np.Spec.Egress[0].Ports[1].Port)
}

// TestServiceAccountFor is the fifth child (carried from Task 2's review):
// named after the Plant, in its namespace, standard labels, and no
// automounted token -- the workload's DeploymentFor references this exact
// object by name (see TestDeploymentFor's ServiceAccountName assertion), so
// the two must agree.
func TestServiceAccountFor(t *testing.T) {
	t.Parallel()

	p := testPlant()
	sa := controller.ServiceAccountFor(p)

	require.Equal(t, "fernie", sa.Name)
	require.Equal(t, "k8s-buddy", sa.Namespace)
	require.Equal(t, controller.LabelsFor(p), sa.Labels)
	require.NotNil(t, sa.AutomountServiceAccountToken)
	require.False(t, *sa.AutomountServiceAccountToken)
}
