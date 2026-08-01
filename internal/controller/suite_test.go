//go:build envtest

// This file (suite_test.go), plant_controller_test.go, and
// counting_client_test.go together are the envtest suite: they run
// PlantReconciler against a real kube-apiserver and etcd (envtest's own,
// entirely separate control plane -- never the kind cluster a developer may
// also have running), rather than a fake client. That is a materially
// stronger guarantee than a fake-client unit test can offer: a fake client
// accepts whatever a reconciler sends it, while a real API server applies
// defaulting, validation, and immutability rules exactly the way production
// does, which is precisely the class of bug (see the write-storm scenario in
// counting_client_test.go's doc comment) a fake client cannot surface.
//
// These three files are gated behind the "envtest" build tag rather than
// running under a plain `go test ./...`, so `make test` (and CI's
// `make test-race` / `make test-cover`) keep working on a machine that has
// never downloaded the envtest control-plane binaries. Booting a real
// kube-apiserver takes real wall-clock time and a real binary download; that
// is a deliberate opt-in cost, not something every `go test ./...` should
// pay by default. The suite is never silently skipped, though: run it (via
// `make test-envtest`, or `go test -tags envtest ./internal/controller/...`
// directly) without the assets available and it fails loudly in TestMain,
// with a message telling the developer to run `make envtest` first -- never
// a quiet t.Skip that could pass green without ever having run.
package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	buddyv1alpha1 "github.com/sean-kramer/k8s-buddy/api/v1alpha1"
)

// envtestK8sVersion pins the Kubernetes control-plane version this suite
// boots. It MUST match the Makefile's ENVTEST_K8S_VERSION -- there is no
// single source of truth shared between make and go test, so the two are
// kept in sync by convention and this comment, not by tooling.
const envtestK8sVersion = "1.36.2"

// reconcilerName matches SetupWithManager's ctrl.NewControllerManagedBy(...).
// Named("plant") call in plant_controller.go: it is the "controller" label
// controller-runtime's own controller_runtime_reconcile_total metric carries
// for every reconcile of a Plant, which reconcileTotal below reads.
const reconcilerName = "plant"

// Shared suite state, set up once in TestMain and read (never reassigned)
// by every test in this package. Booting a real kube-apiserver and etcd per
// test would push the suite well past the ~60s budget the task brief sets;
// sharing one control plane and one running manager across every test is
// what keeps it fast, and is safe as long as every test creates its Plant(s)
// in a namespace of its own (see newTestNamespace) and cleans them up (see
// createPlant) so no test's Plant is still being reconciled by the time the
// next test's assertions run against the two suite-wide shared signals this
// file exposes: testCounting (write counts, bucketed by GVK, not by
// individual object) and controller_runtime_reconcile_total (a single
// process-wide counter, not scoped to a Plant at all).
var (
	testScheme   *k8sruntime.Scheme
	testEnv      *envtest.Environment
	testMgr      ctrl.Manager
	testClient   client.Client // direct, uncounted client -- test setup and assertions always go through this, never through testCounting.
	testCounting *countingClient
	testCtx      context.Context
	testCancel   context.CancelFunc
)

