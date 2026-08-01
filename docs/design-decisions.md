# Design decisions: the study guide

K8s Buddy has nine ADRs under `docs/adr/`. They are the repo's strongest
differentiator, and its biggest liability at the same time: an ADR the author
cannot defend out loud is worse than no ADR at all.

This document turns every non-obvious decision into the question an
interviewer would actually ask, answered the way you'd say it out loud, with
the exact evidence a skeptic could go check. Every `file:line` below was read
directly out of the code while writing this; every command was run against
the live kind cluster and the output below is real. Where a decision has a
real trade-off or a known limitation, it's stated plainly — that reads as more
credible than a claim with no rough edges.

Format for each entry: **the question**, **the answer** (say it out loud),
**the evidence** (go check it yourself).

---

## 1. What actually happens when I `kubectl apply` a Plant?

**The question:** Walk me through the full path — from `kubectl apply` to a
running pod.

**The answer:** The write hits admission first, in a fixed order: CRD schema
defaulting fills unset fields, then the mutating webhook runs (which is
almost always a no-op, because schema defaulting already ran), then CRD
structural/CEL validation, then the validating webhook. Only once all four
pass does the object land in etcd and the controller's watch fires. The
reconciler then does five things in order: fetch the Plant, reject it up
front if the name is too long to be a label value, apply all six (or seven,
with Prometheus installed) owned children field-by-field, write status
through the status subresource only if something besides the timestamp
changed, and requeue on a timer. Nothing about children or status happens
before admission has already accepted the object.

**The evidence:** The exact ordering is spelled out in
`api/v1alpha1/plant_webhook.go:42-48`: `default(decode) -> mutate(webhook) ->
validate(schema) -> validate(webhook)`. The five reconcile steps are the
numbered comments in `internal/controller/plant_controller.go:285-358`
(`Reconcile`). Live proof the whole chain lands correctly:

```
$ kubectl get plants -n k8s-buddy-plants
NAME     SPECIES     MOOD    HEALTH   READY   DESIRED   AGE
fernie   fern        leafy   100      3       3         70m
stormy   succulent   leafy   100      3       3         70m
```

---

## 2. Why is there no finalizer on Plant?

**The question:** Operators normally have finalizers. Why doesn't this one?

**The answer:** It used to, and it was removed — see ADR 0007. The finalizer
cleaned up nothing; every child already carries a controller owner reference,
so Kubernetes' own garbage collector removes them when the Plant goes away.
The finalizer's only actual effect was that if the operator was down, crashed,
or not yet deployed, `kubectl delete plant` would hang forever with a
`deletionTimestamp` and no way to remove it short of hand-editing
`metadata.finalizers`. That's a real availability cost bought for zero cleanup
benefit, so it's gone. The one condition that would bring it back: the moment
this operator owns state Kubernetes itself can't garbage-collect — an external
DNS record, a bucket, a database row — a finalizer becomes necessary again,
and that would be a new ADR, not a quiet revert.

**The evidence:** `docs/adr/0007-no-finalizer-on-plant.md`. In code:
`internal/controller/plant_controller.go:301-307` — `Reconcile` returns
immediately when `DeletionTimestamp != nil`, no finalizer add/remove branch
anywhere in the file. The regression test asserts the list is empty, not just
one string:
`internal/controller/plant_controller_test.go:319-332`
(`TestReconcile_AddsNoFinalizer`) and `suite_test.go:494-502`
(`requireNoFinalizers`).

---

## 3. Your operator holds a ClusterRole — justify it.

**The question:** Why does plant-operator need cluster-scoped RBAC instead of
a namespaced Role?

**The answer:** A Plant can be created in any namespace, and controller-runtime's
informer cache does a real LIST+WATCH against the API server for every type
the operator `Owns()` — a namespaced Role literally cannot grant a cluster-wide
list/watch; the API server would reject it. The actual privilege is narrowed a
different way: the cache is filtered per-type down to objects carrying
`app.kubernetes.io/managed-by=plant-operator`, so even though the RBAC grant is
cluster-wide, the operator only ever holds its own children in memory, not
every ConfigMap and ServiceAccount that exists on the cluster.

