# 9. Webhook certificate strategy: operator-generated, self-signed, regenerated every start

## Status

Accepted

## Context

Task 4 adds two admission webhooks to `plant-operator` -- a mutating defaulter
and a validating rule set (`api/v1alpha1/plant_webhook.go`). Both are TLS
services: the API server refuses to call an admission webhook over plaintext,
so `plant-operator` needs a serving certificate the API server will trust,
before it can accept a single Plant write once the `ValidatingWebhookConfiguration`
exists.

Three ways to get that certificate, all considered:

1. **cert-manager.** The standard answer for any real kubebuilder-scaffolded
   operator: install cert-manager, annotate the webhook configurations for
   CA injection, and let its own controller mint and rotate a certificate
   into a Secret. **Not installed on this cluster, and this task's own brief
   is explicit that adding it is out of proportion for a local demo** -- a
   second Helm release, two more CRDs (`Certificate`, `Issuer`), and a second
   controller to keep healthy, all to solve a problem this project's own
   scope is small enough not to need solved generally. The same reasoning
   ADR 0008 gives for rejecting a Prometheus Adapter just to unblock an HPA
   applies here: correct in general, disproportionate for what this repo is
   demonstrating.
2. **A manual `openssl` step**, documented in the README. Rejected outright
   by this task's own constraint: "the demo must NOT require a manual
   openssl step." A step a developer has to remember, on a fresh clone, is a
   step that will eventually be forgotten, and the failure mode (webhook
   `failurePolicy: Fail` silently blocking every Plant write) is a bad first
   impression for a portfolio project's very first `make demo-operator` run.
3. **The operator generates its own self-signed CA and serving certificate,
   at every process startup**, writes them to an `emptyDir`-backed
   `CertDir`, and patches the resulting CA's PEM bytes into the `caBundle`
   field of both webhook configurations using its own ServiceAccount. No new
   component, no new CRD, no manual step, works identically whether the
   operator was installed via kustomize or the Helm chart.

Option 3 was the brief's own recommended approach, and nothing found while
implementing it changed that recommendation. It is the choice this ADR
records.

### The genuine trade-off: no cross-process durability

A cert-manager-issued certificate lives in a Secret and survives every
container that reads it. This one does not: it is generated fresh, in
memory, by `cmd/plant-operator/webhookcerts.go`'s
`generateWebhookServingCertificate`, at the top of every `main()` run, and
written only to an `emptyDir` volume -- which is deleted the moment its Pod
is. A crash, a rollout, a node reschedule: every one of them means a brand
new CA, a brand new serving certificate, and a fresh `caBundle` patch onto
both webhook configurations, every single time. Nothing about the previous
certificate is ever reused or trusted again.

This is deliberate, not an oversight bought back with a TODO -- but two
mistakes in the first version of this design are worth recording here rather
than quietly fixing, since they were genuine bugs, not just wording:

- **A short validity window was tried first, and was wrong.** The original
  version of this ADR set `webhookCertificateValidity` to 24h, reasoning that
  since the cert is regenerated every restart anyway, a long validity
  "buys nothing." That reasoning missed the actual failure mode: nothing in
  this process *proactively* regenerates the certificate before it expires
  -- regeneration only happens at the top of `main()`. A Pod that simply
  stays up past 24h (a kind demo cluster left running over a weekend is the
  concrete case, not a hypothetical one) serves an **expired** leaf
  certificate. The API server's TLS verification then fails, and with the
  validating webhook's `failurePolicy: Fail`, every Plant `CREATE`/`UPDATE`
  starts failing -- with no restart, no log line from this process (it isn't
  the one rejecting the request), and no automatic recovery, until someone
  happens to notice and manually bounces the Pod. `webhookCertificateValidity`
  is now 10 years (`87600 * time.Hour`), which turns that scenario from a
  timer-driven certainty into something that should never realistically
  happen during this project's lifetime -- and `webhookCertExpiryCheck`
  (registered on both `/healthz` and `/readyz`) is the backstop for even
  that: it reports this process unhealthy once the leaf is within
  `webhookCertExpiryReadinessMargin` (24h) of its actual `NotAfter`, so a
  failing liveness probe restarts the container (minting a fresh
  certificate) well before the old one could ever actually expire in use.
