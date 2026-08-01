# Architecture

This is the deeper technical narrative the top-level [README](../../README.md)
links to. It assumes you have already read that file's Mermaid diagram and
quickstart; this document explains *how* the pieces actually work, for the
reader who is about to open `internal/controller/` or `api/v1alpha1/` and
wants a map first.

Four components, one Go module (`github.com/sean-kramer/k8s-buddy`):

- **buddy-api** (`cmd/buddy-api`) — the plant itself, an HTTP service.
- **plant-operator** (`cmd/plant-operator`) — the controller-runtime operator
  that owns the `Plant` custom resource.
- **chaos-buddy** (`cmd/chaos-buddy`) — a narrowly-scoped failure injector.
- **Observability** — kube-prometheus-stack + Loki, dashboards and alerting
  rules committed as code.

This document covers four things in depth: the reconcile loop and its
idempotence guarantees, the admission chain's exact ordering, how mood and
health are derived, and why the operator reproduces the static base
manifests in Go instead of replacing or templating them.

## The reconcile loop

`PlantReconciler.Reconcile` (`internal/controller/plant_controller.go`) is a
five-step loop, and every step is deliberately narrow:

1. **Fetch.** `Get` the `Plant`. A `NotFound` means it was already deleted —
   not an error, just a return. A `Plant` already carrying a
   `deletionTimestamp` also returns immediately: there is no finalizer (see
   [ADR 0007](../adr/0007-no-finalizer-on-plant.md)) and nothing left for the
   operator to do while Kubernetes' own garbage collector removes the
   owned children.
2. **Validate the name.** A `Plant` name longer than 63 characters cannot
   become a label value on its children (every child carries
   `buddy.k8s-buddy.io/plant: <name>`), so a name that long is rejected with
   `Degraded=True/InvalidName` rather than retried forever against an API
   server that would reject every child create identically.