**The evidence:** `internal/controller/cache.go:33-81` (`CacheOptions`) is the
one place this filter is defined; both the deployed binary
(`cmd/plant-operator/main.go:210`) and the envtest suite
(`internal/controller/suite_test.go:185`) call the same function so they can't
drift. Live proof it's cluster-scoped:

```
$ kubectl get clusterrolebinding | grep plant-operator
plant-operator-rolebinding   ClusterRole/plant-operator-role   4h16m
```

`config/rbac/role.yaml` is a `ClusterRole`, not a `Role` — the whole file has
no `namespace:` field.

---

## 4. How do you know the reconciler is idempotent?

**The question:** Prove a steady-state Plant produces zero writes.

**The answer:** `resourceVersion` alone can't prove this — the API server
re-applies its own defaulting before etcd ever stores anything, so a mutate
function that zeroes a server-defaulted field and forces an `Update` every
pass still shows an unchanged `resourceVersion`, because the bytes that
eventually land are identical to what was already there. That exact bug
shipped once: the three probes didn't set `TimeoutSeconds`, `SuccessThreshold`,
and `HTTPGet.Scheme`, so every reconcile diffed the builder's zero-valued
fields against the server's defaulted ones and wrote every single pass. The
fix was a `countingClient` that wraps the real client and tallies actual
`Create`/`Update`/`Patch` calls, bucketed by GVK, separately from status
writes — and the idempotence test asserts that count is exactly zero on a
settled Plant, not just that `resourceVersion` didn't move.

**The evidence:** `internal/controller/counting_client_test.go:1-28` explains
why `resourceVersion` isn't sufficient. The test:
`internal/controller/plant_controller_test.go:453-493`
(`TestReconcile_Idempotence_SteadyStateReconcileWritesNothing`) resets the
counters after quiescence, triggers one more reconcile, and asserts
`writeTotal == 0` and `statusTotal == 0`. The probe bug itself, fixed:
`internal/controller/resources.go:313-327` (comment explaining why
`TimeoutSeconds: 1`, `SuccessThreshold: 1`, `Scheme: URISchemeHTTP` are set
explicitly even though they equal the server's own defaults) — commit
`a2705d9` (`fix(controller): stop probe defaults from forcing a write every
reconcile`).

---

## 5. What happens if the webhook is down — can you lock yourself out?

**The question:** Your validating webhook has `failurePolicy: Fail`. What
stops that from bricking the cluster's ability to manage Plants?

**The answer:** It genuinely blocks writes while the operator's unreachable —
that's the honest cost of `Fail`, and it's the correct trade because the
validating webhook enforces the two rules nothing else does (species
immutability, the image registry allowlist), and a webhook that fails open on
those enforces nothing when it matters. Three structural things keep that
outage from becoming a lockout: the webhook's own admission rules never list
the `DELETE` operation, so you can always delete a Plant no matter how broken
the operator is; the mutating (defaulting) webhook is `Ignore`, because CRD
schema defaults are a fully adequate fallback on their own; and status writes
go through the `plants/status` subresource, which the webhook's rules don't
list at all — only `plants` `CREATE`/`UPDATE` are gated. Recovery is just
making the operator healthy again; there's no manual `caBundle` fix, no
finalizer to strip, no stuck object.

**The evidence:**

```
$ kubectl get validatingwebhookconfiguration plant-operator-validating-webhook-configuration \
    -o jsonpath='{.webhooks[0].rules[0].operations}{"\n"}{.webhooks[0].failurePolicy}'
["CREATE","UPDATE"]
Fail
$ kubectl get mutatingwebhookconfiguration plant-operator-mutating-webhook-configuration \
    -o jsonpath='{.webhooks[0].failurePolicy}'
Ignore
```

Markers: `api/v1alpha1/plant_webhook.go:17-18`. The `DELETE`-exempt reasoning
and the last-resort escape hatch (`kubectl delete
validatingwebhookconfiguration ...`) are in
`docs/adr/0009-webhook-certificate-strategy.md` (the "Failure policy" section).
`ValidateDelete` unconditionally permits: `api/v1alpha1/plant_webhook.go:256-266`.

---

## 6. Why does the operator rebuild the base manifests in Go instead of applying the YAML?

**The question:** You have both `deploy/kustomize/base/*.yaml` and a Go
builder in `resources.go` producing the same workload. Isn't that
duplication?

**The answer:** Yes, deliberately, and it's kept honest by a test rather than
by discipline. Deleting the static manifests would remove Plan 1's zero-install
demo path — the thing that works with nothing but `kubectl`. Deleting the Go
builders and having the operator apply the static YAML would turn the
operator into a templating engine, which defeats the entire point: the
field-by-field diff-and-correct loop is what's being demonstrated. So both
stay, and `manifest_drift_test.go` parses the raw static YAML and asserts,
field by field, that security contexts, probe timings, resource limits, and
`terminationGracePeriodSeconds` agree — and just as importantly, it asserts
the handful of places they're *supposed* to differ (a 2-key vs 3-key
selector, `managed-by` value, the PDB naming convention), so an unintended
change to a deliberate difference fails the same test an unintended drift
would.