func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// runSuite is TestMain's body, split out so it can return an int (rather
// than calling os.Exit itself from deep inside setup) and so every early
// return funnels through the same "print a clear diagnostic, exit 1" path
// instead of a panic that would dump a raw stack trace at a developer who
// just forgot to run `make envtest`.
func runSuite(m *testing.M) int {
	assetsDir, err := resolveKubebuilderAssets()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	testScheme = k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(testScheme); err != nil {
		fmt.Fprintln(os.Stderr, "adding client-go scheme:", err)
		return 1
	}
	if err := buddyv1alpha1.AddToScheme(testScheme); err != nil {
		fmt.Fprintln(os.Stderr, "adding buddy.k8s-buddy.io/v1alpha1 scheme:", err)
		return 1
	}

	logf.SetLogger(ctrlzap.New(ctrlzap.UseDevMode(true)))

	testEnv = &envtest.Environment{
		// The generated CRD, not a hand-maintained copy -- Task 1's own
		// binding constraint ("never hand-edit generated files") applies
		// just as much to what this suite installs.
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: assetsDir,
		Scheme:                testScheme,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting envtest control plane:", err)
		return 1
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, "stopping envtest control plane:", err)
		}
	}()

	testClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, "building direct test client:", err)
		return 1
	}

	testMgr, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme: testScheme,
		// THE SAME cache configuration the deployed binary uses -- the
		// shared CacheOptions() in cache.go, called by both, never a copy.
		//
		// This suite previously passed no Cache option at all, so it cached
		// every child object unfiltered while cmd/plant-operator cached only
		// those labelled app.kubernetes.io/managed-by=plant-operator. That
		// difference was not cosmetic: it made the suite structurally unable
		// to reproduce any bug whose mechanism is "the cache cannot see this
		// object," which is exactly the class of bug the label filter
		// introduces. A test asserting that a de-labelled child gets its
		// label restored passed here while the same scenario wedged the real
		// operator in an AlreadyExists loop forever.
		//
		// Mirroring the deployed configuration means objects created bare or
		// unlabelled by a test are now invisible to the manager's cache, the
		// same way they are invisible in production. That is correct and is
		// the point. Tests that need to observe such an object read through
		// testClient (a direct, uncached client) rather than loosening this.
		Cache: CacheOptions(),
		// "0" disables the metrics HTTP server and health-probe HTTP
		// server entirely -- this suite reads the reconcile-count metric
		// straight out of the in-process prometheus registry (see
		// reconcileTotal below), never over HTTP, and nothing here serves
		// health probes to anyone. Binding a real port would also risk
		// colliding with another test binary or the live kind cluster's
		// own tooling running on the same dev box.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "building manager:", err)
		return 1
	}

	// testCounting wraps the manager's own client -- the one the running
	// reconciler actually uses for every Get/Create/Update/Patch/Status
	// call -- rather than a separate client of its own. Assigning it to
	// PlantReconciler.Client below (instead of relying on the embedded
	// client.Client controller-runtime would otherwise wire up
	// automatically) is what lets the idempotence test count the exact
	// calls the real reconciler makes, not calls made against some other,
	// unwatched client.
	testCounting = newCountingClient(testMgr.GetClient())

	reconciler := &PlantReconciler{
		Client: testCounting,
		Scheme: testMgr.GetScheme(),
		// The manager's uncached reader, exactly as cmd/plant-operator
		// wires it. applyChild is the only consumer, and giving it
		// the real thing here is what lets the adoption-refusal case below
		// exercise the same code path the deployed operator runs.
		APIReader: testMgr.GetAPIReader(),
		// PlantReconciler.Recorder (plant_controller.go, out of scope for
		// this task) is typed record.EventRecorder, the old events API --
		// GetEventRecorderFor is the only manager method that returns that
		// exact type, so its deprecation warning is unavoidable here without
		// changing Task 3's reconciler.
		Recorder: testMgr.GetEventRecorderFor("plant-controller"), //nolint:staticcheck // record.EventRecorder is PlantReconciler.Recorder's fixed field type
	}
	if err := reconciler.SetupWithManager(testMgr); err != nil {
		fmt.Fprintln(os.Stderr, "wiring PlantReconciler into the manager:", err)
		return 1
	}

	testCtx, testCancel = context.WithCancel(context.Background())
	mgrDone := make(chan error, 1)
	go func() { mgrDone <- testMgr.Start(testCtx) }()

	if !testMgr.GetCache().WaitForCacheSync(testCtx) {
		fmt.Fprintln(os.Stderr, "manager cache never synced")
		testCancel()
		return 1
	}

	code := m.Run()

	testCancel()
	select {
	case err := <-mgrDone:
		if err != nil {
			fmt.Fprintln(os.Stderr, "manager exited with an error during shutdown:", err)
		}
	case <-time.After(10 * time.Second):
		fmt.Fprintln(os.Stderr, "manager did not shut down within 10s of ctx cancellation")
	}

	return code
}

