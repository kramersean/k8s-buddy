// This file is the drift test between K8s Buddy's two descriptions of the
// same workload.
//
// The repo describes buddy-api twice, on purpose:
//
//   - deploy/kustomize/base/*.yaml -- static manifests. Plan 1's demo, and
//     the fallback path that works with nothing running but kubectl.
//   - internal/controller/resources.go -- Go builders. The operator's
//     desired state, and the headline of the project.
//
// docs/adr/0006-operator-reproduces-base-manifests.md records why both exist
// and stay. This test is the mechanism that ADR promises: the two agree on
// every value they are supposed to agree on today, and nothing but a test
// stops them drifting apart tomorrow -- a reviewer who tightens a probe or a
// resource limit in one place and not the other would otherwise get a green
// build and two subtly different workloads.
//
// The two also differ, legitimately, in a handful of places. Every one of
// those differences is asserted EXPLICITLY below, with the reason, rather
// than skipped: an unintended change to a field that is "supposed to be
// different" still fails this test, because the test pins what the
// difference IS, not merely that one exists.
package controller_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	buddyv1alpha1 "github.com/sean-kramer/k8s-buddy/api/v1alpha1"
	"github.com/sean-kramer/k8s-buddy/internal/controller"
)

// baseManifestDir is deploy/kustomize/base, relative to this package's own
// directory (go test runs with the package directory as the working
// directory).
const baseManifestDir = "../../deploy/kustomize/base"

// decodeManifest reads one YAML file from deploy/kustomize/base and decodes
// the document at index into out.
//
// It reads the RAW manifest rather than running it through `kubectl
// kustomize` on purpose: the kustomization applies an images: transformer and
// two common labels, neither of which is what this test is comparing, and
// shelling out to kubectl would make a pure unit test depend on a binary
// being installed. The consequence -- that the image tag and the two
// kustomize-applied labels are absent here -- is handled explicitly at each
// assertion that would otherwise care.
func decodeManifest(t *testing.T, file string, index int, out any) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(baseManifestDir, file))
	require.NoError(t, err, "reading %s", file)

	// The multi-document files in this directory (service.yaml,
	// networkpolicy.yaml) use a bare `---` separator at column 0, always on
	// its own line. Splitting on that is sufficient here and keeps the test
	// free of a YAML streaming decoder for two files.
	docs := strings.Split(string(raw), "\n---\n")
	require.Greater(t, len(docs), index, "%s has no document at index %d", file, index)

	require.NoError(t, yaml.Unmarshal([]byte(docs[index]), out), "decoding %s document %d", file, index)
}

// driftTestPlant returns the Plant whose generated children are compared
// against the static manifests: three replicas and the "small" resource
// profile, which is exactly what deploy/kustomize/base declares.
//
// The Plant is named buddy-api, matching the static manifests' object names,
// so the comparison isolates real differences instead of drowning in
// name-derived ones. That is also, not coincidentally, the exact Plant that
// would have seized these very manifests before the adoption guard existed;
// see TestReconcile_RefusesToAdoptForeignDeployment.
func driftTestPlant() *buddyv1alpha1.Plant {
	p := testPlant()
	p.Name = "buddy-api"
	p.Namespace = "k8s-buddy"
	return p
}

// --- Deployment ----------------------------------------------------------

func TestDrift_DeploymentSecurityContextsMatch(t *testing.T) {
	t.Parallel()

	var static appsv1.Deployment
	decodeManifest(t, "deployment.yaml", 0, &static)
	generated := controller.DeploymentFor(driftTestPlant())

	require.Equal(t, static.Spec.Template.Spec.SecurityContext, generated.Spec.Template.Spec.SecurityContext,
		"pod-level securityContext must be identical in both descriptions of buddy-api")

	require.Len(t, static.Spec.Template.Spec.Containers, 1)
	require.Len(t, generated.Spec.Template.Spec.Containers, 1)
	require.Equal(t,
		static.Spec.Template.Spec.Containers[0].SecurityContext,
		generated.Spec.Template.Spec.Containers[0].SecurityContext,
		"container-level securityContext must be identical in both descriptions of buddy-api")

	require.Equal(t, static.Spec.Template.Spec.AutomountServiceAccountToken,
		generated.Spec.Template.Spec.AutomountServiceAccountToken)
}