**The evidence:** `docs/adr/0006-operator-reproduces-base-manifests.md`
(the table of intentional differences). The test itself:
`internal/controller/manifest_drift_test.go:1-46` (header explaining why it
reads raw YAML rather than shelling out to `kubectl kustomize`). Both files
carry a header comment naming the other as its twin
(`internal/controller/resources.go:1-23`).

---

## 7. Why no HorizontalPodAutoscaler?

**The question:** You have a `scale` subresource but no HPA. Why?

**The answer:** kind ships no `metrics-server`, so `metrics.k8s.io` doesn't
exist on this cluster at all — an HPA created here would sit permanently at
`ScalingActive=False` with `FailedGetResourceMetric`. That's worse than
absent: it's a resource that looks correct in `kubectl get hpa` and has never
made a single scaling decision, which invites a reviewer to believe
autoscaling works when it's never been exercised. Installing
kube-prometheus-stack doesn't fix this either — Prometheus and
`metrics-server` are two unrelated pipelines that happen to share the word
"metrics"; bridging them needs a Prometheus Adapter, which is a second Helm
release and a second CRD, entirely out of proportion for what this demo needs
to prove. The scale subresource stays — `kubectl scale plant fernie
--replicas=5` works today — but `selectorpath` is deliberately left empty,
because that field only matters to an HPA doing CPU/memory-based scaling,
and pointing it somewhere would advertise support for something that can't
work.

**The evidence:** `docs/adr/0008-deferred-to-plan-3.md`, section 1 (marked
"PERMANENT, not merely deferred"). The scale subresource marker:
`api/v1alpha1/plant_types.go:171-188`. Live proof:

```
$ kubectl get hpa -A
No resources found
```

**A note on this ADR's own accuracy, worth naming out loud:** ADR 0008,
section 3, says `PlantSpec.chaos` "remains genuinely deferred/dropped." That
statement is now stale — a later commit (`433f5b7`, after this ADR was
recorded) restored a narrow `ChaosSpec{EnableEndpoints bool}` field so
chaos-buddy's readiness-flap mode could actually be demonstrated end-to-end.
`kubectl explain plant.spec` today shows `chaos` as a real field. The ADR was
never updated to reflect that reversal — a candidate asked "is this ADR
current?" should say no on this one point, and that it should have been
superseded rather than left silently stale. See CORRECTIONS in the PR
description for the full trace.

---

## 8. Why must liveness never consult readiness or business health?

