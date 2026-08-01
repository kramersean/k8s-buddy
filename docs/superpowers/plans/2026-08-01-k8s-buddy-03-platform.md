# Plan 3 of 3 — Chaos, observability, delivery, and the README

**Spec:** `docs/superpowers/specs/2026-07-31-k8s-buddy-platform-showcase-design.md`
**Branch:** `feat/platform-showcase`
**Predecessors:** Plan 1 (foundation) and Plan 2 (Plant CRD + operator), both complete.

**Outcome when complete:** a stranger clones the repo, runs one command, and
within a few minutes is looking at a Grafana dashboard while a chaos injector
wilts a plant and Kubernetes brings it back — with the README explaining what
they just saw and why each piece of it is there.

This plan turns a working system into a portfolio artifact.

---

## Global Constraints

Inherited from Plans 1 and 2 and still binding: `go 1.26.0` with no `toolchain`
directive; the six-mood ladder and its exact message strings; the `buddy_`
metric prefix; the hardened security context on every container; PodSecurity
`restricted` on every namespace that runs workloads; distroless non-root images
that cross-compile via `--platform=$BUILDPLATFORM`; every generated object
carrying the five standard labels; `make lint test` clean at the end of every
task; testify + standard `testing`, no Ginkgo.

**New constraints for this plan:**

- **Namespaces.** Observability lands in `k8s-buddy-observability`. Chaos runs in
  `k8s-buddy-plants` alongside the workloads it targets. Both carry the same PSA
  `restricted` labels as the existing namespaces.
- **Chaos metric names** (all `buddy_`-prefixed, joining the seven from Plan 1):

  | Metric | Type | Labels | Meaning |
  |---|---|---|---|
  | `buddy_chaos_actions_total` | Counter | `mode`, `outcome` | Chaos actions attempted |
  | `buddy_chaos_last_action_timestamp_seconds` | Gauge | `mode` | Unix time of last action |
  | `buddy_chaos_enabled` | Gauge | none | 1 when the kill switch permits chaos |

- **The SLO.** buddy-api targets **99% availability of `/work` over 30 days**,
  where a "good" request is any non-5xx response. Alerting uses multi-window,
  multi-burn-rate rules — a 14.4x burn over 1h/5m (page) and a 6x burn over
  6h/30m (ticket). These exact numbers appear in the PrometheusRule and in the
  runbook, and must agree.
- **Chart/overlay names.** Helm chart `charts/k8s-buddy`. Kustomize overlays
  `deploy/kustomize/overlays/{dev,prod}` layering on the existing base.
- **No new cloud dependencies.** Everything runs on the local kind cluster.

---

## Task 1 — chaos-buddy

**Files:** `cmd/chaos-buddy/main.go`, `internal/chaos/{engine.go,engine_test.go,kube.go}`,
`build/Dockerfile.chaos-buddy`, `deploy/kustomize/chaos/{kustomization.yaml,deployment.yaml,rbac.yaml,configmap.yaml}`,
Makefile targets

A controlled failure injector. Its **narrow RBAC is part of the demonstration** —
a reviewer will open `rbac.yaml` specifically.

**Modes:** `pod-kill` (delete one matching pod), `readiness-flap` (POST
`/chaos/readiness` to flip a pod unready for a bounded window), `latency` (not
implemented against the app — omit rather than fake it), `cpu-burn` and `oom`
(omit for the same reason). **Ship only `pod-kill` and `readiness-flap`,** and
state in the README that the other three were dropped rather than stubbed. A mode
that appears in a flag list and does nothing is worse than an absent mode.

**Safety, which is the point:**
- RBAC grants `list` and `delete` on **pods only**, in **one namespace**, via a
  Role (not ClusterRole), bound to a dedicated ServiceAccount.
- A `CHAOS_TARGET_NAMESPACE` env var, and a hard refusal to act if the pods it
  finds are outside it.
- A label selector it will not run without — an empty selector is a startup error,
  never "match everything".
- A kill switch: a ConfigMap key `enabled: "false"` re-read every loop, reflected
  in `buddy_chaos_enabled`. Chaos must be stoppable without a redeploy.
- A `--dry-run` flag that logs intended actions and deletes nothing, on by default
  in the manifest so an accidental deploy is inert.

Each action emits a Prometheus counter and a Kubernetes Event on the target pod,
so `kubectl describe pod` tells the story too. `internal/chaos` holds the
decision logic as pure functions (which pod to pick, whether the switch permits
it, whether the target is in-namespace) and is unit-tested without a cluster;
`kube.go` holds the client calls.