func TestDrift_DeploymentResourcesMatch(t *testing.T) {
	t.Parallel()

	var static appsv1.Deployment
	decodeManifest(t, "deployment.yaml", 0, &static)
	generated := controller.DeploymentFor(driftTestPlant())

	staticResources := static.Spec.Template.Spec.Containers[0].Resources
	generatedResources := generated.Spec.Template.Spec.Containers[0].Resources

	// Quantity carries unexported formatting state, so compare the values
	// rather than the structs: 200m parsed from YAML and 200m parsed from a
	// Go string literal are equal numbers with different internal caches.
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		wantReq := staticResources.Requests[name]
		gotReq := generatedResources.Requests[name]
		require.True(t, wantReq.Equal(gotReq), "requests.%s: static %s != generated %s", name, wantReq.String(), gotReq.String())

		wantLim := staticResources.Limits[name]
		gotLim := generatedResources.Limits[name]
		require.True(t, wantLim.Equal(gotLim), "limits.%s: static %s != generated %s", name, wantLim.String(), gotLim.String())
	}

	// The static manifest is the "small" profile. If ResourcesFor's small
	// tier ever changes without deployment.yaml changing, the loop above
	// fails -- this assertion just names which tier the static file is
	// supposed to be, so the failure is diagnosable.
	small := controller.ResourcesFor("small")
	smallCPU := small.Requests[corev1.ResourceCPU]
	staticCPU := staticResources.Requests[corev1.ResourceCPU]
	require.True(t, smallCPU.Equal(staticCPU), "deploy/kustomize/base is expected to declare the `small` resource profile")
}

func TestDrift_DeploymentProbeTimingsMatch(t *testing.T) {
	t.Parallel()

	var static appsv1.Deployment
	decodeManifest(t, "deployment.yaml", 0, &static)
	generated := controller.DeploymentFor(driftTestPlant())

	staticContainer := static.Spec.Template.Spec.Containers[0]
	generatedContainer := generated.Spec.Template.Spec.Containers[0]

	probes := []struct {
		name      string
		static    *corev1.Probe
		generated *corev1.Probe
	}{
		{"liveness", staticContainer.LivenessProbe, generatedContainer.LivenessProbe},
		{"readiness", staticContainer.ReadinessProbe, generatedContainer.ReadinessProbe},
		{"startup", staticContainer.StartupProbe, generatedContainer.StartupProbe},
	}

	for _, p := range probes {
		require.NotNil(t, p.static, "%s probe missing from the static manifest", p.name)
		require.NotNil(t, p.generated, "%s probe missing from the generated Deployment", p.name)

		require.Equal(t, p.static.HTTPGet.Path, p.generated.HTTPGet.Path, "%s probe path", p.name)
		require.Equal(t, p.static.HTTPGet.Port, p.generated.HTTPGet.Port, "%s probe port", p.name)
		require.Equal(t, p.static.PeriodSeconds, p.generated.PeriodSeconds, "%s probe periodSeconds", p.name)
		require.Equal(t, p.static.FailureThreshold, p.generated.FailureThreshold, "%s probe failureThreshold", p.name)

		// LEGITIMATE DIFFERENCE, asserted rather than skipped. The static
		// manifest omits timeoutSeconds, successThreshold, and
		// httpGet.scheme and lets the API server default them; the Go
		// builder sets all three explicitly to the SAME values the API
		// server would default them to. That is not redundancy -- the
		// reconciler diffs the builder's output against the live object on
		// every pass, and a field left unset in Go would never equal the
		// server's stored (defaulted) value, producing a permanent phantom
		// diff and a write on every single reconcile. See DeploymentFor's
		// own comment.
		require.Zero(t, p.static.TimeoutSeconds, "%s probe: the static manifest is expected to leave timeoutSeconds defaulted", p.name)
		require.Zero(t, p.static.SuccessThreshold, "%s probe: the static manifest is expected to leave successThreshold defaulted", p.name)
		require.Empty(t, p.static.HTTPGet.Scheme, "%s probe: the static manifest is expected to leave scheme defaulted", p.name)

		require.EqualValues(t, 1, p.generated.TimeoutSeconds, "%s probe: the builder must state the server default explicitly", p.name)
		require.EqualValues(t, 1, p.generated.SuccessThreshold, "%s probe: the builder must state the server default explicitly", p.name)
		require.Equal(t, corev1.URISchemeHTTP, p.generated.HTTPGet.Scheme, "%s probe: the builder must state the server default explicitly", p.name)
	}
}