**The question:** Why doesn't `/healthz` just return whatever `/readyz` or
`/status` says?

**The answer:** Liveness and readiness answer different questions — "is this
process wedged and does it need to be killed" versus "should traffic go here
right now" — and conflating them is one of the most common Kubernetes
misconfigurations. A plant that's merely thirsty, or one chaos-buddy has
deliberately flapped not-ready for a demo, is neither wedged nor broken; it's
healthy code accurately reporting bad news. If `/healthz` mirrored `/readyz`,
the kubelet would restart the pod every time readiness dipped, which destroys
the in-flight requests graceful shutdown exists to protect, resets the
in-memory `/work` window the mood is computed from, and turns a
CrashLoopBackOff into what actually happened — a recoverable degradation. So
`/healthz` returns 200 whenever the process can run a handler at all, full
stop, and never touches `s.ready` or the mood engine.

**The evidence:** `docs/adr/0004-liveness-never-consults-readiness-or-business-health.md`.
Code: `internal/api/handlers.go:25-47` (`healthzHandler`) versus
`internal/api/handlers.go:49-62` (`readyzHandler`) — the first never reads
`s.ready`, the second only reads `s.ready`. The regression guard is named
directly in the ADR: `TestHealthz_AlwaysOK_EvenWhenNotReady`.

---

## 9. How do webhook certificates work without cert-manager?

**The question:** No cert-manager in this cluster — how does the webhook get
a TLS certificate the API server trusts?