- **An overwrite-based `caBundle` patch was tried first, and was wrong.**
  The original version of this ADR also claimed "there is no version-skew
  window where a new Pod serves a new certificate that an old, un-patched
  `caBundle` doesn't yet trust" -- true only for a single Pod restarting in
  isolation, and false for a rolling update: the new Pod's certificate
  bootstrap patches its own, brand-new CA into `caBundle` **before** it is
  marked Ready, while the OLD Pod is still the Deployment's only Service
  endpoint and is still presenting its OLD leaf certificate. Overwriting the
  `caBundle` at that moment means the API server stops trusting the still-
  serving old Pod's certificate immediately -- every Plant write fails x509
  verification for the entire readiness-transition window, self-inflicted by
  the very rollout meant to fix or improve something. `mergeCABundle`
  (`cmd/plant-operator/webhookcerts.go`) fixes this by **appending**: every
  still-valid CA already in the bundle is kept, the new one is added, and
  only already-expired entries are pruned. See "Certificate lifetime,
  rollout overlap, and multiple replicas" below for the full mechanics and
  what this incidentally also fixes for `replicaCount > 1`.
- **A stale `caBundle` cannot silently linger indefinitely.** With a
  Secret-backed cert-manager certificate, an operator that failed to restart
  for months could still be serving a certificate nobody re-validated
  recently. Here, the bundle only ever accumulates entries from Pods that
  have actually run and patched their own CA in -- there is no
  independently-issued, never-verified certificate sitting untouched for
  months, because there is no path to a `caBundle` entry that didn't come
  from a process that, at the time, was itself up and serving.

The cost: a webhook server that is down for any reason is not a webhook
server anyone could dial with a still-valid, still-trusted certificate to
fall back on for THAT Pod specifically -- but with `mergeCABundle` in place,
that cost is scoped to the one Pod that's actually down, not to the whole
`caBundle`: any other still-running replica's own CA remains trusted, and a
down operator has nothing useful to validate against anyway (see the
failure-mode discussion below).

## Decision

`cmd/plant-operator/webhookcerts.go`:

- **`generateWebhookServingCertificate`** creates a fresh ECDSA P-256 CA
  (self-signed, `IsCA: true`, valid `webhookCertificateValidity` -- 10 years)
  and a leaf serving certificate it signs, carrying every DNS name the
  webhook Service could be dialed by (`webhookServiceDNSNames`: bare,
  namespaced, `.svc`, and `.svc.cluster.local` forms) plus `127.0.0.1` for
  local debugging. It writes `tls.crt`/`tls.key` to `--webhook-cert-dir`
  (default `/tmp/k8s-webhook-server/serving-certs`, controller-runtime's own
  `webhook.Server` convention) and returns both the CA's own PEM bytes and
  the leaf's `NotAfter`, the latter feeding `webhookCertExpiryCheck`.
- **`patchWebhookCABundles`** builds a direct (uncached) client from the
  operator's own in-cluster config, `Get`s the two webhook configurations by
  name, **merges** (via `mergeCABundle` -- appends, dedupes, prunes expired
  entries; never overwrites) the new CA's PEM bytes into every entry's
  `clientConfig.caBundle`, and `Update`s them -- retrying with a bounded
  exponential backoff (up to ~30s total) so a `kubectl apply -k
  deploy/kustomize/operator` that applies the RBAC/webhook manifests and the
  Deployment in one pass, with no ordering guarantee between them, does not
  race a not-yet-created `ValidatingWebhookConfiguration`.
- `cmd/plant-operator/main.go` calls both, in that order, **before**
  constructing the `ctrl.Manager` (and therefore before its webhook server
  binds `--webhook-port`, default 9443) -- a request can never reach a
  webhook server whose own certificate isn't already on disk, and the
  `caBundle` merge naming that certificate's CA lands before the server
  starts listening for it.
