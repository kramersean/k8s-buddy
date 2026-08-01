# Plan 2 of 3 — The Plant operator

**Spec:** `docs/superpowers/specs/2026-07-31-k8s-buddy-platform-showcase-design.md`
**Branch:** `feat/platform-showcase`
**Predecessor:** Plan 1 (foundation) — complete, merge-ready at `92156c3`

**Outcome when complete:**

```
$ kubectl apply -f config/samples/plant-fernie.yaml
plant.buddy.k8s-buddy.io/fernie created

$ kubectl get plants
NAME     SPECIES   MOOD      HEALTH   READY   AGE
fernie   fern      thirsty   63%      3/3     41s

$ kubectl delete plant fernie
# Deployment, Service, ConfigMap and PDB are all garbage-collected
```

This is the plan that turns the project from a well-run Deployment into a
demonstration of the Kubernetes control plane.

---

## Global Constraints

Binding. Copy values verbatim; never paraphrase.

**API**
- Group `buddy.k8s-buddy.io`, version `v1alpha1`, Kind `Plant`, plural `plants`,
  singular `plant`, shortName `pl`, scope **Namespaced**, categories `["all"]`.
- Go types live in `api/v1alpha1/`. Package name `v1alpha1`.
- The CRD manifest is **generated** by `controller-gen` into
  `config/crd/bases/buddy.k8s-buddy.io_plants.yaml`. Never hand-edit generated
  files; if the output is wrong, fix the marker comments and regenerate.

**Dependencies** — resolve to the newest versions mutually compatible with the
cluster (Kubernetes 1.36.1) and pin them exactly in `go.mod`:
`k8s.io/api`, `k8s.io/apimachinery`, `k8s.io/client-go` at `v0.36.x`, and
`sigs.k8s.io/controller-runtime` at the release whose own `go.mod` requires that
`v0.36.x` line. Record the resolved versions in the task report.

`go.mod` declares **`go 1.26.0`** and gains no `toolchain` directive.
*(Amended during Task 1. The constraint originally said `go 1.25.0`, inherited
from Plan 1. `k8s.io/apimachinery v0.36.3` declares `go 1.26.0` as its own
minimum — Kubernetes 1.36 is built with Go 1.26 — so the build fails outright
once apimachinery is imported. Verified empirically, not assumed. This is the
second time a dependency has forced the `go` directive upward; the value is
dictated by the dependency tree, not chosen.)*

Resolved and pinned in Task 1: `k8s.io/api`, `k8s.io/apimachinery`,
`k8s.io/client-go` at `v0.36.3`; `sigs.k8s.io/controller-runtime` at `v0.24.1`;
`controller-gen` at `v0.21.0`.

**Pinned tools**, downloaded into `.tools/` by Makefile targets, never `@latest`:
`controller-gen`, `setup-envtest`. Follow the existing `golangci-lint` pattern
in the Makefile.

**Operator ports** — metrics `8081`, health/readiness `8082`. (Webhooks on `9443`
are Plan 3; do not build them here.)

**Naming of generated children.** A `Plant` named `fernie` owns children named
exactly `fernie` (Deployment, Service, ConfigMap) and `fernie-pdb`. All children
are created in the `Plant`'s own namespace.

**Labels on every generated child** — the five standard labels, with these values:
```
app.kubernetes.io/name: buddy-api
app.kubernetes.io/instance: <plant name>
app.kubernetes.io/component: api
app.kubernetes.io/part-of: k8s-buddy
app.kubernetes.io/managed-by: plant-operator
```
Plus `buddy.k8s-buddy.io/plant: <plant name>` as the selector key. The Deployment's
`spec.selector` uses `app.kubernetes.io/instance` + `buddy.k8s-buddy.io/plant`
only — selectors are immutable, so they must not include values that can change.

**Security posture** is inherited unchanged from Plan 1: every generated pod spec
carries `runAsNonRoot: true`, `runAsUser: 65532`, `allowPrivilegeEscalation: false`,
`readOnlyRootFilesystem: true`, `capabilities.drop: ["ALL"]`,
`seccompProfile.type: RuntimeDefault`, resource requests and limits, and
`automountServiceAccountToken: false` on the workload pods. The operator's own pod
carries the same context. Generated pods must be admissible under PodSecurity
`restricted` — this is verified, not assumed.