func TestDrift_DeploymentPodShapeMatches(t *testing.T) {
	t.Parallel()

	var static appsv1.Deployment
	decodeManifest(t, "deployment.yaml", 0, &static)
	generated := controller.DeploymentFor(driftTestPlant())

	require.Equal(t, static.Spec.Template.Spec.TerminationGracePeriodSeconds,
		generated.Spec.Template.Spec.TerminationGracePeriodSeconds,
		"terminationGracePeriodSeconds is load-bearing for the in-process shutdown sequence (ADR 0002) and must match")

	require.Equal(t, static.Spec.Template.Spec.Containers[0].ImagePullPolicy,
		generated.Spec.Template.Spec.Containers[0].ImagePullPolicy,
		"both paths load images straight into kind's containerd, so both must be IfNotPresent")

	require.Equal(t, static.Spec.Template.Spec.Containers[0].Name,
		generated.Spec.Template.Spec.Containers[0].Name)
	require.Equal(t, static.Spec.Template.Spec.Containers[0].Ports,
		generated.Spec.Template.Spec.Containers[0].Ports)

	require.Equal(t, static.Spec.Replicas, generated.Spec.Replicas,
		"the static manifest's replica count and this drift test's Plant must agree, or the comparison is between two different workloads")

	// topologySpreadConstraints: everything except the label selector must
	// match. The selector differs for the same reason spec.selector does --
	// see the next assertion.
	require.Len(t, static.Spec.Template.Spec.TopologySpreadConstraints, 1)
	require.Len(t, generated.Spec.Template.Spec.TopologySpreadConstraints, 1)
	staticSpread := static.Spec.Template.Spec.TopologySpreadConstraints[0]
	generatedSpread := generated.Spec.Template.Spec.TopologySpreadConstraints[0]
	require.Equal(t, staticSpread.MaxSkew, generatedSpread.MaxSkew)
	require.Equal(t, staticSpread.TopologyKey, generatedSpread.TopologyKey)
	require.Equal(t, staticSpread.WhenUnsatisfiable, generatedSpread.WhenUnsatisfiable)
}

// TestDrift_DeploymentDifferencesAreDeliberate pins the places the two
// descriptions are SUPPOSED to disagree. Each assertion states what the
// difference is, so that changing either side unintentionally still fails.
func TestDrift_DeploymentDifferencesAreDeliberate(t *testing.T) {
	t.Parallel()

	var static appsv1.Deployment
	decodeManifest(t, "deployment.yaml", 0, &static)
	plant := driftTestPlant()
	generated := controller.DeploymentFor(plant)

	// DIFFERENCE 1 -- selector cardinality. The static manifest selects on
	// three keys (name, instance, component); the operator selects on two
	// (instance, plant). A Deployment's spec.selector is immutable after
	// creation, and the operator's two keys are both permanently tied to a
	// Plant's identity, while `component` is identical across every Plant it
	// will ever manage and could never disambiguate one Plant's pods from
	// another's. See SelectorFor.
	require.Len(t, static.Spec.Selector.MatchLabels, 3, "the static manifest is expected to select on three keys")
	require.Len(t, generated.Spec.Selector.MatchLabels, 2, "the operator is expected to select on exactly two identity keys")
	require.Equal(t, controller.SelectorFor(plant), generated.Spec.Selector.MatchLabels)

	// DIFFERENCE 2 -- managed-by. Both carry all five standard labels, but
	// with different values for who manages the object, which is the point:
	// `kubectl get -l app.kubernetes.io/managed-by=plant-operator` must
	// return operator-managed objects and nothing else. That label is also
	// the selector cmd/plant-operator narrows its informer cache with.
	require.Equal(t, "kustomize", static.Labels["app.kubernetes.io/managed-by"])
	require.Equal(t, controller.ManagedByValue, generated.Labels["app.kubernetes.io/managed-by"])

	// DIFFERENCE 3 -- serviceAccountName. Both name a dedicated, token-free
	// ServiceAccount rather than inheriting `default`; the static one is
	// fixed at buddy-api and the operator's is the Plant's own name, because
	// each Plant gets an identity of its own. What must NOT differ is that
	// the field is set at all: an empty value here means every pod silently
	// runs as the namespace's default ServiceAccount, which is exactly the
	// regression the Plan 2 review found.
	require.Equal(t, "buddy-api", static.Spec.Template.Spec.ServiceAccountName)
	require.Equal(t, plant.Name, generated.Spec.Template.Spec.ServiceAccountName)
	require.NotEmpty(t, generated.Spec.Template.Spec.ServiceAccountName)

	// DIFFERENCE 4 -- the image tag. The static manifest deliberately
	// carries the repository only; its tag comes from the kustomization's
	// images: transformer, which `make deploy` overrides with an immutable
	// git SHA. The operator takes the image from spec.image, whose CRD
	// default carries a tag.
	require.Equal(t, "ghcr.io/sean-kramer/k8s-buddy/buddy-api", static.Spec.Template.Spec.Containers[0].Image,
		"the static manifest must stay untagged; the tag comes from the kustomize images: transformer")
	require.Equal(t, plant.Spec.Image, generated.Spec.Template.Spec.Containers[0].Image)

	// DIFFERENCE 5 -- envFrom. Both take every non-secret setting from a
	// ConfigMap via envFrom, but the static one is a fixed
	// `buddy-api-config` while the operator generates one named after the
	// Plant.
	require.Equal(t, "buddy-api-config",
		static.Spec.Template.Spec.Containers[0].EnvFrom[0].ConfigMapRef.Name)
	require.Equal(t, plant.Name,
		generated.Spec.Template.Spec.Containers[0].EnvFrom[0].ConfigMapRef.Name)
}