// resolveKubebuilderAssets finds the directory containing the envtest
// control-plane binaries (etcd, kube-apiserver, kubectl), in the order the
// task brief specifies: an already-set KUBEBUILDER_ASSETS env var wins
// outright; otherwise it shells out to setup-envtest to resolve (and, if
// not already cached, download) the pinned Kubernetes version. It never
// returns a "just skip the suite" signal -- only a path or a descriptive
// error -- so a missing-assets suite run fails loudly in TestMain instead of
// silently reporting a pass it never actually attempted.
func resolveKubebuilderAssets() (string, error) {
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir, nil
	}

	bin, err := locateSetupEnvtest()
	if err != nil {
		return "", fmt.Errorf(
			"KUBEBUILDER_ASSETS is not set and no setup-envtest binary could be found (%w). "+
				"Run `make envtest` first to download the envtest control-plane binaries, "+
				"or set KUBEBUILDER_ASSETS to their directory manually", err)
	}

	cmd := exec.Command(bin, "use", envtestK8sVersion, "-p", "path", "--os", goruntime.GOOS, "--arch", goruntime.GOARCH)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf(
			"resolving envtest assets via %q failed: %w (output: %s). "+
				"Run `make envtest` first to download the envtest control-plane binaries",
			bin, err, strings.TrimSpace(string(out)))
	}

	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("%q returned an empty path; run `make envtest` first", bin)
	}
	return dir, nil
}

// locateSetupEnvtest looks for a setup-envtest binary in the same pinned
// location `make envtest` installs it to (.tools/, relative to the repo
// root -- this test binary's working directory is internal/controller, so
// that's two levels up), falling back to PATH for a developer who installed
// it some other way. It deliberately does not attempt to `go install` a
// binary itself: resolving a tool version belongs to the Makefile's pinned-
// tool pattern (see controller-gen and golangci-lint), not to a test file
// reaching out to the network on its own.
func locateSetupEnvtest() (string, error) {
	exeSuffix := ""
	if goruntime.GOOS == "windows" {
		exeSuffix = ".exe"
	}

	pinned := filepath.Join("..", "..", ".tools", "setup-envtest"+exeSuffix)
	if _, err := os.Stat(pinned); err == nil {
		abs, err := filepath.Abs(pinned)
		if err != nil {
			return "", err
		}
		return abs, nil
	}

	if p, err := exec.LookPath("setup-envtest"); err == nil {
		return p, nil
	}

	return "", fmt.Errorf("no setup-envtest binary in .tools/ or PATH")
}

// --- shared test helpers -----------------------------------------------

// newTestNamespace creates a uniquely-named namespace (via GenerateName) and
// returns its name. Every test creates its Plant(s) in a namespace of their
// own so that no two tests' objects can ever collide or be mistaken for one
// another, regardless of execution order.
func newTestNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "plant-test-"},
	}
	require.NoError(t, testClient.Create(testCtx, ns))
	return ns.Name
}

// newTestPlant returns a Plant, fully specified (as if API-server defaulting
// had already applied) rather than relying on the CRD's own default
// markers, so every test's inputs are explicit.
//
// WateringInterval is set far longer than this suite's entire run -- long
// enough that no test's Plant ever requeues on its own timer during the
// suite. Every reconcile this suite observes is therefore caused by an
// explicit action a test took (create, spec update, annotation touch, a
// child edited directly), never by a background timer firing at an
// unpredictable moment. That determinism is what makes it safe to read the
// two suite-wide shared signals (testCounting and the reconcile-count
// metric) from a sequential test run: as long as only one test's Plant is
// under active reconciliation at a time, a signal that is technically
// process-wide behaves, in practice, as if it were scoped to that Plant.
func newTestPlant(namespace, name string, replicas int32) *buddyv1alpha1.Plant {
	return &buddyv1alpha1.Plant{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: buddyv1alpha1.PlantSpec{
			Species:          "fern",
			Replicas:         int32Ptr(replicas),
			Image:            "ghcr.io/sean-kramer/k8s-buddy/buddy-api:dev",
			ResourceProfile:  "small",
			WateringInterval: metav1.Duration{Duration: 10 * time.Minute},
			LatencyBudget:    metav1.Duration{Duration: 150 * time.Millisecond},
		},
	}
}