**Code style** — unchanged from Plan 1: `log/slog`-style structured logging (use
controller-runtime's `logr` inside reconcilers), wrapped errors, doc comments on
exported identifiers, `make lint test` clean at the end of every task. Tests use
`testify/require`. No `-race` locally (no cgo on the dev box); CI runs it.

---

## The Plant API

Exact shapes. Later tasks compile against these.

```go
// PlantSpec describes the desired plant.
type PlantSpec struct {
    // Species is the plant variety. Cosmetic: it flows into the workload's
    // BUDDY_SPECIES and shapes what the plant says about itself.
    // +kubebuilder:validation:Enum=fern;cactus;succulent;orchid;fiddle-leaf
    // +kubebuilder:default=fern
    Species string `json:"species,omitempty"`

    // Replicas is how many leaves this plant grows.
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=10
    // +kubebuilder:default=3
    Replicas *int32 `json:"replicas,omitempty"`

    // Image is the buddy-api container image to run.
    // +kubebuilder:default="ghcr.io/sean-kramer/k8s-buddy/buddy-api:dev"
    Image string `json:"image,omitempty"`

    // ResourceProfile selects the CPU/memory envelope.
    // +kubebuilder:validation:Enum=small;medium;large
    // +kubebuilder:default=small
    ResourceProfile string `json:"resourceProfile,omitempty"`

    // WateringInterval is how often the operator re-reconciles this plant even
    // when nothing has changed, so status stays fresh.
    // +kubebuilder:default="30s"
    WateringInterval metav1.Duration `json:"wateringInterval,omitempty"`

    // LatencyBudget is passed to the workload as BUDDY_LATENCY_BUDGET.
    // +kubebuilder:default="150ms"
    LatencyBudget metav1.Duration `json:"latencyBudget,omitempty"`
}

// PlantStatus is the observed state of a Plant.
type PlantStatus struct {
    Mood               string             `json:"mood,omitempty"`
    HealthPercent      int32              `json:"healthPercent"`
    ReadyReplicas      int32              `json:"readyReplicas"`
    DesiredReplicas    int32              `json:"desiredReplicas"`
    ObservedGeneration int64              `json:"observedGeneration,omitempty"`
    LastWatered        *metav1.Time       `json:"lastWatered,omitempty"`
    Conditions         []metav1.Condition `json:"conditions,omitempty"`
}
```

**Resource profiles** map to exactly these values:

| Profile | requests cpu/mem | limits cpu/mem |
|---|---|---|
| `small` | `50m` / `64Mi` | `200m` / `128Mi` |
| `medium` | `100m` / `128Mi` | `500m` / `256Mi` |
| `large` | `250m` / `256Mi` | `1000m` / `512Mi` |

**Printer columns**, in this order:
`SPECIES` (`.spec.species`), `MOOD` (`.status.mood`),
`HEALTH` (`.status.healthPercent`), `READY` (`.status.readyReplicas`),
`DESIRED` (`.status.desiredReplicas`), `AGE`.

**Conditions** — standard `metav1.Condition`, managed with
`meta.SetStatusCondition`. Exactly these types and reasons:

| Type | Status | Reason | When |
|---|---|---|---|
| `Ready` | `True` | `AllReplicasReady` | ready == desired, desired > 0 |
| `Ready` | `False` | `ReplicasNotReady` | ready < desired |
| `Progressing` | `True` | `RolloutInProgress` | Deployment generation not yet observed, or ready != desired |
| `Progressing` | `False` | `RolloutComplete` | Deployment fully rolled out |
| `Degraded` | `True` | `InsufficientReplicas` | ready == 0 and desired > 0 |
| `Degraded` | `False` | `PlantHealthy` | otherwise |

`observedGeneration` on every condition is set to the `Plant`'s `metadata.generation`.

**Finalizer** — `buddy.k8s-buddy.io/finalizer`.

---

## Task 1 — Plant API types and CRD generation

**Files:** `api/v1alpha1/{groupversion_info.go,plant_types.go,zz_generated.deepcopy.go}`,
`config/crd/bases/buddy.k8s-buddy.io_plants.yaml` (generated),
`config/samples/plant-fernie.yaml`, `config/samples/plant-spike.yaml`,
`Makefile` (add `controller-gen`, `manifests`, `generate` targets), `PROJECT`

- `groupversion_info.go` with `GroupVersion`, `SchemeBuilder`, `AddToScheme`,
  and the `// +kubebuilder:object:generate=true` and `// +groupName=` markers.
- `plant_types.go` with `Plant`, `PlantList`, `PlantSpec`, `PlantStatus` exactly as
  specified above, plus the markers for `subresource:status`, printer columns,
  shortName, categories, and the validation/default markers shown.
- Deepcopy generated by `controller-gen object`. Do not hand-write it.
- Two samples: `fernie` (fern, 3 replicas, defaults) and `spike`
  (cactus, 2 replicas, `resourceProfile: medium`).
- Makefile: `controller-gen` (download pinned version into `.tools/`),
  `generate` (deepcopy), `manifests` (CRD + later RBAC). Add both to the `help`
  output following the existing `##` convention.
- `PROJECT` file in kubebuilder's format, so the layout is recognizable to anyone
  who has used the SDK.

**Verify:** `make generate manifests` is idempotent (run twice, `git status` clean
the second time); `go build ./...`; the generated CRD applies to the live cluster
(`kubectl apply -f config/crd/bases/...`) and `kubectl explain plant.spec.species`
shows the enum; both samples validate with `--dry-run=server`; a Plant with
`replicas: 0` is **rejected** by the API server (proving the Minimum marker works);
a Plant with `species: bonsai` is **rejected** (proving the Enum works).
Delete the applied test objects afterward, leave the CRD installed.

---

## Task 2 — Child resource builders

**Files:** `internal/controller/resources.go`, `internal/controller/resources_test.go`

Pure functions that turn a `Plant` into its desired children. No client calls, no
reconcile logic — this is the testable core, and keeping it pure is what makes
Task 4's envtest suite small instead of sprawling.

```go
func DeploymentFor(p *buddyv1alpha1.Plant) *appsv1.Deployment
func ServiceFor(p *buddyv1alpha1.Plant) *corev1.Service
func ConfigMapFor(p *buddyv1alpha1.Plant) *corev1.ConfigMap
func PodDisruptionBudgetFor(p *buddyv1alpha1.Plant) *policyv1.PodDisruptionBudget
func LabelsFor(p *buddyv1alpha1.Plant) map[string]string
func SelectorFor(p *buddyv1alpha1.Plant) map[string]string
func ResourcesFor(profile string) corev1.ResourceRequirements
```

Requirements:
- The Deployment mirrors Plan 1's hardened pod spec: the full security context,
  the three probes with Plan 1's timings (`/healthz` liveness, `/readyz` readiness
  at `periodSeconds: 2`/`failureThreshold: 2`, `/healthz` startup),
  `terminationGracePeriodSeconds: 30`, no `preStop` hook (distroless has no shell —
  reference ADR 0002 in a comment), `imagePullPolicy: IfNotPresent`,
  topology spread across `kubernetes.io/hostname` with `ScheduleAnyway`.