// --- ConfigMap -----------------------------------------------------------

func TestDrift_ConfigMapKeysAndStaticValuesMatch(t *testing.T) {
	t.Parallel()

	var static corev1.ConfigMap
	decodeManifest(t, "configmap.yaml", 0, &static)
	generated := controller.ConfigMapFor(driftTestPlant())

	staticKeys := make([]string, 0, len(static.Data))
	for k := range static.Data {
		staticKeys = append(staticKeys, k)
	}
	generatedKeys := make([]string, 0, len(generated.Data))
	for k := range generated.Data {
		generatedKeys = append(generatedKeys, k)
	}
	require.ElementsMatch(t, staticKeys, generatedKeys,
		"both descriptions must configure buddy-api with exactly the same set of environment variables")

	// BUDDY_PORT must be absent from BOTH. Adding it would advertise a knob
	// that, when turned, moves the server to a new port while the container
	// port, the Service targetPort, and all three probes keep targeting
	// 8080 -- a ConfigMap edit that breaks the workload instead of
	// configuring it.
	require.NotContains(t, staticKeys, "BUDDY_PORT")
	require.NotContains(t, generatedKeys, "BUDDY_PORT")

	// The keys neither side derives from a Plant must carry identical
	// values. The three that ARE derived (BUDDY_NAME, BUDDY_SPECIES,
	// BUDDY_LATENCY_BUDGET) are checked separately below.
	for _, key := range []string{
		"BUDDY_LOG_LEVEL",
		"BUDDY_WORK_ERROR_RATE",
		"BUDDY_WORK_MIN_DELAY",
		"BUDDY_WORK_MAX_DELAY",
		"BUDDY_ENABLE_CHAOS_ENDPOINTS",
		"BUDDY_SHUTDOWN_DELAY",
	} {
		require.Equal(t, static.Data[key], generated.Data[key], "%s must match between the two descriptions", key)
	}
}

// TestDrift_ConfigMapDifferencesAreDeliberate pins the three keys the
// operator derives from the Plant rather than hardcoding. The static
// manifest's values happen to describe fernie, which is why they read as
// identical to a casual glance -- this asserts the operator computes them.
func TestDrift_ConfigMapDifferencesAreDeliberate(t *testing.T) {
	t.Parallel()

	plant := driftTestPlant()
	plant.Spec.Species = "cactus"
	generated := controller.ConfigMapFor(plant)

	require.Equal(t, plant.Name, generated.Data["BUDDY_NAME"])
	require.Equal(t, "cactus", generated.Data["BUDDY_SPECIES"])
	require.Equal(t, plant.Spec.LatencyBudget.Duration.String(), generated.Data["BUDDY_LATENCY_BUDGET"])
}