func int32Ptr(v int32) *int32 { return &v }

// createPlant creates plant via the direct (uncounted) test client and
// registers a cleanup that deletes it and waits for the delete to actually
// complete -- i.e. for the reconciler's finalizer-removal reconcile to have
// already run -- before the calling test returns. That synchronous wait is
// what stops one test's trailing delete-reconcile from bleeding into the
// next test's "reset counters, trigger one reconcile" window.
func createPlant(t *testing.T, plant *buddyv1alpha1.Plant) {
	t.Helper()
	require.NoError(t, testClient.Create(testCtx, plant))
	t.Cleanup(func() { deletePlantAndWait(t, plant) })
}

func deletePlantAndWait(t *testing.T, plant *buddyv1alpha1.Plant) {
	t.Helper()
	key := client.ObjectKeyFromObject(plant)

	if err := testClient.Delete(testCtx, plant); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("cleanup: deleting plant %s: %v", key, err)
		return
	}

	require.Eventually(t, func() bool {
		got := &buddyv1alpha1.Plant{}
		return apierrors.IsNotFound(testClient.Get(testCtx, key, got))
	}, 10*time.Second, 100*time.Millisecond, "cleanup: plant %s was never actually deleted", key)
}

// updatePlant fetches the Plant at key, applies mutate to it, and Updates
// it, retrying the whole fetch-mutate-update cycle on a Conflict via
// retry.RetryOnConflict. Every test that reads a Plant and writes it back
// needs this rather than a plain Get-then-Update: the reconciler's OWN
// status-subresource write can land in the exact window between this
// helper's Get and Update (most commonly right after waitForChildrenExist,
// while the creation reconcile's status write is still in flight), bumping
// resourceVersion and turning what looks like an unconditional Update into
// an intermittent 409 the moment the reconciler is even mildly busy. A
// bare require.NoError(t, testClient.Update(...)) has no way to recover
// from that; retrying with a fresh Get does.
func updatePlant(t *testing.T, key client.ObjectKey, mutate func(*buddyv1alpha1.Plant)) *buddyv1alpha1.Plant {
	t.Helper()
	fresh := &buddyv1alpha1.Plant{}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := testClient.Get(testCtx, key, fresh); err != nil {
			return err
		}
		mutate(fresh)
		return testClient.Update(testCtx, fresh)
	})
	require.NoError(t, err, "updating plant %s", key)
	return fresh
}

// requireNoFinalizers asserts plant carries no finalizers at all.
//
// This operator adds none, deliberately -- see ADR 0007. The assertion is
// "the list is empty", not "our particular string is absent", because the
// failure this guards against is a finalizer being reintroduced under ANY
// name: the liability is not the string, it is that a Plant becomes
// undeletable whenever the operator is down, and nothing this operator owns
// needs cleanup that garbage collection does not already perform.
func requireNoFinalizers(t *testing.T, plant *buddyv1alpha1.Plant) {
	t.Helper()
	got := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, client.ObjectKeyFromObject(plant), got))
	require.Empty(t, got.Finalizers,
		"plant %s/%s must carry no finalizers: children are removed by garbage collection via owner references, "+
			"and a finalizer that cleans nothing only makes deletion depend on the operator being up (see ADR 0007)",
		got.Namespace, got.Name)
}

