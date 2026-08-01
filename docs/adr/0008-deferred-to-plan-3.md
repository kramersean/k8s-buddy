# 8. Capabilities deliberately deferred to Plan 3

## Status

Accepted. Amended twice since:

- Plan 3 Task 2: the ServiceMonitor deferral (#2) is **closed** — it is
  delivered below. The HPA omission (#1) is **not** closed; it is upgraded
  from a deferral to a **permanent decision**, for the reason given in its
  own section.
- 2026-08-01, commit `433f5b7`: the `PlantSpec.chaos` drop (#3) is
  **REVERSED** — the field is back, narrowly scoped, opt-in, and wired into
  the operator. See its section below for what changed and why.

## Context

Three things the original design spec
(`docs/superpowers/specs/2026-07-31-k8s-buddy-platform-showcase-design.md`)
describes are absent from the shipped operator. Two were recorded only inside a
plan document's "Out of scope" section; one was dropped with no record at all.

A plan document is a work log. Nobody reading `internal/controller/resources.go`
and counting owned children, or reading `api/v1alpha1/plant_types.go` and
counting spec fields, is going to find a rationale buried in a per-plan section
of a file they have no reason to open. An absence with no discoverable reasoning
reads as an oversight, which is the same thing as an oversight for the audience
this project is written for.

This ADR is where those absences live, so a reader who notices one finds the
reasoning next to every other design decision in the repo.

## Decision

The following are deliberately deferred to Plan 3. None is an oversight; each is
blocked on a prerequisite that Plan 3 installs.

### 1. HorizontalPodAutoscaler — PERMANENT, not merely deferred

The spec lists an HPA among the operator's owned resources. It is not created,
and — as of Plan 3 Task 2, which installs kube-prometheus-stack — it is now
clear it should not be, ever, on this cluster.

An HPA scaling on CPU or memory requires `metrics-server` to be running and
serving `metrics.k8s.io`. This is a bare `kind` cluster with no metrics pipeline
at all, so an HPA created today would come up and sit permanently at
`ScalingActive=False` with `FailedGetResourceMetric`. That is worse than absent:
it is a resource that looks correct in `kubectl get hpa` and does nothing, which
invites a reviewer to conclude the autoscaling works when it has never once made
a scaling decision.

The original text of this section expected Plan 3's observability stack to
supply the missing prerequisite. It does not, and on reflection could not have:
`metrics-server` and `kube-prometheus-stack` are two independent metrics
pipelines that happen to share the word "metrics". Installing Prometheus gives
the cluster a time-series database that SCRAPES `/metrics` endpoints; it does
not make the cluster's `metrics.k8s.io` aggregated API exist, which is the one
and only thing `autoscaling/v2`'s `Resource` metric source (CPU/memory) actually
reads from. (A [Prometheus Adapter](https://github.com/kubernetes-sigs/prometheus-adapter)
can bridge the two, translating PromQL into a `custom.metrics.k8s.io` or
`external.metrics.k8s.io` API an HPA can target — but that is a second Helm
release, a second CRD, and a second thing to keep healthy, entirely out of
proportion to what a portfolio demo's autoscaling story needs to prove.)

So the HPA is not "next"; it is **permanently out of scope for this project**,
for the same one-line reason as always — kind ships no metrics-server, so an
HPA would sit `ScalingActive=False` forever, and a resource that looks right
and does nothing is worse than an absent one — now stated as a decision rather
than a placeholder waiting on a prerequisite that was never actually coming.

Partial groundwork stays in place regardless: the `Plant` CRD carries the
**scale subresource** (`specpath: .spec.replicas`, `statuspath:
.status.readyReplicas`), so `kubectl scale plant fernie --replicas=5` works
today and will keep working. Its `selectorpath` stays deliberately empty —
that field is consulted only by an HPA reading pod metrics, and pointing it
somewhere would be declaring support for something that, per this section, is
not coming.

### 2. ServiceMonitor — CLOSED in Plan 3 Task 2

The spec lists a ServiceMonitor among the operator's owned resources. It was not
created in Plan 2. **It is now the operator's seventh owned child**, added by
Plan 3 Task 2 (`ServiceMonitorFor` in `internal/controller/resources.go`,
wired into `PlantReconciler.reconcileChildren` in `plant_controller.go`).

`ServiceMonitor` is a CRD owned by the Prometheus Operator. On a cluster where
that CRD is not installed, creating one fails outright with `no matches for
kind "ServiceMonitor"`, and an operator that unconditionally reconciled one
would go into a permanent error loop on a cluster with no Prometheus — so the
child is guarded behind a runtime check
(`PlantReconciler.serviceMonitorCRDAvailable`) that asks the client's
RESTMapper whether `monitoring.coreos.com/v1, Kind=ServiceMonitor` resolves,
on every reconcile of every Plant. When it does not, the Plant reconciles its
other six children exactly as before, logs the absence once per process
lifetime (not once per Plant per reconcile), and stays Ready — a missing
optional CRD is not a degraded Plant. When the CRD later appears (Prometheus
installed after the operator was already running), the very next reconcile of
every Plant starts creating its ServiceMonitor, with no operator restart
required.

`ServiceMonitorFor` returns `*unstructured.Unstructured` rather than a typed
object from the `prometheus-operator/prometheus-operator` API module — a
dependency this repo has no other reason to take on. `unstructured` also
composes naturally with the availability guard above: the builder itself stays
unconditional and cluster-independent (testable with zero cluster, exactly like
every other builder in `resources.go`), and the ONE place that needs to know
whether the CRD actually exists is the reconciler, not the builder.

The workload already exposes `/metrics` in Prometheus format and the operator
already serves its own controller-runtime metrics on `:8081` — Plan 3 Task 2
additionally gates that endpoint with `filters.WithAuthenticationAndAuthorization`
and adds the RBAC and metrics Services letting Prometheus reach it. Nothing about
the scrape TARGETS changed the shape they already had; this closes the object
that tells Prometheus to scrape them.

### 3. `PlantSpec.chaos` — REVERSED 2026-08-01 (`433f5b7`)

The spec describes a `chaos` field on `PlantSpec`, carrying `enabled`, `mode`,
and `schedule`. **It was dropped during Plan 2 with no record, and this section
is that record.**

The field would have been inert: nothing reads it. Chaos injection is
`chaos-buddy`'s job — a separate binary, with deliberately narrow RBAC
(list/delete pods matching one label selector, in one namespace, and nothing
else), which does not exist yet. A `chaos` block on `PlantSpec` today would be
API surface a user can set, that validates, that shows up in `kubectl explain`,
and that has no effect whatsoever.

Adding an inert field to a versioned API is worse than omitting it, because the
API is the part that is hardest to change later: fields are effectively
permanent, whereas an absent field can be added in `v1alpha1` at any time without
breaking a single existing object. Plan 3 adds `chaos` at the same time as the
controller that reads it.

**This decision did not survive Plan 3.** By the time `chaos-buddy` existed and
implemented a readiness-flap mode, the "inert field" argument above had become
the opposite problem: `chaos-buddy`'s readiness-flap mode had no way to reach a
Plant at all, because every Plant's ConfigMap hardcoded its chaos endpoint off.
The mode could only be exercised against a fake client in tests, never proven
against the live cluster — the same shape of gap the project had already cut
latency/cpu-burn/oom chaos modes to avoid.

Commit `433f5b7` restores a `chaos` field, but narrower than the original spec:
`PlantSpec.Chaos` is a single `ChaosSpec{EnableEndpoints bool}`
(`api/v1alpha1/plant_types.go`), not the original three-field `enabled`/`mode`/
`schedule` block. `EnableEndpoints` defaults to `false`
(`+kubebuilder:default=false`) and is threaded by `ConfigMapFor`
(`internal/controller/resources.go`) into the workload's
`BUDDY_ENABLE_CHAOS_ENDPOINTS` environment variable, which gates whether the
Plant's pods expose `POST /chaos/readiness` at all. A Plant opts in
per-instance (see `config/samples/plant-chaos.yaml`); the default posture for
every other Plant is unchanged — no chaos endpoint reachable unless the owner
asks for one. That closes the "inert field" objection this section originally
raised: the field is no longer inert, and it does not weaken the default
security posture, because reaching the endpoint still requires an explicit
opt-in on the specific Plant being targeted.

## Consequences

- `Plant` now owns **seven** children (Deployment, Service, ConfigMap,
  PodDisruptionBudget, ServiceAccount, NetworkPolicy, ServiceMonitor), not six
  and not eight. The count in `resources.go`, the determinism test, and CI's
  live-cluster e2e assertion all agree on seven whenever the ServiceMonitor CRD
  is installed; envtest's own control plane never installs that third-party CRD
  (see `suite_test.go`), so its shared six-children helper stays six by
  construction — the seventh child's existence is proven on the live cluster
  instead (both in this task's own verification and in CI's e2e job), and its
  GRACEFUL ABSENCE is proven in envtest.
- `kubectl autoscale plant` will never do anything useful on this cluster. This
  is now permanent, not pending — see section 1's amendment above. `kubectl
  scale plant` keeps working, unaffected.
- `kubectl explain plant.spec` now shows a `chaos` field again, as of
  `433f5b7` (section 3, REVERSED). It is one field —
  `chaos.enableEndpoints` (`boolean`, default `false`) — not the three-field
  `enabled`/`mode`/`schedule` block the original design spec described. A
  reader comparing the CRD against the design spec will find that narrower
  shape and find this ADR explaining why.
- Chaos injection is still not a per-Plant controller: `chaos-buddy` remains a
  separate workload that acts on Plants from the outside (list/delete pods,
  and now also `POST /chaos/readiness` when a Plant opts in). `PlantSpec.chaos`
  only ever controls what a Plant's own pods *expose*; it does not make the
  Plant controller itself schedule or perform chaos.