**The answer:** The operator mints its own self-signed CA and a leaf serving
certificate at every process startup, before the manager (and therefore the
webhook server) ever starts, and patches the CA's PEM bytes into both webhook
configurations' `caBundle`. Nothing survives a restart — a crash, a rollout, a
reschedule all mint a brand-new CA. Two mistakes in the first version are
worth naming because they're the actual interesting content: a 24h validity
window was tried first and was wrong, because nothing proactively regenerates
the cert before expiry — a pod that just stays up eventually serves an
expired leaf and every write starts failing silently. It's 10 years now, with
a health check that fails `/healthz` and `/readyz` 24h before actual expiry so
a restart happens long before that could ever matter. The second mistake:
overwriting the `caBundle` instead of merging it breaks a rolling update,
because the new pod's CA gets patched in before it's Ready, while the old pod
is still serving with its old cert — an overwrite makes the API server stop
trusting the still-serving old pod immediately. The fix appends and caps at 3
entries (covers one rollout's old+new overlap plus one generation of margin).

**The evidence:** `docs/adr/0009-webhook-certificate-strategy.md`. Code:
`cmd/plant-operator/webhookcerts.go:43-64` (`webhookCertificateValidity`,
10 years, with the 24h mistake documented in the comment),
`webhookcerts.go:186-301` (`maxRetainedCAs`, `mergeCABundle`),
`webhookcerts.go:302-321` (`webhookCertExpiryCheck`). Ordering in
`cmd/plant-operator/main.go:176-207` — cert generation and `caBundle` patch
both run before `ctrl.NewManager`. Live proof the bundle is capped and
populated:

```
$ kubectl get validatingwebhookconfiguration plant-operator-validating-webhook-configuration \
    -o jsonpath='{.webhooks[0].clientConfig.caBundle}' | base64 -d | grep -c "BEGIN CERTIFICATE"
3
```

That's the cap (`maxRetainedCAs = 3`) already reached on this cluster from
prior restarts — not zero, not unbounded.

---

## 10. Why is the observability namespace `privileged` when everything else is `restricted`?

**The question:** Every other namespace enforces Pod Security `restricted`.
Why does `k8s-buddy-observability` allow `privileged`?

**The answer:** One DaemonSet forces it: promtail reads node-local log files
(`/var/log/pods` and the container runtime's own log directory), and there's
no way to read a node's own filesystem without a `hostPath` mount — that's
inherent to being a node-level log shipper, not something tunable away. The
first attempt used `baseline` on the assumption that only `restricted` blocks
`hostPath`; that's wrong per the actual Pod Security Standards spec, and it
failed on contact with the real cluster (`helm upgrade` produced a hostPath
admission warning and the DaemonSet sat at 0/3 Ready). `privileged` is the
only PSA level that admits promtail's upstream defaults at all. The blast
radius is still shrunk on the one axis that's controllable:
`prometheus-node-exporter` — the other component that would've needed
`privileged` — is disabled outright, because nothing in this project's
dashboards or alert rules reads a single `node_*` metric. Every other
component in that namespace (Prometheus, Alertmanager, Grafana, Loki) runs
exactly as hardened as `restricted` would force anyway, on its own chart
defaults.

**The evidence:** `deploy/observability/namespace.yaml:1-76` (the full
rationale is a comment on the manifest itself). Live proof:

```
$ kubectl get ns k8s-buddy-observability -o jsonpath='{.metadata.labels}' | tr ',' '\n' | grep pod-security
"pod-security.kubernetes.io/audit":"baseline"
"pod-security.kubernetes.io/enforce":"privileged"
$ kubectl get ns k8s-buddy-plants -o jsonpath='{.metadata.labels}' | tr ',' '\n' | grep pod-security
"pod-security.kubernetes.io/audit":"restricted"
"pod-security.kubernetes.io/enforce":"restricted"
$ kubectl get all -n k8s-buddy-observability | grep -i node-exporter
(no output — disabled)
```

`deploy/observability/values-kube-prometheus-stack.yaml:52-53` —
`nodeExporter: enabled: false`.

---

## 11. Why is there no `preStop` hook?

**The question:** Zero-downtime shutdown usually means a `preStop` sleep.
Why doesn't buddy-api have one?

**The answer:** The distroless image has no shell (see ADR 0005), so an
`exec` `preStop` would fail on every single termination — a loud,
permanent `FailedPreStopHook` event rather than a quiet no-op. The `httpGet`
variant would at least run, but it buys nothing: it can't extend
`terminationGracePeriodSeconds`, so it's decoration. The delay runs in-process
instead — `gracefulShutdown` in `cmd/buddy-api/main.go` marks the pod
not-ready first, sleeps `BUDDY_SHUTDOWN_DELAY` (5s default) to let that
propagate through Endpoints/EndpointSlice and every node's kube-proxy, then
calls `http.Server.Shutdown` with a 15s drain. `terminationGracePeriodSeconds:
30` has to stay comfortably above delay-plus-drain, and that coupling lives
in two different files with no compiler check tying them together — it's a
convention, not an invariant.

**The evidence:** `docs/adr/0002-in-process-shutdown-delay-instead-of-prestop-hook.md`.
Code: `cmd/buddy-api/main.go:201-271` (`gracefulShutdown`, three numbered
phases logged as they happen). No `Lifecycle` field anywhere in
`internal/controller/resources.go`'s `DeploymentFor`
(`internal/controller/resources.go:259-271` explicitly documents the
omission). The grace period: `internal/controller/resources.go:106-110`
(`terminationGracePeriodSeconds = 30`).

---

## 12. Why is the NetworkPolicy ingress open on 8080 rather than podSelector-scoped?

**The question:** Your NetworkPolicy's ingress rule on port 8080 has no
`from:` selector — any pod can reach it. Why not scope it?

**The answer:** The demo's traffic path is a NodePort, and kube-proxy SNATs
that traffic to the node's own IP before it reaches the pod — so the source
address the NetworkPolicy actually evaluates is a node IP, never a pod IP or
the real client's. A `from: podSelector` rule would never match, and the
failure mode is the worst kind: TCP connects and then just hangs, no RST,
which reads as an application bug rather than a policy denial. Two tighter
alternatives were rejected for concrete reasons: `externalTrafficPolicy:
Local` preserves source IP, but the NodePort's port mapping lands on the
control-plane node, which is tainted `NoSchedule` and never runs a buddy-api
pod — `Local` would just drop the traffic outright. An `ipBlock` on the
docker network's CIDR would work on this machine but hardcodes a subnet kind
assigns differently on a clean install, reintroducing "works on my machine."
So the hole stays unscoped, and the blast radius is constrained on every
other axis: only buddy-api's own pods are selected, only port 8080, and
egress stays fully denied except DNS.

**The evidence:** `docs/adr/0003-networkpolicy-ingress-open-on-tcp-8080.md`.
Code: `internal/controller/resources.go:511-594` (`NetworkPolicyFor`, "No
From: — see this function's doc comment and ADR 0003"). Live proof of the
rule as shipped:

```
$ kubectl get networkpolicy fernie -n k8s-buddy-plants -o yaml | sed -n '/ingress:/,/egress:/p'
  ingress:
  - ports:
    - port: 8080
      protocol: TCP
  podSelector:
    matchLabels:
      app.kubernetes.io/instance: fernie
      buddy.k8s-buddy.io/plant: fernie
```

Note the ingress rule itself carries no `from:` — the `podSelector` shown is
what the *policy* is scoped to (which pods it governs), not who's allowed in.

---

## 13. How does the SLO alerting work, and why multi-window multi-burn-rate?

**The question:** Walk me through the SLO alerting design.

**The answer:** The SLO is 99% availability of `/work` over 30 days, where
"good" is any non-5xx response. A single static error-rate threshold is
either too sensitive — paging on a blip that would never actually exhaust a
30-day budget — or too slow — missing a real outage until the budget's
already gone. So it's two alerts, each requiring both a long and a short
window to breach simultaneously: a page fires at a 14.4x burn rate held over
both 1h and 5m (exhausts the budget in ~2 days), a ticket fires at 6x held
over both 6h and 30m (~5 days). The long window filters out a single sharp
spike; the short window is what lets the alert clear quickly once the
problem's actually fixed instead of the long window's slow decay keeping it
firing for hours after recovery. Both alerts and the runbook that documents
them have to agree on 14.4x/6x, or the drift is itself the bug.

**The evidence:** `observability/rules/slo.yaml:79-96`
(`BuddyAPIWorkErrorBudgetBurnPage`, `14.4 * 0.01` on both `rate1h` and
`rate5m`) and `slo.yaml:99-116` (`BuddyAPIWorkErrorBudgetBurnTicket`, `6 *
0.01` on `rate6h` and `rate30m`). Cross-checked against
`docs/runbook/README.md:29-44`, which states the same two ratios and the same
"if these drift, that's the bug" framing. The four recording rules per window
exist so the alert `expr` stays short and reusable:
`observability/rules/slo.yaml:34-70`.

---

## 14. Why does `wilting` need a special case in the operator's mood derivation?

**The question:** `internal/mood` is a shared package. Why does the
operator's use of it need a special case for zero-ready Plants?

**The answer:** The mood engine's own rule — a non-positive `LatencyBudget`
means "no data, award full latency marks" — is correct for its real owner,
buddy-api, which has an actual latency signal most of the time. But the
operator has *no* latency signal at all, ever; it only knows ready and
desired replica counts. Feeding the shared scoring function zero-valued
latency inputs meant that rule silently handed every Plant 30 of the ladder's
100 points regardless of health. At zero readiness that's decisive: error
component 0, latency component 30 (the "no data" freebie), readiness
component 0, total 30 — which lands in `not-too-hot`, not `wilting`, and 30
sits comfortably under the not-ready safety ceiling of 35 so that cap never
pulls it back down either. `wilting`, the ladder's most severe state, was
structurally unreachable from the operator. The fix special-cases `ready ==
0 && desired > 0` to `MoodWilting` directly, before `Score` ever runs — fixing
the wrong answer at its actual source (the operator fabricating latency data
it doesn't have) rather than touching `internal/mood`'s own rule, which stays
correct for buddy-api.

**The evidence:** `internal/controller/status.go:141-194` (`moodFor`, the
full reasoning is in the doc comment). The rule it works around:
`internal/mood/mood.go:117-144` (`Score`, the `if s.LatencyBudget > 0`
branch). Live proof, using a throwaway Plant with an unpullable image to
force `ready=0`:

```
$ kubectl apply -f - <<'EOF'
apiVersion: buddy.k8s-buddy.io/v1alpha1
kind: Plant
metadata:
  name: verify-wilting
  namespace: k8s-buddy-plants
spec:
  species: cactus
  replicas: 1
  image: docker.io/library/does-not-exist-k8s-buddy-verify:latest
EOF
... (20s later)
$ kubectl get plant verify-wilting -n k8s-buddy-plants
NAME             SPECIES   MOOD      HEALTH   READY   DESIRED   AGE
verify-wilting   cactus    wilting   0        0       1         20s
$ kubectl delete plant verify-wilting -n k8s-buddy-plants
```

Commit `ff2b83e` (`fix(controller): make wilting reachable — zero ready
replicas is wilting, not not-too-hot`) is the fix itself; before it, this
same Plant would have reported `not-too-hot`.

---

## Two more, briefly, for completeness

### Why record any of this as ADRs at all?

**The answer:** Every one of these decisions has a plausible alternative a
competent reviewer would expect instead — plain Deployment instead of an
operator, `podSelector`-scoped NetworkPolicy, cert-manager, a `preStop` hook.
Without a written record the answer only exists in the author's head, and
that's gone the moment the branch merges or the interview starts. ADR 0001
sets the bar for what warrants one: would it change the answer to "why does
this repo look this way," is there a plausible alternative, does it constrain
future work in a way the code alone doesn't show.

**The evidence:** `docs/adr/0001-record-architecture-decisions.md`.

### Why `gcr.io/distroless/static-debian12:nonroot` instead of alpine or scratch?

**The answer:** Alpine ships busybox, apk, and a shell — attack surface the
app never uses and a source of CVEs on packages the binary doesn't even link
against. `scratch` is smaller still but has no CA bundle and no passwd entry,
so anything resolving the run-as user by name breaks and future outbound TLS
would fail silently. Distroless nonroot gets CA certs, tzdata, and a passwd
entry for uid 65532, and nothing else — no shell, no package manager. The
direct cost is the one that drove ADR 0002: no `exec`-based `preStop` is
possible, and `kubectl exec -- sh` fails outright, so debugging a running pod
needs `kubectl debug` with an ephemeral container instead.

**The evidence:** `docs/adr/0005-distroless-nonroot-base-image.md`.
`build/Dockerfile.buddy-api`'s final stage `FROM
gcr.io/distroless/static-debian12:nonroot` with `USER 65532:65532` set
explicitly rather than relying on the tag default.

---

## Bugs this project's tests did not catch, and what changed

None of these are apologies — the pattern in every one is the same, and it's
a useful pattern to name out loud: **the component itself was correct in
isolation; the wiring between it and the running system was untested.** A
builder that's asserted only against its own output, a package that's
covered but never fed real data, a script that measures the wrong window —
all six passed their own tests right up until someone looked at the live
system.

1. **The mood engine had 97.6% unit test coverage and zero effect on the
   running system.** `internal/mood` shipped a tuned six-mood ladder, fully
   tested — but `internal/api/server.go` never fed it anything real: error
   rate came from lifetime counters only `/work` moved, and P95 latency was
   never set at all. `/status` answered `leafy`/100 unconditionally; five of
   the six moods were unreachable in the shipped binary. Fixed by adding a
   bounded rolling window of the last 100 `/work` observations
   (`internal/api/server.go:189-226`, `currentReport`) that the mood is
   actually derived from. Commit `9024271`.

2. **A ServiceAccount was created, owned, and garbage-collected correctly —
   and never actually used**, while a code comment asserted the opposite.
   `DeploymentFor` set `ServiceAccountName` on the builder's output, and
   `resources_test.go` asserted it there — but `mutateDeployment` never
   copied that field onto the *live* Deployment object, so every real pod
   ran as the namespace's `default` ServiceAccount while the Plant's own
   account sat unused. The bug survived two whole tasks because only the
   builder was ever asserted against. Fixed by having `mutateDeployment`
   copy the field explicitly (`internal/controller/plant_controller.go:797-800`)
   and adding an envtest case that reads the *live* child back from the API
   server, not the builder's output
   (`TestReconcile_LiveDeploymentRunsAsPlantServiceAccount`,
   `internal/controller/plant_controller_test.go:208-227`). Commit `941b49e`.

3. **An alert cleared itself during the exact outage it was built to
   detect.** `PlantNotReconciling`'s expression compared
   `sum(increase(controller_runtime_reconcile_total{...}[5m]))` against `0`
   — but when the operator is genuinely down, the metric series itself
   vanishes rather than reading 0, and `sum()` over an empty vector is an
   *empty vector*, not zero. `== 0` against nothing returns nothing. The
   alert fired for the few minutes the pre-crash series took to age out of
   the window, then resolved itself while the operator was still dead — an
   alert clearing during the outage it exists to catch actively misleads
   whoever's paged. Fixed with `(sum(...) or vector(0)) == 0`
   (`observability/rules/operational.yaml:63-83`), verified live by scaling
   the operator to zero and watching the alert hold firing rather than
   self-resolve. Commit `d8ef634`.

4. **A demo script could report `SELF-HEALED` without ever observing
   chaos.** `hack/demo.sh`'s recovery poll counted every pod matching the
   buddy-api label — including the pod it had just deleted, which stays
   `Terminating` with a stale `ready:true` until its next readiness probe
   catches up. A poll landing in that window could see 3/3 immediately and
   print "SELF-HEALED after 0s" without a replacement pod ever existing.
   Fixed by excluding the deleted pod by name from every count, so recovery
   can only be declared once a genuinely new, distinct pod is Ready
   alongside the survivors (`hack/demo.sh:216-227`). Commit `f0607ae`.

5. **Probes that zeroed server-defaulted fields caused a write on every
   single reconcile.** `DeploymentFor`'s three probes never set
   `TimeoutSeconds`, `SuccessThreshold`, or `HTTPGet.Scheme`. The API
   server defaults all three when they're absent, so every subsequent
   reconcile diffed the builder's zero-valued fields against the server's
   defaulted ones, saw a difference, and issued an `Update` — forever, on a
   Plant that had nothing left to converge on. This is exactly the shape of
   bug `resourceVersion`-based idempotence checks structurally cannot see
   (see Q4 above). Fixed by setting all three explicitly to match what the
   server would default anyway (`internal/controller/resources.go:313-374`).
   Commit `a2705d9`.

6. **A data race in the graceful-shutdown test only Linux + `-race` ever
   caught.** `TestGracefulShutdown_OrderingAndDelay` wrote a timestamp from
   inside `http.Server`'s `RegisterOnShutdown` callback — which `net/http`
   runs on its own goroutine, with no happens-before edge back to the test
   goroutine that read the value afterward. That's a genuine data race in
   the test itself; it was invisible on Windows by scheduling luck and only
   surfaced under `-race` on CI's Linux runner. Fixed by replacing the
   mutex-guarded variable with a buffered channel — a channel receive is a
   defined synchronization point in the Go memory model, so reading the
   value afterward is actually race-free. Commit `1e747a9`.

---

*Every command in this document was run against the live kind cluster on
2026-08-01. Every `file:line` reference was checked against the file at the
time of writing. If either has drifted since, that drift is itself worth
flagging — the same standard this document applies to the ADRs it's built
from.*