// waitForChildrenExist polls until all six of plant's children exist.
func waitForChildrenExist(t *testing.T, plant *buddyv1alpha1.Plant) {
	t.Helper()
	require.Eventually(t, func() bool {
		return childrenExist(plant)
	}, 10*time.Second, 100*time.Millisecond, "children for plant %s/%s were never all created", plant.Namespace, plant.Name)
}

func childrenExist(plant *buddyv1alpha1.Plant) bool {
	if err := testClient.Get(testCtx, client.ObjectKey{Namespace: plant.Namespace, Name: plant.Name}, &appsv1.Deployment{}); err != nil {
		return false
	}
	if err := testClient.Get(testCtx, client.ObjectKey{Namespace: plant.Namespace, Name: plant.Name}, &corev1.Service{}); err != nil {
		return false
	}
	if err := testClient.Get(testCtx, client.ObjectKey{Namespace: plant.Namespace, Name: plant.Name}, &corev1.ConfigMap{}); err != nil {
		return false
	}
	if err := testClient.Get(testCtx, client.ObjectKey{Namespace: plant.Namespace, Name: plant.Name + "-pdb"}, &policyv1.PodDisruptionBudget{}); err != nil {
		return false
	}
	if err := testClient.Get(testCtx, client.ObjectKey{Namespace: plant.Namespace, Name: plant.Name}, &corev1.ServiceAccount{}); err != nil {
		return false
	}
	if err := testClient.Get(testCtx, client.ObjectKey{Namespace: plant.Namespace, Name: plant.Name}, &networkingv1.NetworkPolicy{}); err != nil {
		return false
	}
	return true
}

// waitForStatusPopulated polls until plant's status carries at least one
// condition (i.e. the first real reconcileStatus write has landed), and
// returns the object as observed at that point.
func waitForStatusPopulated(t *testing.T, plant *buddyv1alpha1.Plant) *buddyv1alpha1.Plant {
	t.Helper()
	got := &buddyv1alpha1.Plant{}
	require.Eventually(t, func() bool {
		if err := testClient.Get(testCtx, client.ObjectKeyFromObject(plant), got); err != nil {
			return false
		}
		return len(got.Status.Conditions) > 0
	}, 10*time.Second, 100*time.Millisecond, "status was never populated for plant %s/%s", plant.Namespace, plant.Name)
	return got
}

// assertControllerOwnerRef asserts obj carries exactly one Controller:true
// owner reference, pointing at owner with the correct Kind, APIVersion,
// Name, and UID -- what Kubernetes garbage collection itself reads to
// decide whether to remove obj when owner is deleted. "Exactly one" is
// actually checked (not just found-at-least-one): the API server itself
// rejects a second Controller:true owner reference on the same object, so
// more than one here would mean this suite's own scheme/client is doing
// something it shouldn't, not a real possible cluster state -- but the
// assertion is cheap and keeps the comment honest either way.
func assertControllerOwnerRef(t *testing.T, obj metav1.Object, owner *buddyv1alpha1.Plant) {
	t.Helper()
	var controllerRefs []metav1.OwnerReference
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			controllerRefs = append(controllerRefs, ref)
		}
	}
	require.Len(t, controllerRefs, 1, "expected exactly one controller owner reference on %s/%s, got %+v",
		obj.GetNamespace(), obj.GetName(), controllerRefs)

	found := controllerRefs[0]
	require.Equal(t, "Plant", found.Kind)
	require.Equal(t, buddyv1alpha1.GroupVersion.String(), found.APIVersion)
	require.Equal(t, owner.Name, found.Name)
	require.Equal(t, owner.UID, found.UID)
	require.True(t, found.BlockOwnerDeletion != nil && *found.BlockOwnerDeletion,
		"controllerutil.SetControllerReference always sets BlockOwnerDeletion:true")
}

