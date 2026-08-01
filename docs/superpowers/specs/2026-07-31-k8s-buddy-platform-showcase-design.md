# K8s Buddy — Platform Showcase Design

**Date:** 2026-07-31
**Status:** Approved
**Supersedes:** the untracked prototype intent in `AGENTS.md` and `.cursor/agents/`

## Purpose

K8s Buddy is a portfolio project whose job is to prove Kubernetes competence to a
technical reviewer in under five minutes. It does that by making cluster
self-healing *visible*: a friendly "talking plant" workload reports its mood,
controlled chaos wilts it, and Kubernetes brings it back while Prometheus and
Grafana record the whole arc.

The project must read as production engineering, not as a tutorial. Two reviewers
matter:

- A recruiter or hiring manager who reads only the README and looks at screenshots.
- A senior Kubernetes engineer who opens `internal/controller/` and the CI workflows.

Both must come away convinced.

## The Central Design Decision

The plant is a **Custom Resource**, not a hardcoded Deployment.

```
$ kubectl get plants
NAME     SPECIES   MOOD          HEALTH   READY   AGE
fernie   fern      leafy         100%     3/3     4m12s
spike    cactus    not-too-hot   62%      2/3     2m01s
```

Applying a `Plant` manifest causes a custom operator to create and continuously
reconcile the workload behind it. This is the difference between demonstrating
"I can write YAML" and "I understand the Kubernetes control plane," and it is
what the rest of the design hangs from. It forces genuine use of CRDs,
reconciliation loops, status subresources with conditions, owner references and
garbage collection, finalizers, admission webhooks, leader election, and
least-privilege RBAC.

## Success Criteria

The project is done when all of the following hold:

1. `make demo` on a clean machine with Docker produces a running multi-node kind
   cluster, a deployed plant, and an open Grafana dashboard, with no manual steps.
2. `kubectl apply` of a `Plant` resource creates a Deployment, Service,
   ConfigMap, PDB, HPA, and ServiceMonitor owned by that resource, and deleting
   the `Plant` garbage-collects all of them.
3. `kubectl get plants` shows a live mood and health percentage that changes in
   response to chaos.
4. Chaos injection visibly degrades the plant, and Kubernetes recovers it without
   human intervention, with the degradation and recovery both legible in Grafana.
5. CI is green and includes a job that spins up kind, injects chaos, and asserts
   recovery — the demo is proven by machine, not by a screenshot.
6. Every container runs non-root, read-only-rootfs, with all capabilities dropped,
   under Pod Security Admission `restricted`.
7. The README lets a stranger understand the architecture and run the demo without
   asking a question.

## Architecture

Four components, all Go, in a single module.

### buddy-api

The plant itself. An HTTP service with deliberately simple handlers.

- `GET /healthz` — liveness. Reports process health only. Never fails for
  business reasons, so the kubelet does not restart a merely-unhappy pod.
- `GET /readyz` — readiness. Reflects simulated failure state, so chaos removes
  the pod from Service endpoints without killing it.
- `GET /status` — plant-themed mood summary as JSON, with a human-readable message.
- `GET /work` — simulated unit of work with configurable latency and error rate.
- `GET /metrics` — Prometheus exposition.

Behavior requirements:

- Structured JSON logs via `log/slog`, including trace and span IDs.
- OpenTelemetry trace context propagation across `/work`.
- Graceful shutdown: on SIGTERM, fail readiness first, drain in-flight requests,
  then exit. Paired with a `preStop` sleep so rollouts are genuinely zero-downtime.
- Mood is a pure function of health inputs, isolated in `internal/mood` so it is
  unit-testable without HTTP or Kubernetes.

Mood ladder, from healthiest to least healthy:

| Mood | Message |
|---|---|
| `leafy` | "I'm feeling leafy and stable" |
| `sprouting` | "I'm ready to rock and roll" |
| `thirsty` | "Could use a drink, but I'm managing." |
| `lost-a-leaf` | "Lost a leaf, but I'm recovering." |
| `not-too-hot` | "I'm not feeling too hot." |
| `wilting` | "I'm wilting. Send help." |