// --- Service -------------------------------------------------------------

func TestDrift_ClusterIPServiceMatches(t *testing.T) {
	t.Parallel()

	// Document 0 of service.yaml is the ClusterIP Service; document 1 is the
	// NodePort one, which has no operator counterpart (see below).
	var static corev1.Service
	decodeManifest(t, "service.yaml", 0, &static)
	generated := controller.ServiceFor(driftTestPlant())

	require.Equal(t, corev1.ServiceTypeClusterIP, static.Spec.Type)
	require.Equal(t, static.Spec.Type, generated.Spec.Type)
	require.Equal(t, static.Spec.Ports, generated.Spec.Ports,
		"port 80 -> the container's named `http` port, in both descriptions")
}

// TestDrift_NodePortServiceHasNoOperatorCounterpart is a legitimate
// difference, asserted explicitly.
//
// The static path ships a second, NodePort Service fixed at 30080 to match
// the extraPortMappings in deploy/kind/kind-config.yaml, so hack/demo.sh and
// a host-side curl can reach it with no port-forward. The operator
// deliberately does NOT generate one: a nodePort is a cluster-wide singleton,
// so every Plant creating one at 30080 would mean the second Plant on a
// cluster fails to create its Service forever, and letting the API server
// allocate a random one instead would produce a port nothing knows in
// advance. Plants are reached over their ClusterIP Service from inside the
// cluster.
func TestDrift_NodePortServiceHasNoOperatorCounterpart(t *testing.T) {
	t.Parallel()

	var nodePort corev1.Service
	decodeManifest(t, "service.yaml", 1, &nodePort)

	require.Equal(t, "buddy-api-nodeport", nodePort.Name)
	require.Equal(t, corev1.ServiceTypeNodePort, nodePort.Spec.Type)
	require.EqualValues(t, 30080, nodePort.Spec.Ports[0].NodePort,
		"30080 must stay in lockstep with deploy/kind/kind-config.yaml's extraPortMappings")

	// The operator's only Service is a ClusterIP, with no nodePort at all.
	generated := controller.ServiceFor(driftTestPlant())
	require.Equal(t, corev1.ServiceTypeClusterIP, generated.Spec.Type)
	require.Zero(t, generated.Spec.Ports[0].NodePort)
}

// --- PodDisruptionBudget -------------------------------------------------

func TestDrift_PodDisruptionBudgetMatches(t *testing.T) {
	t.Parallel()

	var static policyv1.PodDisruptionBudget
	decodeManifest(t, "pdb.yaml", 0, &static)
	generated := controller.PodDisruptionBudgetFor(driftTestPlant())

	require.Equal(t, static.Spec.MinAvailable, generated.Spec.MinAvailable,
		"at 3 replicas both descriptions must allow exactly one voluntary disruption at a time")
	require.Equal(t, intstr.FromInt32(2), *generated.Spec.MinAvailable)

	// LEGITIMATE DIFFERENCE: the object name. The static PDB is named
	// buddy-api (same as the Deployment); the operator suffixes "-pdb"
	// because a Plant's Deployment, Service, ConfigMap, ServiceAccount, and
	// NetworkPolicy all already take the Plant's bare name, and a PDB in the
	// same namespace would be the only one of the six to collide with
	// nothing -- suffixing keeps the naming rule uniform instead of
	// depending on which kinds happen to share a namespace-scoped name.
	require.Equal(t, "buddy-api", static.Name)
	require.Equal(t, "buddy-api-pdb", generated.Name)
}

// --- NetworkPolicy -------------------------------------------------------