**Verify:** unit tests for selection and every refusal path; deploy to the live
cluster with `--dry-run=true` and show it logging intended actions while pod
count stays constant; flip to `--dry-run=false` and show a real pod deletion plus
recovery; flip the kill switch and show it stop within one loop; prove RBAC by
`kubectl auth can-i --as=<chaos sa> delete pods -n kube-system` returning **no**.

---

## Task 2 — Observability

**Files:** `deploy/observability/{kustomization.yaml,namespace.yaml,values-kube-prometheus-stack.yaml,values-loki.yaml}`,
`observability/dashboards/k8s-buddy.json`, `observability/rules/{slo.yaml,operational.yaml}`,
`internal/controller/resources.go` (add `ServiceMonitorFor`), `internal/controller/plant_controller.go` (own it),
Makefile targets

- Install `kube-prometheus-stack` and `loki-stack` via Helm into
  `k8s-buddy-observability`, with committed values files. Pin chart versions
  exactly; `@latest` is not reproducible.
- **Close Plan 2's deferral:** the operator now creates a `ServiceMonitor` per
  Plant as a **seventh** owned child, guarded so it is skipped gracefully when the
  Prometheus Operator CRDs are absent (check via RESTMapper, log once, continue —
  a missing optional CRD must not wedge reconciliation). Add `servicemonitors` to
  the RBAC markers. Extend the determinism, owner-reference, and CI child-count
  assertions to seven.
- **HPA:** deliberately still **not** created. metrics-server is not installed on
  kind by default, so an HPA would sit `ScalingActive=False` forever. Record this
  in ADR 0008's deferral list as a permanent decision rather than a pending item,
  with the one-line reason.
- **Dashboard** `observability/dashboards/k8s-buddy.json`, provisioned via the
  stack's sidecar (a ConfigMap with the `grafana_dashboard` label), never clicked
  together by hand. One screen must show: current mood, health score,
  ready-vs-desired replicas, `/work` request rate split by outcome, latency
  percentiles, restart count, and chaos actions as annotations. A reviewer should
  be able to point at the moment chaos hit and the moment recovery completed.
- **PrometheusRules:** `slo.yaml` implementing the multi-window multi-burn-rate
  alerting from Global Constraints, and `operational.yaml` for
  `PlantDegraded`, `PlantNotReconciling` (no successful reconcile in 5m), and
  `ChaosRunawayDeletions`.
- **Operator metrics:** wire `filters.WithAuthenticationAndAuthorization` on the
  operator's `:8081` and add a metrics Service so Prometheus can scrape it. This
  closes the last item carried out of Plan 2.

**Verify:** stack installs on the live cluster; Prometheus targets show buddy-api
and the operator **UP**; `buddy_health_score` and `controller_runtime_reconcile_total`
are queryable; the dashboard loads from the committed JSON with no manual steps;
`promtool check rules` passes on both rule files; deliberately break a plant and
show the alert firing in Alertmanager.

---

## Task 3 — Helm chart and Kustomize overlays

**Files:** `charts/k8s-buddy/{Chart.yaml,values.yaml,values.schema.json,NOTES.txt,templates/*,tests/*}`,
`deploy/kustomize/overlays/{dev,prod}/kustomization.yaml`, Makefile targets

- Chart deploys the operator, the CRD, and optionally a sample Plant. `Chart.yaml`
  `apiVersion: v2`; the CRD ships in `crds/` so Helm installs it before the
  templates that depend on it.
- `values.schema.json` genuinely constraining types and enums, so `helm install`
  rejects a malformed `values.yaml` rather than producing a broken release.
- `NOTES.txt` printing the actual next commands, including how to reach Grafana.
- `helm-unittest` tests covering: the operator Deployment renders with the right
  image and security context; `replicaCount` propagates; disabling the sample
  Plant omits it; an invalid `resourceProfile` fails schema validation.
- Kustomize `dev` and `prod` overlays over the existing base — `dev` with 1
  replica and debug logging, `prod` with 3 replicas, tighter resources, and
  `topologySpreadConstraints` set to `DoNotSchedule`. Both must build.

**Verify:** `helm lint`; `helm template` output passes `kubeconform -strict`;
`helm-unittest` green; `helm install --dry-run` succeeds and a bad value is
rejected; both overlays `kubectl kustomize` cleanly and pass kubeconform; install
the chart for real on the live cluster into a scratch namespace, confirm the
operator runs, then uninstall and confirm cleanup.

---

## Task 4 — Admission webhooks