// reconcileTotal reads controller-runtime's own
// controller_runtime_reconcile_total counter for this reconciler
// (summed across every "result" label: success, error, requeue,
// requeue_after) straight out of the in-process metrics registry. Unlike
// countingClient's counts, this metric increments only after Reconcile
// returns -- so once it stops moving, every write that reconcile pass could
// possibly have made has already happened, which is what makes it safe to
// use as a "that reconcile is now fully finished" signal in
// waitForReconcileQuiescence and triggerReconcile below.
//
// It deliberately takes no *testing.T and returns an error instead of
// calling require.NoError itself: both call sites below invoke it from
// inside a require.Eventually condition function, which testify runs on a
// goroutine of its own -- calling a require.* (FailNow-family) method from
// that goroutine, rather than the test's own, is undefined behavior per the
// testing package's own rules. Returning the error and letting the caller
// decide (log-and-retry inside Eventually, require.NoError only on the
// test's own goroutine) avoids that entirely.
func reconcileTotal() (float64, error) {
	families, err := metrics.Registry.Gather()
	if err != nil {
		return 0, fmt.Errorf("gathering metrics: %w", err)
	}

	var total float64
	for _, fam := range families {
		if fam.GetName() != "controller_runtime_reconcile_total" {
			continue
		}
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "controller" && lp.GetValue() == reconcilerName {
					total += m.GetCounter().GetValue()
				}
			}
		}
	}
	return total, nil
}

// waitForReconcileQuiescence polls reconcileTotal until it has reported the
// same value three samples in a row (300ms of observed stillness), i.e.
// until nothing is actively being reconciled. Every test that needs a clean
// "steady state" starting point -- in particular the idempotence test, which
// must not mistake a still-settling creation reconcile for the one no-op
// reconcile it's trying to isolate -- calls this before taking its baseline.
func waitForReconcileQuiescence(t *testing.T) {
	t.Helper()
	last := -1.0
	stable := 0
	require.Eventually(t, func() bool {
		cur, err := reconcileTotal()
		if err != nil {
			// t.Logf, unlike require.NoError, is safe to call from this
			// polling goroutine -- see reconcileTotal's own comment.
			// Gather() against an in-process registry essentially never
			// fails; if it somehow does, this makes the eventual timeout
			// message diagnosable instead of silently looping forever.
			t.Logf("reconcileTotal: %v", err)
			return false
		}
		if cur == last {
			stable++
		} else {
			stable = 0
			last = cur
		}
		return stable >= 3
	}, 15*time.Second, 100*time.Millisecond, "reconcile count for %q never stabilized", reconcilerName)
}

// triggerReconcile makes a single, deliberate, spec-inert edit to plant (an
// annotation the reconciler never reads) so the manager's watch on Plant
// enqueues exactly one more reconcile, then blocks until that reconcile (and
// only that one) has fully completed, per reconcileTotal's own doc comment.
// plant is updated in place with the post-edit object.
func triggerReconcile(t *testing.T, plant *buddyv1alpha1.Plant) {
	t.Helper()
	waitForReconcileQuiescence(t)
	before, err := reconcileTotal()
	require.NoError(t, err) // safe here: runs on the test's own goroutine, not inside Eventually.

	key := client.ObjectKeyFromObject(plant)
	fresh := &buddyv1alpha1.Plant{}
	require.NoError(t, testClient.Get(testCtx, key, fresh))
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations["test.k8s-buddy.io/touch"] = time.Now().UTC().Format(time.RFC3339Nano)
	require.NoError(t, testClient.Update(testCtx, fresh))

	require.Eventually(t, func() bool {
		cur, err := reconcileTotal()
		if err != nil {
			t.Logf("reconcileTotal: %v", err)
			return false
		}
		return cur > before
	}, 10*time.Second, 100*time.Millisecond, "annotation touch never triggered a reconcile")

	waitForReconcileQuiescence(t)
	*plant = *fresh
}

// conditionsByType indexes conditions by Type for easy before/after
// comparison.
func conditionsByType(conditions []metav1.Condition) map[string]metav1.Condition {
	m := make(map[string]metav1.Condition, len(conditions))
	for _, c := range conditions {
		m[c.Type] = c
	}
	return m
}
