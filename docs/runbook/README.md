# Runbook

K8s Buddy is a demo, but this runbook treats it like a production service:
for every alert that can fire, what it means, how to confirm it, how to fix
it, and how to roll back. The `runbook_url` annotation on every alert in
`observability/rules/*.yaml` points at a specific heading below — if you
followed a link from Alertmanager or Grafana, you're in the right place, and
the anchor should have taken you straight to the relevant section.

There are 17 rules across the two committed rule files (12 recording rules
and 5 alerting rules). The recording rules exist only to keep the alert
expressions short and reusable; they never fire on their own. This runbook
covers the five that do:

| Alert | File | Severity | Section |
|---|---|---|---|
| `BuddyAPIWorkErrorBudgetBurnPage` | `observability/rules/slo.yaml` | page | [below](#buddyapiworkerrorbudgetburnpage) |
| `BuddyAPIWorkErrorBudgetBurnTicket` | `observability/rules/slo.yaml` | ticket | [below](#buddyapiworkerrorbudgetburnticket) |
| `PlantDegraded` | `observability/rules/operational.yaml` | warning | [below](#plantdegraded) |
| `PlantNotReconciling` | `observability/rules/operational.yaml` | critical | [below](#plantnotreconciling) |
| `ChaosRunawayDeletions` | `observability/rules/operational.yaml` | critical | [below](#chaosrunawaydeletions) |

Everything else in this document — operator down, webhook unreachable, a
`Plant` stuck `Degraded` with no alert yet firing, and the certificate story
— has no dedicated alert but is exactly the kind of thing an on-call reader
diagnosing one of the alerts above will run into, so it's covered at the
end.

## The SLO

buddy-api targets **99% availability of `/work` over 30 days**, where a
"good" request is any non-5xx response. This is implemented as multi-window,
multi-burn-rate alerting (the shape recommended by the Google SRE workbook),
not a single static error-rate threshold — a static threshold is either too
sensitive (paging on a blip that would never actually exhaust the 30-day
budget) or too slow (missing a real outage until the budget is already
gone). Requiring **both** a long window and a short window to exceed the
threshold simultaneously is what makes it "multi-window": the long window
filters out a single sharp spike, and the short window makes the alert
clear quickly once the real problem is fixed instead of the long window's
own slow decay keeping it firing for hours after recovery. These numbers
(14.4x / 1h+5m for page, 6x / 6h+30m for ticket) must always agree between
`observability/rules/slo.yaml` and this document — if you're reading this
because they've drifted apart, that drift is itself the bug.

### BuddyAPIWorkErrorBudgetBurnPage

**What it means.** buddy-api's `/work` endpoint is returning 5xx responses
at 14.4x the rate the 99% availability SLO can sustain, over *both* the
last hour and the last 5 minutes. At this rate the entire 30-day error
budget is exhausted in about 2 days. This is the page-worthy alert — real,
sustained, severe.

**How to confirm.**

```bash
kubectl -n k8s-buddy-plants get plants
kubectl -n k8s-buddy-plants get pods -l app.kubernetes.io/name=buddy-api
```

Then in Prometheus (`kubectl -n k8s-buddy-observability port-forward svc/kube-prometheus-stack-prometheus 9090:9090`),
graph `buddy_api:work_error_ratio:rate5m` and `buddy_api:work_error_ratio:rate1h`
— both should be visibly above `0.144` (14.4 × 1%). Cross-check against the
Grafana dashboard's "/work request rate by outcome" panel: a real burn shows
as a rising `failure` series, not `warning` (which buddy-api emits for a
*simulated* failure at its default 5% rate — that alone should never trip
this alert; see the note in `observability/rules/operational.yaml` on why
`ChaosRunawayDeletions` similarly excludes dry-run outcomes).

**How to fix.** This is almost always one of:

- **A `Plant` is genuinely down or degraded.** Check
  [`PlantDegraded`](#plantdegraded) and [`PlantNotReconciling`](#plantnotreconciling)
  first — if either is also firing, fix that first; this alert is likely a
  downstream symptom.
- **Chaos is misconfigured or running with `--dry-run=false` unexpectedly.**
  Check `buddy_chaos_enabled` and `buddy_chaos_actions_total` — see
  [`ChaosRunawayDeletions`](#chaosrunawaydeletions) for the kill switch.
- **A bad rollout.** `kubectl -n k8s-buddy-plants rollout status deployment/<plant>`
  and `kubectl -n k8s-buddy-plants rollout history deployment/<plant>`. Roll
  back with `kubectl -n k8s-buddy-plants rollout undo deployment/<plant>` if
  a recent image or config change is the cause — but remember the operator
  reconciles the `Deployment` from `Plant.spec` every `wateringInterval`
  (default 30s), so a raw `rollout undo` against the Deployment is only a
  temporary reprieve unless the `Plant`'s own `spec.image` is also reverted
  (`kubectl -n k8s-buddy-plants edit plant <name>`).
- **buddy-api's own simulated error rate was raised.** `BUDDY_WORK_ERROR_RATE`
  in the generated ConfigMap defaults to `0.05` (5%), well under the 1%
  budget's 14.4x threshold (14.4%) — if it was hand-edited higher for a demo
  and never reverted, that alone can trip this alert legitimately. Check
  `kubectl -n k8s-buddy-plants get configmap <plant> -o jsonpath='{.data.BUDDY_WORK_ERROR_RATE}'`.

**How to roll back.** Whatever the fix was, verify recovery the same way you
confirmed the alert: `buddy_api:work_error_ratio:rate5m` back under `0.144`,
sustained for at least the 2-minute `for:` window, will clear the alert on
its own — no manual "resolve" step exists or is needed in Alertmanager for
a Prometheus-managed alert.

### BuddyAPIWorkErrorBudgetBurnTicket

**What it means.** The same signal as the page alert above, but at a slower,
less severe burn: 6x the sustainable rate over both a 6-hour and a
30-minute window, exhausting the 30-day budget in about 5 days rather than
2. This is a ticket, not a page — real, but with room to investigate during
business hours rather than immediately.

**How to confirm and fix.** Identical procedure to
[`BuddyAPIWorkErrorBudgetBurnPage`](#buddyapiworkerrorbudgetburnpage) above,
substituting `buddy_api:work_error_ratio:rate30m` and
`buddy_api:work_error_ratio:rate6h` (threshold `0.06`) for the 5m/1h pair.
If both this alert and the page alert are firing simultaneously, treat it as
the page: the ticket-level windows are strictly longer and will keep firing
after the page-level burn has already cleared, which is expected and not a
second, independent problem to chase.

**How to roll back.** Same as above: `buddy_api:work_error_ratio:rate30m`
back under `0.06`, sustained for the 15-minute `for:` window, clears it
automatically.

## Operational alerts

### PlantDegraded

**What it means.** A `Plant`'s `Degraded` condition has been `True` for at
least 5 minutes — per `internal/controller/status.go`'s conditions table,
that means `readyReplicas == 0` while `desiredReplicas > 0`: the `Plant` has
*no* ready replicas at all, not merely fewer than desired.

**How to confirm.**

```bash
kubectl -n k8s-buddy-plants get plants
kubectl -n k8s-buddy-plants describe plant <name>
kubectl -n k8s-buddy-plants get pods -l buddy.k8s-buddy.io/plant=<name>
```

`describe plant` shows the condition's `Reason` and `Message` directly
(`ReasonInsufficientReplicas`, `"no replicas are ready"`), plus recent
`PlantDegraded`/`PlantRecovered` Events emitted on the transition edges (see
`emitHealthEvents` in `plant_controller.go` — these fire once per
transition, not once per reconcile, so their absence over a long window is
itself informative: a `Plant` degraded for hours with no fresh Event means
it degraded once and never recovered, not that it's flapping).

**How to fix.** Read the pod-level reason first —
`kubectl -n k8s-buddy-plants describe pods -l buddy.k8s-buddy.io/plant=<name>`
— the usual causes, in rough likelihood order:

- **`ImagePullBackOff`.** `spec.image` points at a tag the node has never
  loaded. If this is a `make demo-operator` cluster, images are loaded
  directly into kind's containerd and never pulled from a real registry
  (`imagePullPolicy: IfNotPresent`); re-run `make docker-build-operator
  kind-load-operator` (or the buddy-api equivalent) if the image was rebuilt
  without reloading it.
- **`CrashLoopBackOff`.** `kubectl -n k8s-buddy-plants logs <pod> --previous`.
  Given [ADR 0004](../adr/0004-liveness-never-consults-readiness-or-business-health.md),
  a genuine crash loop here means the process itself is wedged or panicking
  — not a business-logic health dip, since `/healthz` never consults
  readiness or the mood engine.
- **Chaos.** If `chaos-buddy` is running with `--dry-run=false` and its mode
  is `pod-kill`, a `Plant` whose replica count is low enough (or whose PDB
  is being violated by something else at the same time) can genuinely spend
  a few seconds at zero ready replicas during a kill-and-reschedule cycle.
  This should self-resolve well inside the alert's 5-minute `for:` window;
  if it doesn't, see [`ChaosRunawayDeletions`](#chaosrunawaydeletions).
- **PSA/webhook rejection on every replacement pod.** If the `Plant`'s
  namespace enforces `restricted` (every `k8s-buddy-plants`-style namespace
  in this project does) and something changed the pod template to violate
  it, the ReplicaSet emits `FailedCreate` Events forever and `readyReplicas`
  never moves. `kubectl -n k8s-buddy-plants get events --sort-by=.lastTimestamp`.

**How to roll back.** Fix the underlying pod-level problem (revert the
image, fix the crash, disable chaos); the operator's own reconcile loop
will pick up a corrected `Deployment` on its next pass (at most one
`wateringInterval` later, default 30s) and `Degraded` clears itself once
`readyReplicas > 0` again — no manual condition reset is possible or needed.

### PlantNotReconciling

**What it means.** `plant-operator` has recorded **zero** non-error
reconcile outcomes for the `plant` controller in the last 5 minutes. This is
`critical`, not `warning`, because it means the entire control loop for
every `Plant` on the cluster has stopped — not just one `Plant`'s workload.
The alert is carefully written to catch the case that matters most: if the
operator process is gone entirely, its `controller_runtime_reconcile_total`
series disappears with it, and `sum(increase(<empty>[5m]))` evaluates to an
*empty vector*, not `0` — PromQL's `== 0` against an empty vector matches
nothing at all. The rule's `or vector(0)` fallback exists specifically so an
operator crash still trips this alert instead of silently going quiet.

**How to confirm.**

```bash
kubectl -n k8s-buddy-system get pods -l app.kubernetes.io/name=plant-operator
kubectl -n k8s-buddy-system get deployment plant-operator
kubectl -n k8s-buddy-system logs deploy/plant-operator --tail=100
```

If the pod is missing or not `Running`, that's the whole diagnosis. If it
*is* running, check whether reconciles are happening but all erroring:
query `controller_runtime_reconcile_total{controller="plant",result="error"}`
in Prometheus, and read the operator's own logs for the error — a stuck
leader election (`kubectl -n k8s-buddy-system get lease
plant-operator.buddy.k8s-buddy.io -o yaml`) is a plausible cause if a
previous replica crashed uncleanly and holds a lease no live pod is
renewing.

**How to fix.**

- **Pod missing/crashing:** `kubectl -n k8s-buddy-system describe pods -l app.kubernetes.io/name=plant-operator`
  for the scheduling/crash reason; check `ImagePullBackOff` (same story as
  above), OOMKilled (`kubectl -n k8s-buddy-system get pods -o
  jsonpath='{.items[*].status.containerStatuses[*].lastState.terminated.reason}'`),
  or a webhook certificate problem — see
  [Certificate story](#certificate-story) below, since a bad
  `--webhook-cert-dir` or an unwritable `emptyDir` fails the process at
  startup before it ever reconciles anything.
- **Every reconcile erroring:** read the specific error in the logs.
  A `conflictingResourceError` (a pre-existing, non-operator-owned object
  blocking a child create — see
  [Architecture: Ownership and the uncached reader](../architecture/README.md#ownership-and-the-uncached-reader))
  surfaces as `Degraded/ConflictingResource` on the affected `Plant` and
  should not by itself stop *other* Plants from reconciling; if it does,
  that's a regression worth filing, not expected behavior.
- **Stuck leader election:** delete the stale `Lease` object
  (`kubectl -n k8s-buddy-system delete lease plant-operator.buddy.k8s-buddy.io`)
  only after confirming no operator pod is actually running — deleting a
  lease a live pod still holds just forces an unnecessary re-election.

**How to roll back.** Restore the operator to `Running`/`Ready`
(`kubectl -n k8s-buddy-system rollout status deployment/plant-operator`);
`controller_runtime_reconcile_total` resumes incrementing on the very next
successful reconcile and the alert clears once 5 minutes of non-zero
increase have accumulated.

### ChaosRunawayDeletions

**What it means.** `buddy_chaos_actions_total` (excluding dry-run outcomes)
is increasing at more than 6 actions/minute, sustained for 2 minutes — three
times the steady-state ceiling a default `CHAOS_INTERVAL=30s` implies (at
most one action per iteration, so at most 2/minute normally). This catches
a misconfigured interval or a bug in the poll loop *before* it can delete a
meaningful fraction of a `Plant`'s pods in a short window.

**How to confirm.**

```bash
kubectl -n k8s-buddy-plants get pods -l app.kubernetes.io/name=chaos-buddy
kubectl -n k8s-buddy-plants get configmap chaos-buddy-config -o yaml
kubectl -n k8s-buddy-plants logs deploy/chaos-buddy --tail=100
```

Check `CHAOS_INTERVAL` in the ConfigMap against what it should be (default
`30s`); check the logs for a tight loop (timestamps far closer together than
`CHAOS_INTERVAL` apart indicate the loop itself, not just the configured
interval, is broken).

**How to fix — immediately, before investigating root cause:**

```bash
kubectl -n k8s-buddy-plants patch configmap chaos-buddy-switch \
  --type=merge -p '{"data":{"enabled":"false"}}'
```

This is the kill switch: `chaos-buddy` re-reads it every loop iteration
(`internal/chaos`'s `PodClient.ReadSwitch`), so it takes effect within one
`CHAOS_INTERVAL` — no redeploy, no rollout, no waiting on a rollout status.
`buddy_chaos_enabled` drops to `0` once it's picked up, visible immediately
on the Grafana dashboard's "Chaos actions" panel. Only *after* the switch is
off should you investigate the ConfigMap or the pod's logs for the actual
misconfiguration.

**How to roll back.** Fix `CHAOS_INTERVAL` (or whatever the root cause was)
in `deploy/kustomize/chaos/configmap.yaml`, redeploy
(`make deploy-chaos`), confirm the rate has returned to normal via
`buddy_chaos_actions_total`, and only then flip the switch back on:

```bash
kubectl -n k8s-buddy-plants patch configmap chaos-buddy-switch \
  --type=merge -p '{"data":{"enabled":"true"}}'
```

Also double check `--dry-run`: the shipped manifest defaults
`CHAOS_DRY_RUN=true`, so an accidental redeploy is inert by construction. If
this alert fired against real (non-dry-run) deletions, that was a
deliberate `--dry-run=false` flip somewhere — confirm it was intended before
re-enabling.

## Other failure modes

These have no dedicated alert (either because they're covered indirectly by
one of the five above, or because they're rare enough that hand-diagnosis is
the right tool), but are exactly what an on-call reader chasing one of the
alerts above is likely to actually find.

### Operator down

Covered in detail under [`PlantNotReconciling`](#plantnotreconciling) above.
The short version: `kubectl -n k8s-buddy-system get pods -l
app.kubernetes.io/name=plant-operator`, then `describe`/`logs` for why.
Because there is no finalizer on `Plant` ([ADR 0007](../adr/0007-no-finalizer-on-plant.md)),
an operator outage never blocks `kubectl delete plant` — only reconciliation
(new/changed `Plant`s not being picked up) and, separately, admission
writes (see below) are affected.

### Webhook unreachable

The validating webhook has `failurePolicy: Fail` — an operator that's down
or whose webhook server isn't yet Ready causes every `Plant`
`CREATE`/`UPDATE` to be rejected with a clear
`Error from server (InternalError): ... failed calling webhook ...` message,
not a silent no-op. This is deliberate (see
[ADR 0009](../adr/0009-webhook-certificate-strategy.md)'s "Failure policy"
section) — a validating webhook that fails open enforces nothing when it
matters most.

**Normal recovery:** just make the operator healthy again
(`kubectl -n k8s-buddy-system rollout status deployment/plant-operator`, or
fix whatever crashed it). The moment its pod is `Ready`, the webhook
`Service` routes to it, TLS succeeds against the `caBundle` it patches in at
its own startup, and `Plant` writes resume — no manual `caBundle` fix-up, no
finalizer to strip, no stuck object.

**Cluster-admin last resort** (from ADR 0009): if you need `Plant` writes to
succeed *before* the operator can be fixed — for instance, to debug the
operator itself by editing a `Plant` it reconciles — remove the validating
gate entirely:

```bash
kubectl delete validatingwebhookconfiguration plant-operator-validating-webhook-configuration
```

This is a deliberate, visible, `cluster-admin`-only escape hatch, not
something the operator does automatically, and it requires the same RBAC
level that could just as easily delete the `Plant`'s namespace outright.
Recreate it once the operator is healthy again by re-applying
`config/webhook/manifests.yaml` (via `make deploy-operator`, which applies
the whole `deploy/kustomize/operator` kustomization) — the operator will
re-patch a fresh `caBundle` into it on its next restart, or immediately if
it's already running (`patchWebhookCABundles` retries with backoff against
exactly this "webhook configuration doesn't exist yet" race).

Note `DELETE` is exempt from webhook enforcement entirely — `kubectl delete
plant <name>` always works, operator up or down, webhook reachable or not
— so this escape hatch is only ever needed to unblock a `CREATE` or
`UPDATE`.

### A Plant stuck Degraded

If `PlantDegraded` hasn't fired yet (under 5 minutes) or you're looking at
a `Plant` outside the alert's window, the diagnosis is the same as
[`PlantDegraded`](#plantdegraded) above: `kubectl describe plant <name>`
for the condition's `Reason`/`Message`, then `describe pods` for the
pod-level cause. Two `Degraded` reasons are worth knowing about because
they are **not** transient and will never self-heal no matter how long you
wait:

- **`InvalidName`** — the `Plant`'s own name is too long to become a label
  value (over 63 characters). `metadata.name` is immutable; the only fix is
  deleting and recreating the `Plant` under a shorter name.
- **`ConflictingResource`** — a same-named, same-kind object already exists
  in the namespace and was not created by this operator (see
  [Architecture: Ownership and the uncached reader](../architecture/README.md#ownership-and-the-uncached-reader)).
  The message names the object and why it was refused. Fix by renaming the
  `Plant`, or by removing/relabelling the conflicting object if it's safe to
  do so.

### Certificate story

`plant-operator` generates a fresh self-signed CA and serving certificate
at **every process startup**, writes it to an `emptyDir`-backed
`--webhook-cert-dir`, and merges (never overwrites) its CA into both
webhook configurations' `caBundle` before the manager starts listening.
Full reasoning: [ADR 0009](../adr/0009-webhook-certificate-strategy.md).
What actually matters operationally:

- **Certificate validity is 10 years.** You should not encounter a genuine
  expiry on this project's timescale. If you somehow do (a badly skewed
  clock, for instance), both `/healthz` and `/readyz` fail once the leaf is
  within 24 hours of its real expiry (`webhookCertExpiryCheck`), which
  triggers a kubelet restart of the container — minting a fresh certificate
  automatically, no manual intervention required.
- **A crash-looping operator regenerates its certificate every restart**,
  which is harmless: `mergeCABundle` appends rather than overwrites, so a
  currently-serving replica's own CA is never evicted by a different
  replica (or a previous incarnation of the same one) patching in its own.
- **If webhook TLS verification is failing** (`x509: certificate signed by
  unknown authority` in the API server's rejection, or in
  `kubectl get events` around a `Plant` write), confirm the `caBundle` is
  actually populated:

  ```bash
  kubectl get validatingwebhookconfiguration plant-operator-validating-webhook-configuration \
    -o jsonpath='{.webhooks[0].clientConfig.caBundle}' | wc -c
  ```

  A byte count of `0` means the operator's startup patch never ran or
  failed — check its logs for `"unable to patch webhook CA bundles"`. A
  non-zero count that's still failing verification most likely means the
  webhook `Service`'s DNS name doesn't match what the certificate's SANs
  cover; `webhookServiceDNSNames` computes these from `--webhook-service-name`
  and `POD_NAMESPACE` (via the downward API), so an operator running under a
  webhook `Service` name it wasn't told about (a customized Helm release
  name, for instance) is the thing to check first.
- **Redeploying the operator is always a safe way to force a fresh
  certificate and a fresh `caBundle` merge** — there is no cross-restart
  state to corrupt, by design.