- **`webhookCertExpiryCheck`** is registered on both `mgr.AddHealthzCheck`
  and `mgr.AddReadyzCheck` under the name `webhook-cert-expiry`, alongside
  the existing `healthz.Ping` checks. It reports unhealthy once the leaf
  certificate is within `webhookCertExpiryReadinessMargin` (24h) of its
  actual expiry -- registered on `/healthz` specifically so a failing
  liveness probe actually restarts the container (the only thing that mints
  a fresh certificate), and on `/readyz` too so the Pod is pulled out of the
  webhook Service's endpoints as early as possible, before the liveness
  probe's own `failureThreshold * periodSeconds` finally triggers the
  restart.

### Certificate lifetime, rollout overlap, and multiple replicas

`mergeCABundle` is what makes both a rolling update and running more than
one replica safe -- neither of which a plain overwrite (the version this
design shipped with first) would have been:

- **Rolling update.** The webhook server is not gated by leader election, so
  as soon as a new Pod's container starts, its certificate bootstrap runs
  and merges its own brand-new CA into both `caBundle`s -- before that Pod
  is Ready, while the OLD Pod is still the Deployment's only Service
  endpoint and is still presenting its OLD leaf certificate. Because
  `mergeCABundle` appends rather than overwrites, the old CA stays in the
  bundle throughout the overlap: the still-serving old Pod remains trusted
  right up until it actually stops serving, and the new Pod's own CA is
  already trusted the moment traffic starts reaching it. There is no window
  where the currently-serving Pod's certificate is untrusted.
- **Multiple replicas.** `charts/k8s-buddy/values.schema.json`'s
  `replicaCount` allows up to 5, and every replica independently generates
  and trusts only its own CA (there is no shared identity or shared Secret).
  With an overwrite, only the last replica to patch would ever end up
  trusted, and the API server's Service-level load balancing has no
  affinity to route around that -- roughly `(n-1)/n` of admission calls
  would fail TLS verification depending on which replica happened to answer.
  With `mergeCABundle`, the bundle accumulates every currently-running
  replica's CA, so any of them can be dialed successfully. `replicaCount > 1`
  is therefore genuinely safe for the webhook server too, not merely
  tolerated -- see `values.schema.json`'s own updated description.
- **Bundle growth is capped by count, not merely bounded by pruning.** An
  earlier version of this design relied on pruning-by-expiry alone, reasoning
  that a demo-grade operator's restart frequency would keep growth
  "acceptable." That reasoning was wrong: every restart mints a brand-new,
  random ECDSA CA -- there is no persistent key identity for "the same CA"
  the way a cert-manager-issued one would have, so `mergeCABundle`'s
  dedup-by-byte-equality does NOT collapse restarts into one entry, only
  exact retries of the same generation. With `webhookCertificateValidity` at
  10 years, pruning-by-expiry effectively never fires either. An operator
  restarted daily would accumulate hundreds of live, permanently-valid trust
  anchors over the project's lifetime -- unbounded growth AND a quietly
  widening trust surface, on the exact field this whole design exists to
  keep tightly scoped. `mergeCABundle` now also caps the bundle at
  `maxRetainedCAs` (3) entries, keeping the most recently-issued (by
  `NotBefore`) and dropping the rest regardless of whether they've expired
  yet: a CA whose process has already exited has no ongoing reason to stay
  trusted. 3 covers one rolling update's old+new overlap (2) plus one full
  extra generation of margin. Verified live: two consecutive redeploys
  (a real image rollout, then a `kubectl rollout restart`) each minted a
  new CA; the `caBundle` count went 2 -> 3 -> 3, capping exactly at
  `maxRetainedCAs` on the second redeploy rather than growing to 4, with a
  fresh Plant write succeeding after each restart (proving the newest CA
  was trusted, not just present).