// TestDrift_NetworkPolicyShapesDiffer is the largest legitimate difference
// and therefore the one most worth pinning.
//
// The static path uses THREE namespace-wide policies: default-deny-all,
// allow-dns-egress, allow-http-ingress, each with `podSelector: {}`. The
// operator generates ONE policy scoped to a single Plant's pods. The reason
// is ownership, not taste: a `podSelector: {}` policy is a claim over every
// pod in the namespace, so two Plants sharing a namespace would each own an
// identical namespace-wide object and fight over it forever, and deleting
// either Plant would tear down the other's default-deny along with it. A
// NetworkPolicy is deny-by-default for every pod it selects, so scoping to
// the Plant costs nothing.
//
// What must NOT differ is the posture itself: both deny everything by
// default, both open DNS egress to kube-system on UDP and TCP 53, and both
// open TCP 8080 ingress with no `from:` selector (ADR 0003).
func TestDrift_NetworkPolicyShapesDiffer(t *testing.T) {
	t.Parallel()

	var denyAll, dnsEgress, httpIngress networkingv1.NetworkPolicy
	decodeManifest(t, "networkpolicy.yaml", 0, &denyAll)
	decodeManifest(t, "networkpolicy.yaml", 1, &dnsEgress)
	decodeManifest(t, "networkpolicy.yaml", 2, &httpIngress)

	require.Equal(t, "default-deny-all", denyAll.Name)
	require.Equal(t, "allow-dns-egress", dnsEgress.Name)
	require.Equal(t, "allow-http-ingress", httpIngress.Name)
	require.Empty(t, denyAll.Spec.PodSelector.MatchLabels, "the static default-deny is namespace-wide by design")

	plant := driftTestPlant()
	generated := controller.NetworkPolicyFor(plant)

	// One object, scoped to this Plant, carrying both policy types.
	require.Equal(t, controller.SelectorFor(plant), generated.Spec.PodSelector.MatchLabels,
		"the operator's policy must be Plant-scoped, never namespace-wide")
	require.ElementsMatch(t,
		[]networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		generated.Spec.PolicyTypes,
		"one object has to declare both directions, since there is no separate default-deny object")

	// The DNS hole is identical in substance: same destination namespace,
	// same two protocols, same port.
	require.Equal(t, dnsEgress.Spec.Egress[0].To[0].NamespaceSelector.MatchLabels,
		generated.Spec.Egress[0].To[0].NamespaceSelector.MatchLabels)
	require.Equal(t, dnsEgress.Spec.Egress[0].Ports, generated.Spec.Egress[0].Ports,
		"UDP/53 and TCP/53 -- TCP is not optional, Go's resolver falls back to it for truncated responses")

	// The HTTP hole is identical in substance: same port, and crucially the
	// same absence of a `from:` selector.
	require.Equal(t, httpIngress.Spec.Ingress[0].Ports, generated.Spec.Ingress[0].Ports)
	require.Empty(t, httpIngress.Spec.Ingress[0].From, "ADR 0003")
	require.Empty(t, generated.Spec.Ingress[0].From, "ADR 0003")
}

// --- namespace posture ---------------------------------------------------

// TestDrift_BothNamespacesEnforceRestricted asserts the property that made
// the Plant namespace necessary in the first place: the project's spec says
// the security posture applies to every workload without exception, and a
// Plant applied with no namespace used to land in `default`, which carries no
// Pod Security Admission labels at all.
func TestDrift_BothNamespacesEnforceRestricted(t *testing.T) {
	t.Parallel()

	var staticNS corev1.Namespace
	decodeManifest(t, "namespace.yaml", 0, &staticNS)

	rawPlants, err := os.ReadFile(filepath.Join("..", "..", "deploy", "kustomize", "plants", "namespace.yaml"))
	require.NoError(t, err)
	var plantsNS corev1.Namespace
	require.NoError(t, yaml.Unmarshal(rawPlants, &plantsNS))

	require.Equal(t, "k8s-buddy", staticNS.Name)
	require.Equal(t, "k8s-buddy-plants", plantsNS.Name)

	for _, label := range []string{
		"pod-security.kubernetes.io/enforce",
		"pod-security.kubernetes.io/enforce-version",
		"pod-security.kubernetes.io/audit",
		"pod-security.kubernetes.io/warn",
	} {
		require.NotEmpty(t, staticNS.Labels[label], "%s missing from the static namespace", label)
		require.Equal(t, staticNS.Labels[label], plantsNS.Labels[label],
			"%s must be identical in both namespaces; operator-managed workloads must not be less constrained than static ones", label)
	}
	require.Equal(t, "restricted", plantsNS.Labels["pod-security.kubernetes.io/enforce"])
}