**Files:** `api/v1alpha1/plant_webhook.go`, `api/v1alpha1/plant_webhook_test.go`,
`cmd/plant-operator/main.go` (register), `config/webhook/*`, `deploy/kustomize/operator/*` (cert wiring),
`internal/controller/suite_test.go` (envtest webhook support)

- **Defaulting webhook:** fills unset optional fields, so a `Plant` with only
  `species` set becomes fully specified. It must agree exactly with the CRD's
  `+kubebuilder:default` markers — a defaulting webhook that disagrees with the
  schema default is a genuine trap. Test that agreement explicitly.
- **Validating webhook:** rejects what the OpenAPI schema cannot express —
  notably `replicas: 0` with the message **"plants need at least one leaf"** (the
  spec calls for this exact string), a `wateringInterval` shorter than the
  reconcile floor, an `image` from a registry not in an allowlist, and any change
  to `species` on update (immutable field enforcement, which CRD markers alone
  cannot do).
- Certificates: use a self-signed issuer generated at install time by a Job or
  cert-manager if already present; the demo must not require a manual openssl
  step. If cert-manager is not installed, generate the cert in a `helm` pre-install
  hook or an init container and patch the `caBundle`. Document the choice in an ADR.
- envtest gains webhook support (`envtest.Environment.WebhookInstallOptions`) and
  the suite proves both webhooks fire against a real API server.

**Verify:** envtest cases for defaulting and every rejection; on the live cluster,
`kubectl apply` a `Plant` with `replicas: 0` and paste the rejection showing the
exact message; apply a Plant with only `species` and show the defaults filled;
attempt a `species` change and show it rejected.

---

## Task 5 — The README and the docs that make it presentable

**Files:** `README.md`, `docs/runbook/README.md`, `docs/architecture/README.md`,
`docs/adr/0009+`, `.github/ISSUE_TEMPLATE/`, `CONTRIBUTING.md`, screenshots under `docs/images/`

The README is the artifact a recruiter reads. It must:
- Open with what this is and what it demonstrates, in three sentences.
- Show `kubectl get plants` output immediately — the hook.
- A Mermaid architecture diagram (renders natively on GitHub).
- **A sixty-second quickstart** that actually works from a clean machine.
- Grafana screenshots captured during a real chaos run, showing the dip and the
  recovery. Capture these from the live cluster; do not mock them.
- A table mapping each feature to the Kubernetes concept it demonstrates
  (CRDs, reconciliation, finalizers-and-why-we-removed-ours, owner references,
  admission webhooks, PodSecurity, NetworkPolicy, PDB, topology spread, SLO alerting).
- Links to the ADRs, with a one-line summary of each.
- An honest "what this is not" section: local-only, no multi-cluster, no
  persistence. Stating limits reads as confidence, not weakness.

The runbook treats the demo as a production service: how to diagnose each failure
mode, what each alert means and what to do about it, and how to roll back.

**Verify:** every command in the README executed verbatim on the live cluster and
confirmed to work; every internal link resolves; the Mermaid diagram renders;
screenshots exist and are referenced; `make help` output matches what the README
claims exists.

---

## Task 6 — Clean-room rebuild and full re-test

**Files:** possibly none — this task's output is evidence, plus fixes for whatever
it uncovers.

Everything so far has been verified against a cluster that accumulated state
across three plans. That is not the same as working. This task proves the claim
the README will make.

1. `kind delete cluster --name k8s-buddy` — destroy everything.
2. `docker image rm` the locally built images so nothing is cached.
3. From the repo root, run **only** what the README's quickstart says.
4. Record every command and its output verbatim.
5. Then run the full matrix: `make demo` (static path), `make demo-operator`,
   the observability stack, a chaos run, the webhook rejections, `helm install`,
   both Kustomize overlays, `make test`, and `make test-envtest`.
6. Any failure is a defect to fix, not a step to skip — a portfolio project that
   only works on the machine that built it is the failure mode this whole plan
   exists to avoid.

**Verify:** the clean-room transcript is the verification. Paste it in full into
the task report, and fix everything it surfaces.

---

## Out of scope

Argo CD/GitOps, service mesh, multi-cluster, OpenTelemetry tracing with Tempo
(metrics and logs are enough to tell the story), cloud deployment, and
authentication on buddy-api. The spec's `chaos` field on `PlantSpec` stays
unimplemented: chaos is deployed as its own workload rather than driven per-Plant,
which keeps the blast radius explicit — record that in an ADR rather than leaving
it as a silent omission.