### plant-operator

A controller-runtime operator owning the `Plant` custom resource in group
`buddy.k8s-buddy.io`, version `v1alpha1`.

**Spec fields:** `species`, `replicas`, `wateringInterval`, `resourceProfile`,
`chaos` (enabled + mode + schedule), `image`.

**Status fields:** `mood`, `healthPercent`, `readyReplicas`, `observedGeneration`,
`lastWatered`, and `conditions` following the standard Kubernetes condition
convention (`Ready`, `Degraded`, `Progressing`).

**Owned resources:** Deployment, Service, ConfigMap, PodDisruptionBudget,
HorizontalPodAutoscaler, ServiceMonitor. All carry owner references to the
`Plant`, so deletion cascades via garbage collection.

**Required controller behaviors:**

- Idempotent reconciliation — reconciling an unchanged resource performs no writes.
- A finalizer that performs cleanup of non-owned external state before deletion.
- Status updated via the status subresource, never via a full-object update.
- `observedGeneration` tracking so stale status is detectable.
- Leader election enabled, so multiple replicas are safe.
- Printer columns on the CRD so `kubectl get plants` is informative without `-o yaml`.
- A defaulting webhook that fills unset fields, and a validating webhook that
  rejects nonsense (notably `replicas: 0`, with the message "plants need at
  least one leaf").

**Testing:** controller logic is tested against a real API server using
`envtest`, covering create, update, drift correction, status transitions, and
finalizer-driven deletion. This is the single highest-signal artifact in the
repository and is not optional.

### chaos-buddy

A controlled failure injector, deployed as a Deployment with a scoped
ServiceAccount.

Modes: `pod-kill`, `readiness-flap`, `latency`, `cpu-burn`, `oom`.

Requirements:

- RBAC is deliberately minimal: it may `list` and `delete` pods matching a single
  label selector, in a single namespace, and nothing else. The narrow Role is
  itself part of the demonstration and is called out in the README.
- Every chaos action emits both a Prometheus counter and a Kubernetes Event on
  the affected object, so `kubectl describe` tells the story too.
- A global kill switch via ConfigMap, and a hard refusal to run if the target
  namespace is not the configured one. Chaos must be impossible to point at
  anything unintended.

### Observability

- kube-prometheus-stack (Prometheus, Grafana, Alertmanager) installed as a
  chart dependency.
- Loki for logs, with the buddy-api JSON logs parsed into labels.
- Grafana dashboards committed as JSON under `observability/dashboards/` and
  provisioned automatically — never clicked together by hand.
- `PrometheusRule` definitions implementing multi-window, multi-burn-rate SLO
  alerting on a stated availability objective, not naive threshold alerts.

The primary dashboard must place, on one screen: current mood, health percentage,
ready-versus-desired replicas, request rate split by outcome, latency
percentiles, restart count, and chaos events as annotations. A reviewer should be
able to point at the exact moment chaos hit and the exact moment recovery
completed.

## Delivery Layer

- **Helm chart** at `charts/k8s-buddy` with `values.schema.json`, a useful
  `NOTES.txt`, and `helm-unittest` tests covering the templating logic.
- **Kustomize overlays** at `deploy/kustomize/{base,overlays/{dev,prod}}`,
  demonstrating fluency in both packaging idioms.
- **kind cluster config** with one control-plane and two workers, so pod
  anti-affinity, topology spread constraints, and PodDisruptionBudget behavior
  during a node drain are real rather than theoretical.
- **Makefile** as the single entry point. `make demo` performs the entire
  end-to-end setup. Every CI step is a Makefile target, so CI and local
  development cannot drift.

## CI/CD

GitHub Actions, with these jobs:

1. `lint` — `golangci-lint`, `go vet`, `gofmt` check.
2. `test` — unit tests plus the envtest controller suite, with coverage reported.
3. `build` — multi-arch container images, distroless base, SBOM generation.
4. `scan` — Trivy scan of images and filesystem; fails on HIGH or CRITICAL.
5. `chart` — `helm lint`, `helm template`, `helm-unittest`, `kubeconform` against
   real Kubernetes schemas.
6. `e2e` — the important one. Creates a kind cluster, deploys the stack, applies
   a `Plant`, waits for `Ready`, injects chaos, asserts the plant degrades, then
   asserts it returns to `Ready` within an SLO. This makes the demo a tested
   property of the repository rather than a claim in the README.

## Security Posture

Applied to every workload without exception:

- Distroless base image, non-root UID, read-only root filesystem.
- `allowPrivilegeEscalation: false`, all Linux capabilities dropped,
  `seccompProfile: RuntimeDefault`.
- Namespace labeled for Pod Security Admission `restricted` enforcement.
- Default-deny `NetworkPolicy` with explicit allows for exactly the flows
  required (Prometheus scrape, chaos-to-api, DNS).
- Resource requests and limits set on every container.
- No secrets in the repository; anything secret-shaped is generated at demo time.

## Repository Structure

Kubebuilder conventions, so the layout is instantly familiar to any Kubernetes
engineer reading it.

```
cmd/{buddy-api,plant-operator,chaos-buddy}/main.go
api/v1alpha1/                     CRD types and generated deepcopy
internal/api/                     HTTP handlers
internal/mood/                    pure mood logic
internal/chaos/                   chaos engine
internal/telemetry/               metrics, logging, tracing setup
internal/controller/              reconciler and envtest suite
config/{crd,rbac,manager,webhook}/
charts/k8s-buddy/
deploy/kind/  deploy/kustomize/{base,overlays}/
observability/{dashboards,rules}/
docs/{architecture,adr,runbook}/
hack/
.github/workflows/
.claude/agents/
Makefile  README.md
```

Each package has one clear purpose and can be understood without reading its
consumers. `internal/mood` in particular has no Kubernetes or HTTP dependency,
which keeps the interesting logic trivially testable.

## Documentation

- **README.md** is the portfolio artifact: a Mermaid architecture diagram, a
  sixty-second quickstart, Grafana screenshots captured during a real chaos run,
  a table mapping each feature to the Kubernetes concept it demonstrates, and
  links to the ADRs.
- **ADRs** under `docs/adr/` recording the decisions a reviewer would question:
  why an operator, why kind over minikube, why Helm and Kustomize both, why
  readiness-based chaos rather than liveness-based.
- **Runbook** under `docs/runbook/` treating the demo as if it were a production
  service, including how to diagnose each failure mode.

## Migration of Existing Content

The repository currently contains only intent documents. `AGENTS.md` is rewritten
to describe the real project. The seven `.cursor/agents/*.md` files are malformed
— each file's entire body was collapsed into its YAML `description:` field, so
none function as intended. They are converted into correct `.claude/agents/*.md`
definitions with proper frontmatter and real bodies, preserving the original
subagent intent.

## Build Sequence

Each stage ends in a demonstrable state, so the project is never in a broken
half-built condition.

1. Scaffold: Go module, Makefile, CI skeleton, repo layout, converted agents.
2. buddy-api: mood engine, handlers, metrics, structured logs, unit tests.
3. kind cluster and raw manifests — the first working self-healing demo.
4. Plant CRD and operator, with the envtest suite.
5. chaos-buddy and its scoped RBAC.
6. Observability stack, dashboards, SLO alerting rules.
7. Helm chart, Kustomize overlays, webhooks, NetworkPolicies.
8. Full CI e2e job, README, ADRs, runbook, captured screenshots.

## Explicit Non-Goals

- No deployment to real cloud infrastructure. The demo is local-only, on kind.
- No service mesh. It would add operational surface without adding signal.
- No multi-cluster or federation.
- No user authentication or persistence. The plant is stateless by design.
- No Argo CD or GitOps controller. Considered and deferred; it adds a second
  control loop to explain without strengthening the core demonstration.