- **The alternative not taken: persisting the CA in a Secret.** The
  production-grade answer to "avoid minting a new CA on every restart"
  entirely is to generate the CA once, store it in a `Secret`, and have
  every subsequent process start read the existing one back out rather than
  minting fresh -- the cert-manager-adjacent pattern this project otherwise
  avoids installing a whole component for. It was deliberately not taken
  here: it would require granting `plant-operator`'s ServiceAccount RBAC to
  read (and, for the first-ever start, create) a `Secret` -- a permission
  this project has pointedly avoided everywhere else (see
  `config/rbac/role.yaml`: no `secrets` rule exists at all, and the live
  e2e suite explicitly asserts `plant-operator` cannot read secrets
  cluster-wide). Capping the bundle at `maxRetainedCAs` gets the practical
  benefit -- a caBundle that does not grow without limit -- without adding
  that RBAC surface. This is a choice, not an oversight: if a future need
  ever justifies Secret access for some other reason, revisiting CA
  persistence at the same time would be the natural next step; until then,
  the cap is the intentionally cheaper trade.

### RBAC: a real, narrowly-scoped privilege increase

Patching a cluster-scoped object needs cluster-scoped RBAC that did not
exist before Task 4:

```
+kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations;validatingwebhookconfigurations,verbs=get;update;patch,resourceNames=plant-operator-mutating-webhook-configuration;plant-operator-validating-webhook-configuration
```

This **is** a genuine privilege increase over Plan 2's RBAC (`config/rbac/role.yaml`
gained a new rule), reported as such rather than folded silently into the
same commit as everything else. It is scoped as narrowly as the
`admissionregistration.k8s.io` API allows for these two resource kinds:

- **`resourceNames` pins the grant to exactly two, specific, already-known
  objects** -- not "every `MutatingWebhookConfiguration` on the cluster,"
  which is the alternative a naive `resources: mutatingwebhookconfigurations`
  rule with no `resourceNames` would grant. `resourceNames` **is** honored by
  the Kubernetes API server for `get`, `update`, and `patch` on
  cluster-scoped resources -- confirmed against the RBAC authorizer, and
  proven by this project's own live-cluster verification (Task 4's own
  `patchWebhookCABundles` succeeding while scoped this way).
- **`list` and `watch` are deliberately absent.** Kubernetes RBAC's own
  documented limitation is that `resourceNames` cannot narrow `list` or
  `watch` requests at all -- a rule combining either verb with
  `resourceNames` is accepted by the API server but does not actually
  restrict what it returns; requesting either verb here would therefore
  have meant either granting collection-wide list/watch access (defeating
  the whole point of naming two specific objects) or shipping a rule that
  looks scoped but isn't. `patchWebhookCABundles` never lists: it already
  knows both names (they are `cmd/plant-operator`'s own
  `--mutating-webhook-configuration-name` / `--validating-webhook-
  configuration-name` flags, matching what `api/v1alpha1/plant_webhook.go`'s
  `+kubebuilder:webhookconfiguration` markers bake into
  `config/webhook/manifests.yaml`), so it only ever needs `Get`, which
  **is** correctly restricted by `resourceNames`.
- **No `create` or `delete`.** Both webhook configuration objects must
  already exist (created by the same `kubectl apply -k
  deploy/kustomize/operator` / `helm install` that deploys everything else)
  before this code ever runs; it only ever mutates a field on an object it
  did not create and will never remove.