3. **Reconcile every owned child.** `reconcileChildren` builds and applies
   the Deployment, Service, ConfigMap, PodDisruptionBudget, ServiceAccount,
   NetworkPolicy, and (conditionally) ServiceMonitor — see
   [Seven owned children](#seven-owned-children-and-graceful-degradation)
   below.
4. **Recompute and write status**, through the status subresource only.
5. **Requeue** after `Plant.Spec.WateringInterval`, clamped to a 30-second
   floor (`minRequeueInterval`), so status keeps refreshing even when
   nothing external changed.

### Idempotence: how "no writes on an unchanged Plant" is actually enforced

The reconciler's central discipline is that reconciling an unchanged `Plant`
performs **zero** writes to the API server. Three mechanisms combine to make
that true rather than merely intended:

- **Field-by-field mutation, never whole-object replacement.**
  `resources.go`'s builders (`DeploymentFor`, `ServiceFor`, ...) are pure
  functions from a `*Plant` to a desired object. `plant_controller.go`'s
  `mutate*` functions then copy *only the fields the operator owns* from
  that desired object onto the live one — never `existing.Spec = desired.Spec`.
  This matters because a whole-object copy would also copy back every
  zero-valued field the builder never set: an immutable `spec.selector`, a
  server-assigned `ClusterIP`, a defaulted `terminationMessagePath`. Those
  get clobbered (for the mutable ones) or rejected outright (for the
  immutable ones) by a whole-object copy, and clobbering a server default
  with a zero value produces a permanent diff that triggers a write on
  *every single reconcile*, forever. `mutateDeployment`, for example,
  assigns `ServiceAccountName`, `SecurityContext`,
  `TopologySpreadConstraints`, and the merged container list explicitly,
  and leaves `Strategy`, `RevisionHistoryLimit`, `RestartPolicy`,
  `DNSPolicy`, and half a dozen other API-server-defaulted fields alone.
- **`applyChild`'s before/after comparison.** `applyChild`
  (`plant_controller.go`) fetches the live object, deep-copies it, runs the
  mutate function, and compares before and after with
  `apiequality.Semantic.DeepEqual`. Only a real difference issues an
  `Update`; an unchanged object reports `OperationResultNone` and nothing is
  sent to the API server. This is the same pattern
  `controllerutil.CreateOrUpdate` uses internally — `applyChild` is a
  variant of it with one addition (see
  [Ownership and the uncached reader](#ownership-and-the-uncached-reader)
  below).
- **`statusChanged`'s exclusion of `LastWatered`.** Every reconcile computes
  a fresh `LastWatered` timestamp by construction, so a naive comparison of
  old and new status would always report "changed" and write on every single
  pass — the single most common operator defect, and one severe enough to be
  called out by name in the source (`status.go` calls it "BUG A" in its own
  comment, as the failure mode the exclusion exists to prevent).
  `statusChanged` zeroes `LastWatered` on both sides before comparing, so a
  `Plant` whose `Mood`, `HealthPercent`, `ReadyReplicas`, and `Conditions`
  are unchanged produces zero status writes, no matter how many times
  `WateringInterval` elapses.

Probe fields are the sharpest illustration of why this discipline has to be
exact rather than approximate: `DeploymentFor` sets `TimeoutSeconds: 1`,
`SuccessThreshold: 1`, and `HTTPGet.Scheme: URISchemeHTTP` explicitly on
every probe, even though the API server would default to the same values if
they were left unset. If they were left unset in the builder, the *desired*
probe object would never equal the *server's own defaulted, stored* probe
object, and every single reconcile would see a phantom diff and issue a
write. Matching the server's defaults byte-for-byte is what keeps an
unchanged `Plant` idempotent in practice, not just in theory.

### Seven owned children, and graceful degradation

A `Plant` owns seven children, six unconditionally and one conditionally:

| # | Kind | Notes |
|---|------|-------|
| 1 | Deployment | The buddy-api workload itself. |
| 2 | Service | ClusterIP, port 80 → container port 8080. |
| 3 | ConfigMap | Non-secret runtime config, consumed via `envFrom`. |
| 4 | PodDisruptionBudget | `minAvailable = replicas - 1`, floored at 1. |
| 5 | ServiceAccount | Dedicated identity, no token mounted. |
| 6 | NetworkPolicy | Default-deny scoped to this Plant's own pods. |
| 7 | ServiceMonitor | Only when the Prometheus Operator CRD is installed. |

The seventh is the interesting one. `ServiceMonitor` is a
`CustomResourceDefinition` owned by the Prometheus Operator, not a built-in
Kubernetes type — creating one on a cluster where that CRD is absent fails
outright with `no matches for kind "ServiceMonitor"`. `reconcileChildren`
guards it with `serviceMonitorCRDAvailable`, which asks the client's
`RESTMapper` whether `monitoring.coreos.com/v1, Kind=ServiceMonitor`
resolves — the same mechanism client-go's own typed and dynamic clients use
internally, so a "no" here means the API server itself would reject the
request, not merely that this process hasn't cached it yet. A missing CRD is
treated as "not available, other six children continue normally, log the
absence once per process lifetime" — never as a fatal error. A negative
result is cached for 30 seconds (a real discovery round-trip on this
controller-runtime version has no rate limiting of its own); a positive
result is never cached, so a cluster that installs Prometheus *after* the
operator is already running starts getting ServiceMonitors on the very next
`WateringInterval`, no restart required. `ServiceMonitorFor` itself returns
an `*unstructured.Unstructured` rather than a typed object from the
prometheus-operator API module — this repo has no other reason to take that
dependency, and `unstructured` is what makes the CRD-optional story
possible without dragging its Go types into the scheme.

### Ownership and the uncached reader

`applyChild`'s initial read goes through `PlantReconciler.APIReader` — an
**uncached**, direct client (`mgr.GetAPIReader()`) — not the manager's
cached client every other read in this codebase uses. This is a correctness
requirement, not a style choice, and the reason is an interaction between
two otherwise-good decisions:

- `cmd/plant-operator` narrows the manager's informer cache for every child
  type to objects labelled `app.kubernetes.io/managed-by=plant-operator`, so
  the operator does not hold every `ConfigMap` and `ServiceAccount` in the
  cluster in memory.
- `mergeLabels` re-asserts that label on every reconcile, so a human
  stripping it (`kubectl label deploy fernie app.kubernetes.io/managed-by-`)
  is supposed to be self-correcting drift, like any other drift.

Through the *cached* client, those two are not compatible: stripping the
label removes the object from the cache, a cached `Get` then reports
`NotFound`, `CreateOrUpdate` takes the create path, and the API server
rejects it with `AlreadyExists` — forever, because the one thing that would
have fixed the label (a normal `Update`) can no longer run. This was
observed on the live cluster, not theorized. Reading through the uncached
`APIReader` closes the gap: the object is always found, the update path
always runs, `mergeLabels` restores the label, and the object re-enters the
cache on its own.

The same read also feeds `assertOwnership`, which refuses to adopt a
pre-existing object that merely happens to share a child's name — a `Plant`
named `buddy-api` created in the `k8s-buddy` namespace would otherwise seize
Plan 1's own static Deployment. Ownership is checked in a specific order,
and the order is load-bearing: a controller owner reference whose UID
matches the `Plant`'s own UID is checked **first** and is definitive (a UID
is server-assigned and unforgeable); only when there is no matching owner
reference does the `app.kubernetes.io/managed-by` label get consulted, as a
weaker signal for the case of a stale child from a just-deleted,
same-named `Plant` still awaiting garbage collection. Checking the label
first — an earlier version of this code did — was a real regression: a
stripped label wedged a `Plant`'s own child at
`Degraded/ConflictingResource` permanently, refusing to touch the very
object it owned.

## The admission chain, in exact order

A `Plant` write passes through four independent enforcement layers, and the
order is precise and worth stating exactly because two things in this
codebase depend on getting it right:

```
default (CRD schema, at decode time)
  -> mutate (MutatingWebhookConfiguration: PlantCustomDefaulter)
    -> validate (CRD structural schema + CEL rules)
      -> validate (ValidatingWebhookConfiguration: PlantCustomValidator)
```

1. **CRD schema defaulting, at decode time.** Every `+kubebuilder:default`
   marker on `PlantSpec` (`api/v1alpha1/plant_types.go`) is applied by the
   API server before *any* admission plugin — mutating or validating — ever
   sees the object. A `Plant` with only `spec.species` set is already fully
   defaulted by the time the mutating webhook runs.
2. **Mutating admission webhook** (`PlantCustomDefaulter.Default`). Because
   of step 1, this webhook is, honestly, almost always a no-op against real
   traffic — every field it would fill in has usually already been filled in
   by CRD-level defaulting. It exists anyway, both as defense in depth for a
   `Plant` constructed directly against the API without a client-side
   OpenAPI round trip, and — critically — it must agree *exactly*, field for
   field, with the CRD's own defaults. `plant_webhook_test.go`'s
   `TestDefaultingAgreesWithCRDSchema` reads the generated CRD and fails the
   build the moment the two diverge, rather than trusting two
   hand-maintained copies of the same six values to stay in sync by
   convention. This is why the mutating webhook is safe to leave
   `failurePolicy: Ignore` forever — see
   [ADR 0009](../adr/0009-webhook-certificate-strategy.md) for the full
   argument.
3. **CRD structural schema and CEL validation.** `Minimum=1` on
   `spec.replicas`, the `Pattern` on `spec.image`, and the CEL rules on
   `spec.replicas` (`self >= 1`, message `"plants need at least one leaf"`)
   and `spec.wateringInterval` (`>= 30s`, `<= 24h`) all run here, as part of
   the object's own strategy validation — still before the validating
   webhook. A bare `kubectl apply --validate=false` does not skip this: that
   flag only disables *client-side* OpenAPI validation before the request is
   even sent; the API server's own structural/CEL validation always runs
   server-side regardless of what the client asked for.
4. **Validating admission webhook** (`PlantCustomValidator.ValidateCreate` /
   `ValidateUpdate`). Rejects what the schema genuinely cannot express:
   - `replicas: 0` — re-asserted here for defense in depth with the exact
     message `"plants need at least one leaf"` (the CEL rule above already
     rejects it; both messages appear together in the API server's
     aggregated rejection).
   - `wateringInterval` below the operator's own reconcile floor — again
     defense in depth, for the case a future schema change loosens or drops
     the CEL rule without this check being updated in lockstep.
   - `image` from a registry outside the configured allowlist
     (`--allowed-image-registries`, default `ghcr.io/,docker.io/library/`) —
     something no CRD marker can express at all.
   - Any change to `spec.species` on update — immutability the CRD's schema
     has no keyword for either. `ValidateUpdate` compares old and new and
     rejects with both the old and new value named in the message.

   `ValidateDelete` permits every delete unconditionally, and — more
   fundamentally — the webhook's own `+kubebuilder:webhook` rules never list
   the `DELETE` verb at all, so a `kubectl delete plant` never reaches this
   webhook, reachable or not. This is deliberate: the same posture
   [ADR 0007](../adr/0007-no-finalizer-on-plant.md) commits to for
   finalizers ("nothing this operator owns should be able to make a `Plant`
   undeletable"), applied to admission instead of garbage collection. It is
   also why `failurePolicy: Fail` on the *validating* webhook is safe rather
   than a self-lockout risk: an operator outage blocks `Plant`
   **writes** cleanly (a clear "webhook ... failed calling webhook" error,
   not a silent no-op) and un-blocks itself automatically the instant the
   operator is healthy again — never blocks **deletes**. See
   [ADR 0009](../adr/0009-webhook-certificate-strategy.md) for the
   certificate story that makes the webhook server reachable in the first
   place, and its own recovery procedure for the rare case an admin needs
   writes to proceed before the operator can be fixed.

## Mood and health derivation

Mood is computed in two different places, deliberately sharing one engine
rather than one computation feeding the other:

- **buddy-api's own `/status` response** (`internal/mood`) scores itself
  from request-level signals: `Ready` (its own readiness flag), `ErrorRate`
  and `P95Latency` (from a rolling window of recent `/work` observations),
  and `RestartCount`. `Signals.Score()` combines these into a 0–100 value —
  up to 50 points for error rate, 30 for latency (scaled against
  `LatencyBudget`, full marks if no budget is configured), 20 for readiness,
  minus up to 10 for restarts — with one additional rule: if `Ready` is
  false, the total is capped at 35 regardless of how good the other signals
  look, so an unready pod can never report as anything but unhealthy.
  `FromScore` then buckets the result onto the six-mood ladder (`leafy` at
  ≥95 down to `wilting` below 20).
- **The operator's `Plant.status.mood`** (`internal/controller/status.go`)
  reuses the *same* `mood.FromScore` function, but feeds it a much narrower
  signal: only `Ready` (`readyReplicas > 0`) and a derived `ErrorRate`
  (`1 - readyReplicas/desiredReplicas`). The operator has no visibility into
  buddy-api's own request-level error rate or latency from the Deployment
  status alone, so `P95Latency` and `LatencyBudget` are left at their zero
  values — full marks on the latency component by construction. The
  operator's mood is therefore driven entirely by *how many replicas are
  ready*, not by request-level health, which is exactly what a Deployment's
  own status can actually observe.

Reusing one scoring function for both, rather than hand-rolling a second
threshold ladder in `internal/controller`, is what guarantees the two moods
can never disagree about what a given score *means* — `leafy` means the
same thing whether buddy-api or the operator says it. `HealthPercent` on
`Plant.status` is a separate, simpler number: `readyReplicas` as a
percentage of `desiredReplicas`, rounded to the nearest whole percent, 0
when `desiredReplicas` is 0 rather than an undefined division. It does not
run through the mood engine at all — it is the number `kubectl get plants`
shows in its `HEALTH` column, and it is what chaos actually moves in the
dashboard's "Health score" panel when it targets the operator-visible
signal (readiness), as opposed to buddy-api's own request-level `/status`
score, which chaos does not target at all in this project — see the
[README's "what this is not"](../../README.md#what-this-is-not) for why
`latency`, `cpu-burn`, and `oom` chaos modes were dropped rather than
stubbed.

## Why the operator reproduces the base manifests in Go

`deploy/kustomize/base/*.yaml` and `internal/controller/resources.go`
describe the *same* buddy-api workload twice — once as static YAML (what
`make demo` applies) and once as pure Go builders (what the operator
reconciles a `Plant` into). [ADR 0006](../adr/0006-operator-reproduces-base-manifests.md)
covers the reasoning in full; the short version:

- **Deleting the static manifests** would mean the project's first, simplest
  entry point — the one that works with nothing installed but `kubectl`, and
  the one that still proves chaos/recovery when the operator itself is what
  is broken — disappears. An operator repo whose workload can only be
  inspected by reading Go is worse for a recruiter skimming the repo, not
  better.
- **Deleting the Go builders**, and having the operator apply the static
  YAML directly (embed it, template it, apply it), would turn the operator
  into a YAML templating engine with a `Plant`-shaped front end. The
  reconciliation loop — diffing desired state against observed state,
  field by field, correcting only what it owns — is the actual thing this
  project exists to demonstrate, and templating reintroduces every problem
  the field-by-field `mutate*` functions exist to avoid: applying a whole
  rendered spec clobbers server-assigned fields and forces a write on every
  single pass.
- **Keeping both**, with the coupling made *enforceable* rather than
  aspirational, is what was chosen. `internal/controller/manifest_drift_test.go`
  parses the static YAML and asserts, field by field, that security
  contexts, probe timings, resource requests/limits,
  `terminationGracePeriodSeconds`, `imagePullPolicy`, container ports, and
  the ConfigMap key set match what the Go builders produce for an
  equivalent `Plant` — and every *intended* difference (a 3-key vs. 2-key
  selector, `managed-by: kustomize` vs. `plant-operator`, three
  namespace-wide `NetworkPolicy` objects vs. one Plant-scoped one, and a
  handful more) is asserted explicitly, with its own reason, rather than
  excluded from comparison. Tightening a probe or a resource limit in one
  description and not the other now fails `make test`, which is what turns
  "these two must agree" from a comment into a property CI actually checks.

Read `resources.go`'s own package doc comment and ADR 0006's differences
table for the complete, current list of what's shared and what's
deliberately not.

## See also

- [README.md](../../README.md) — the portfolio-facing overview, quickstart,
  and feature/concept table.
- [docs/adr/](../adr/) — the numbered decision record, including the three
  referenced directly above (0006, 0007, 0009) and six more covering
  shutdown behavior, NetworkPolicy scoping, liveness/readiness separation,
  the base image choice, and the Plan 3 deferral/closure list.
- [docs/runbook/README.md](../runbook/README.md) — what to do when any of
  this breaks.
