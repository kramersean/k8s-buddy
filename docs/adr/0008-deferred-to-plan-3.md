# 8. Capabilities deliberately deferred to Plan 3

## Status

Accepted

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

### 1. HorizontalPodAutoscaler

The spec lists an HPA among the operator's owned resources. It is not created.

An HPA scaling on CPU or memory requires `metrics-server` to be running and
serving `metrics.k8s.io`. This is a bare `kind` cluster with no metrics pipeline
at all, so an HPA created today would come up and sit permanently at
`ScalingActive=False` with `FailedGetResourceMetric`. That is worse than absent:
it is a resource that looks correct in `kubectl get hpa` and does nothing, which
invites a reviewer to conclude the autoscaling works when it has never once made
a scaling decision.

Plan 3 installs kube-prometheus-stack, which brings the metrics prerequisite with
it, and the HPA becomes a seventh owned child there.

Partial groundwork is already in place: the `Plant` CRD carries the **scale
subresource** (`specpath: .spec.replicas`, `statuspath: .status.readyReplicas`),
so `kubectl scale plant fernie --replicas=5` works today. Its `selectorpath` is
deliberately empty — that field is consulted only by an HPA reading pod metrics,
so pointing it somewhere now would be declaring support for something that cannot
work. Plan 3 sets it to `.status.selector` and adds the corresponding string
field to `PlantStatus`.

### 2. ServiceMonitor

The spec lists a ServiceMonitor among the operator's owned resources. It is not
created.

`ServiceMonitor` is a CRD owned by the Prometheus Operator. On a cluster where
that CRD is not installed — which is every cluster this project currently runs
on — creating one fails outright with `no matches for kind "ServiceMonitor"`, and
an operator that unconditionally reconciles one would go into a permanent error
loop on a cluster with no Prometheus. Guarding it behind a runtime
API-availability check is real, legitimate operator engineering, and it is Plan 3
work, done once the CRD is actually present to check for.

The workload already exposes `/metrics` in Prometheus format and the operator
already serves its own controller-runtime metrics on `:8081`, so nothing about
the scrape targets changes in Plan 3 — only the object that tells Prometheus to
scrape them.

### 3. `PlantSpec.chaos`

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

## Consequences

- `Plant` today owns six children, not eight. The count in `resources.go`, the
  envtest suite, and CI's e2e assertion all agree on six, and all three change
  together when Plan 3 adds more.
- `kubectl scale plant` works now; `kubectl autoscale plant` will not do anything
  useful until Plan 3 supplies `selectorpath` and a metrics source.
- `kubectl explain plant.spec` shows six fields and no `chaos`. A reader
  comparing the CRD against the design spec finds the gap and finds this ADR.
- Plan 3 is where all three land. If Plan 3 changes shape and any of them is
  dropped permanently rather than deferred, this ADR should be superseded with
  one that says so, not silently left describing a plan that no longer exists.