The two Helm-chart object names differ from the kustomize path's fixed
`plant-operator-*` names (they are release-scoped --
`{{ include "k8s-buddy.fullname" . }}-mutating-webhook-configuration`, the
same pattern `templates/clusterrole.yaml`'s own `metadata.name` already
uses, and for the identical reason: two independent chart installs, or a
chart install alongside the kustomize path, must never collide on a
cluster-scoped object's name). `charts/k8s-buddy/templates/deployment.yaml`
passes both names explicitly via `--mutating-webhook-configuration-name` /
`--validating-webhook-configuration-name` args; `templates/clusterrole.yaml`'s
own `resourceNames` are templated to match. `hack/check-helm-rbac-drift.py`
was extended (not weakened) to account for this: it still asserts every
other field of every rule matches `config/rbac/role.yaml` exactly, and for
`resourceNames` specifically it asserts *presence and count* rather than
*exact value* -- a rule silently losing its narrowing altogether (chart or
source) is still caught; the fact that a release-scoped name and a
fixed name are different strings, by design, is not.

### Failure policy: Ignore for defaulting, Fail for validating -- and why that's safe

`+kubebuilder:webhook` markers on `SetupPlantWebhookWithManager`
(`api/v1alpha1/plant_webhook.go`) set:

- **Mutating (defaulting) webhook: `failurePolicy: Ignore`.** Safe to fail
  open because it has a fully adequate fallback that runs regardless of
  whether this webhook is reachable at all: the CRD's own
  `+kubebuilder:default` markers (`plant_types.go`), which
  `plant_webhook_test.go`'s `TestDefaultingAgreesWithCRDSchema` mechanically
  proves apply the *exact same values* this webhook does. An operator that
  is down when a Plant is created still gets a fully-defaulted Plant --
  CRD-level defaulting has no dependency on the webhook server being up at
  all.
- **Validating webhook: `failurePolicy: Fail`.** The brief calls this "the
  more correct choice for a validating webhook," and this project agrees:
  `Ignore` here would mean an operator outage silently stops enforcing
  species immutability and the image registry allowlist -- exactly the two
  rules that exist **because** nothing else enforces them. A validating
  webhook that fails open enforces nothing when it matters most, which is
  worse than not having it.

**`Fail` on a validating webhook is the textbook way to lock a cluster out of
managing a resource if the webhook target is unreachable — this project
avoids that specific failure mode structurally, not by hoping the operator
never goes down:**

- **The webhook's own `rules` never list the `DELETE` operation** -- only
  `CREATE` and `UPDATE` (`verbs=create;update` on both
  `+kubebuilder:webhook` markers). `kubectl delete plant <name>` never
  reaches this webhook at all, reachable or not, so an operator outage can
  never prevent a Plant from being deleted. This is the same posture ADR
  0007 already commits to for finalizers -- "nothing this operator owns
  should be able to make a Plant undeletable" -- applied to admission
  instead of garbage collection.
- **Recovering from an operator outage, concretely:** while `plant-operator`
  is down (crashed, mid-rollout, scaled to zero, or simply not yet deployed
  on a fresh cluster after the CRD and webhook configurations already are),
  every `CREATE`/`UPDATE` of a Plant is rejected with a clear "webhook ...
  failed calling webhook" error -- an honest signal, not a silent no-op or
  data corruption. Recovery is simply making the operator healthy again
  (`kubectl -n k8s-buddy-system rollout status deployment/plant-operator`,
  or fixing whatever crashed it): the moment its Pod is Ready again, the
  Service routes to it, TLS succeeds against the `caBundle` it patched in at
  startup, and writes resume with no further action. There is no manual
  `caBundle` fix-up, no finalizer to strip, no stuck object -- this is a
  strict subset of ADR 0007's own "the operator being down never makes a
  Plant undeletable" guarantee, extended to "an operator outage blocks
  *writes*, cleanly, and un-blocks itself the moment the operator is
  healthy again."
  - The one true last resort, for an admin who needs Plant writes to
    proceed *before* the operator can be fixed (e.g. debugging the operator
    itself by editing a Plant it reconciles): `kubectl delete
    validatingwebhookconfiguration plant-operator-validating-webhook-configuration`
    removes the gate entirely. This is a deliberate, visible, cluster-admin-only
    escape hatch, not a code path -- exactly the kind of "folklore operation"
    ADR 0007 warns against baking into the operator's own logic, but perfectly
    appropriate as a documented manual recovery step here, since it requires
    the same `cluster-admin`-level RBAC that could just as easily delete the
    Plant's namespace outright.
- **`timeoutSeconds: 5`** on both webhooks (well under the API server's own
  30s admission ceiling) bounds how long a single Plant write can hang
  behind an unresponsive-but-not-yet-declared-down webhook server, rather
  than tying up the requesting client for the platform default of 10s.
- No `namespaceSelector`/`objectSelector` narrows either rule further. Both
  already scope as tightly as the type system allows --
  `apiGroups: [buddy.k8s-buddy.io]`, `resources: [plants]` -- and `Plant` is
  a project-specific CRD that cannot exist in `kube-system` or any other
  namespace this project doesn't itself manage; a selector would only ever
  be useful to *exempt* a namespace Plants are never created in, which adds
  a knob with nothing real to turn.

## Consequences

- No cert-manager dependency, no manual certificate step, and no CRD beyond
  `Plant` itself -- `make demo-operator` reaches a fully working webhook
  install in the same one command it always has.
- The certificate is regenerated, and both `caBundle`s re-merged (not
  overwritten), on every single restart of `plant-operator` -- verified
  live: redeploying the operator with `make deploy-operator` grows the
  `caBundle` by the new CA (`kubectl get
  {mutating,validating}webhookconfiguration ... -o
  jsonpath='{.webhooks[0].clientConfig.caBundle}'` is longer after a
  redeploy, and decodes to more than one `CERTIFICATE` PEM block once a
  second generation has been merged in), and both configurations' `caBundle`
  are populated and non-empty within seconds of the Pod reporting Ready.
- `webhookCertificateValidity` is 10 years, not the original 24h -- an
  operator Pod can now stay up indefinitely on this project's timescale
  without its own certificate expiring out from under it.
  `webhookCertExpiryCheck` is the backstop should that ever change (a
  shorter validity introduced later, a clock badly skewed): both
  `/healthz` and `/readyz` fail once the leaf is within 24h of its actual
  expiry, so the kubelet restarts the container and a fresh certificate is
  minted before the old one could ever actually be served expired.
- `config/rbac/role.yaml` (and `charts/k8s-buddy/templates/clusterrole.yaml`)
  carry one new rule neither had before Task 4:
  `admissionregistration.k8s.io` `get`/`update`/`patch` on
  `{mutating,validating}webhookconfigurations`, `resourceNames`-pinned to
  exactly the two objects this project ships. `hack/check-helm-rbac-drift.py`
  was extended, not disabled, to keep asserting the chart and
  `config/rbac/role.yaml` agree on everything except the (by-design,
  install-method-scoped) exact resource name strings.
- The mutating webhook is safe to leave `failurePolicy: Ignore` forever: it
  duplicates, and can never diverge from (mechanically enforced by
  `TestDefaultingAgreesWithCRDSchema`), what the CRD schema already
  guarantees on its own.
- The validating webhook's `failurePolicy: Fail` means a Plant `CREATE`/
  `UPDATE` genuinely blocks while `plant-operator` is unreachable. This is
  the deliberate, correct trade for a webhook whose entire reason to exist
  is enforcing rules nothing else does; `DELETE` is structurally exempt, so
  the cluster can never become unable to *remove* a Plant, only temporarily
  unable to *write* one -- and that condition self-heals the moment the
  operator is healthy again, with no manual recovery step required in the
  ordinary case.
- `replicaCount > 1` (both the kustomize path, which pins `replicas: 1`
  deliberately but could be scaled by hand, and `charts/k8s-buddy`, whose
  schema allows up to 5) is genuinely safe for the webhook server now that
  `mergeCABundle` appends rather than overwrites -- each replica's
  independently-minted CA accumulates in both `caBundle`s rather than the
  last writer silently evicting every other replica's trust. This was NOT
  true of this ADR's first version (a plain overwrite), which is why the
  `values.schema.json` description for `replicaCount` now explains why
  `>1` works, not merely that it's "safe, not useful."
- If this project ever needs a certificate to survive a restart FOR A REASON
  OTHER than the ones already covered here (an external system independently
  verifying this specific CA's identity across restarts, for instance, which
  nothing in this project currently does), that is the point at which this
  ADR is superseded in favor of cert-manager or a shared Secret, not amended.