- Env comes from the generated ConfigMap via `envFrom`. The ConfigMap carries
  `BUDDY_NAME` (the Plant's name), `BUDDY_SPECIES`, `BUDDY_LATENCY_BUDGET`, and the
  remaining `BUDDY_*` values at Plan 1's defaults. It must **not** set `BUDDY_PORT`
  (Plan 1 removed it deliberately — the port is pinned in three probe definitions).
- `ResourcesFor` returns the exact table above; an unknown profile falls back to
  `small` rather than returning empty requirements.
- PDB: `minAvailable` = `replicas - 1`, floored at `1`. With 1 replica this yields
  `minAvailable: 1`, which is correct — a single-replica plant should block
  voluntary disruption rather than permit its own deletion.
- **Determinism:** calling a builder twice with the same Plant must produce
  deeply-equal output. Any map iteration that reaches a slice must be sorted.
  Task 3 depends on this to detect drift without false positives.
- Builders set labels and the selector, but **not** owner references — the
  reconciler owns that, so these stay pure.

**Tests** (pure, fast, table-driven): every field above asserted; all three resource
profiles plus an unknown profile; PDB `minAvailable` at replicas 1, 2, 3, 10;
determinism (build twice, `require.Equal`); the selector contains only the two
immutable keys; `BUDDY_PORT` is absent from the ConfigMap; the security context is
complete and matches PodSecurity `restricted`.

**Verify:** `make test lint` clean; coverage on `internal/controller` ≥ 90% for
this file's functions.

---

## Task 3 — The reconciler

**Files:** `internal/controller/plant_controller.go`, `internal/controller/status.go`

`PlantReconciler` with `client.Client`, `*runtime.Scheme`, and a `record.EventRecorder`.

**Reconcile flow, in this order:**
1. Fetch the `Plant`. `apierrors.IsNotFound` → return without error (already deleted).
2. If `DeletionTimestamp` is set: run finalizer cleanup, remove the finalizer,
   update, return. Cleanup emits an Event and logs; owned children are removed by
   garbage collection via owner references, so cleanup must **not** delete them
   manually — say so in a comment, because deleting them by hand is the common
   mistake and doing it correctly is the point.
3. Ensure the finalizer is present; if added, update and return early to get a
   fresh object.
4. Build each desired child, set the owner reference via
   `controllerutil.SetControllerReference`, and apply with
   `controllerutil.CreateOrUpdate` — mutating **only** the fields the operator
   owns, so it never fights another controller over fields it does not manage.
5. Recompute status and update via the **status subresource** only.
6. Return `ctrl.Result{RequeueAfter: spec.WateringInterval}`.

**Idempotence is a hard requirement.** Reconciling an unchanged `Plant` must
perform zero writes. Task 4 asserts this by counting resourceVersion changes.

**Status computation** (`status.go`), given the Plant and its Deployment:
- `ReadyReplicas` / `DesiredReplicas` from the Deployment status/spec.
- `HealthPercent` = `round(readyReplicas / desiredReplicas * 100)`; `0` when
  desired is `0`.
- `Mood` derived by reusing **`internal/mood`** — do not reimplement the ladder.
  Build `mood.Signals` with `Ready: readyReplicas > 0`,
  `ErrorRate: 1 - (ready/desired)`, and zero latency inputs, then `FromScore`.
  Reusing the package is deliberate: one mood ladder, two consumers.
- `LastWatered` set to now on each successful reconcile.
- `ObservedGeneration` set to `metadata.generation`.
- Conditions per the table in Global Constraints, via `meta.SetStatusCondition`.
- **Only write status when it actually changed** — compare against the existing
  status and skip the update otherwise, or the `WateringInterval` requeue turns
  into an infinite write loop against the API server. This is the single most
  common operator bug; guard it and comment why.

`SetupWithManager` owns `Deployment`, `Service`, `ConfigMap`, and
`PodDisruptionBudget` so child changes re-trigger reconciliation.

Emit Events: `PlantCreated`, `PlantUpdated`, `PlantDegraded`, `PlantRecovered`,
`PlantDeleting`.

**Verify:** `make test lint` clean. Unit tests here may use a fake client; the real
verification is Task 4.

---

## Task 4 — envtest suite

**Files:** `internal/controller/suite_test.go`,
`internal/controller/plant_controller_test.go`, `Makefile` (add `envtest` target)

This is the highest-signal artifact in the repository. Most portfolio operators
have no controller tests at all; this one runs against a real API server.

- `setup-envtest` pinned in `.tools/`, with a Makefile target that downloads the
  Kubernetes 1.36.x control-plane binaries and exports `KUBEBUILDER_ASSETS`.
- `suite_test.go` boots `envtest.Environment` with the generated CRD directory,
  starts the manager with the reconciler registered, and tears down cleanly.
- Tests must be independent: each creates its `Plant` in a **fresh namespace** so
  they cannot interfere. Use `Eventually`-style polling with explicit timeouts,
  never a bare `time.Sleep`.

**Required cases:**
1. Creating a `Plant` creates all four children, each with an owner reference
   pointing at the `Plant` and `Controller: true`.
2. The finalizer is added on creation.
3. Status reaches `ReadyReplicas: 0`, `DesiredReplicas: N`, and a `Ready=False`
   condition with reason `ReplicasNotReady`. *(envtest runs no kubelet, so pods
   never become ready — assert the not-ready path honestly rather than faking
   readiness, and comment that this is why.)*
4. **Drift correction:** externally mutate the child Deployment's replica count,
   then assert the reconciler restores it.
5. **Idempotence:** capture each child's `resourceVersion`, force a reconcile by
   touching an unrelated annotation on the Plant, and assert the children's
   `resourceVersion`s are unchanged.
6. Updating `spec.replicas` propagates to the Deployment and to
   `status.desiredReplicas`.
7. Updating `spec.resourceProfile` changes the container's resources.
8. `observedGeneration` tracks `metadata.generation` after a spec change.
9. Deleting the `Plant` removes the finalizer and the object disappears.
   *(envtest has no garbage collector, so assert the owner references are correct
   rather than asserting the children vanish — and comment that distinction. A test
   claiming to prove GC under envtest would be false.)*
10. Two `Plant`s in the same namespace do not interfere with each other's children.

**Verify:** `make envtest` downloads assets; `go test ./internal/controller/... -v`
all pass; report the run time (a suite over ~60s is too slow — tune the polling).

---

## Task 5 — Operator binary, RBAC, and deployment

**Files:** `cmd/plant-operator/main.go`, `config/rbac/{role.yaml,role_binding.yaml,service_account.yaml,leader_election_role.yaml,leader_election_role_binding.yaml}` (role generated by controller-gen),
`deploy/kustomize/operator/{kustomization.yaml,deployment.yaml,namespace.yaml}`,
`build/Dockerfile.plant-operator`, `Makefile` targets, `.github/workflows/ci.yaml` (extend)

- `main.go`: `ctrl.NewManager` with metrics on `8081`, health/readiness probes on
  `8082`, **leader election enabled** with id `plant-operator.buddy.k8s-buddy.io`,
  graceful shutdown via `ctrl.SetupSignalHandler()`. Flags for metrics address,
  probe address, and leader election, following controller-runtime convention.
  `version`/`commit` vars injected via `-ldflags`, logged at startup.
- RBAC generated from `// +kubebuilder:rbac` markers on the reconciler.
  **Least privilege:** full verbs on `plants`, `plants/status`, `plants/finalizers`;
  create/get/list/watch/update/patch/delete on `deployments`, `services`,
  `configmaps`, `poddisruptionbudgets`; create/patch on `events`. Nothing cluster-wide
  that is not needed — a reviewer will read this file specifically, and a
  `cluster-admin`-shaped role would undo the whole demonstration.
- Operator Dockerfile mirrors Plan 1's: multi-stage, cross-compiling with
  `--platform=$BUILDPLATFORM` and `GOOS/GOARCH=$TARGETOS/$TARGETARCH`, distroless
  nonroot final stage, OCI labels.
- Operator Deployment: 1 replica, the standard hardened security context, resource
  requests/limits, liveness `/healthz` and readiness `/readyz` on `8082`.
- Makefile: `docker-build-operator`, `kind-load-operator`, `deploy-operator`,
  `undeploy-operator`, and `install-crd` / `uninstall-crd`.
- CI: extend the existing e2e job — after the Plan 1 demo passes, install the CRD,
  deploy the operator, apply the `fernie` sample, wait for
  `status.readyReplicas == spec.replicas`, assert `kubectl get plants` shows a mood,
  then delete the Plant and assert the children are garbage-collected.

**Verify (on the live kind cluster, for real):**
`make install-crd docker-build-operator kind-load-operator deploy-operator`;
operator pod Running and Ready; `kubectl apply -f config/samples/plant-fernie.yaml`;
`kubectl get plants` shows populated `MOOD`, `HEALTH`, and `READY` columns;
`kubectl describe plant fernie` shows the conditions and Events;
the four children exist with correct owner references
(`kubectl get deploy,svc,cm,pdb -l buddy.k8s-buddy.io/plant=fernie`);
curl the plant's Service and get a `/status` response;
`kubectl delete plant fernie` and confirm **all four children are gone** — this is
the garbage-collection proof, and it must be demonstrated on a real cluster because
envtest cannot show it;
`kubectl auth can-i --as=system:serviceaccount:<ns>:plant-operator delete nodes`
returns **no**, proving the RBAC is actually scoped.

---

## Out of scope for Plan 2

Admission webhooks (defaulting and validating), chaos-buddy, the Prometheus/Grafana/Loki
stack, the Helm chart, Kustomize dev/prod overlays, and the README/runbook — all Plan 3.

**Deliberately deferred from the original spec:** the operator does *not* yet create
an HPA or a ServiceMonitor. An HPA without metrics-server would sit permanently
`ScalingActive=False`, and a ServiceMonitor requires the Prometheus Operator CRDs
that Plan 3 installs. Creating either now would produce a resource that looks right
and does nothing. Both move to Plan 3, where their prerequisites exist.
