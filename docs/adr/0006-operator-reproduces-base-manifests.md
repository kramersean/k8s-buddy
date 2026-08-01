# 6. The operator reproduces deploy/kustomize/base rather than replacing it

## Status

Accepted

## Context

K8s Buddy now describes the same buddy-api workload twice.

- `deploy/kustomize/base/*.yaml` — static manifests: a Namespace, ServiceAccount,
  ConfigMap, Deployment, two Services, a PodDisruptionBudget, and three
  NetworkPolicies. This is what `make demo` applies.
- `internal/controller/resources.go` — pure Go builders producing a Deployment,
  Service, ConfigMap, PodDisruptionBudget, ServiceAccount, and NetworkPolicy for
  a `Plant`, plus a seventh, conditional ServiceMonitor (added in Plan 3 Task 2;
  see ADR 0008) created only when the Prometheus Operator's CRD is present. This
  is what `make demo-operator` applies, indirectly, by creating a Plant and
  letting the operator reconcile it.

The two agree today on every value that matters: the same pod and container
security contexts, the same three probes with the same cadences, the same
resource envelope, the same `terminationGracePeriodSeconds`, the same
`imagePullPolicy`. Nothing but discipline kept them in agreement, and the only
acknowledgement that they were two descriptions of one thing was a doc comment
in `resources.go`.

Three options were considered.

- **Delete the static manifests.** The operator is the headline; the static path
  duplicates it. Rejected: the static path is Plan 1's demo and the artifact a
  reviewer reads first, it works with nothing installed but `kubectl`, and it is
  the fallback when the operator itself is what is broken. An operator repo whose
  workload can only be inspected by reading Go is worse for the audience this
  project is for, not better.
- **Delete the Go builders and have the operator apply the static manifests**
  (embed the YAML, template it, apply it). Rejected: the operator would then be a
  YAML templating engine with a `Plant`-shaped front end, and the reconciliation
  loop — diffing desired against observed, field by field, and correcting only
  what it owns — is the thing this project exists to demonstrate. Templating
  reintroduces every problem `mutateDeployment`'s field-by-field copy exists to
  avoid: applying a whole rendered spec clobbers server-assigned fields and
  produces a write on every single pass.
- **Keep both and make the coupling enforceable.** Chosen.

## Decision

Both descriptions stay. Their relationship is made explicit in three places:

1. This ADR, which states which is which and why both exist.
2. A header comment on `deploy/kustomize/base/deployment.yaml` naming
   `internal/controller/resources.go` as its Go twin, and the reciprocal comment
   at the top of `resources.go`.
3. `internal/controller/manifest_drift_test.go` — a test that parses the static
   YAML with `sigs.k8s.io/yaml` and asserts, field by field, that the security
   contexts, probe timings, resource requests and limits,
   `terminationGracePeriodSeconds`, `imagePullPolicy`, container ports, and
   ConfigMap key set match what the builders produce for an equivalent Plant.

The two do differ in a handful of places, all deliberate. Every one of those
differences is asserted **explicitly** by the same test, with its reason, rather
than excluded from comparison:

| Difference | Why |
| --- | --- |
| 3-key vs 2-key `spec.selector` | A selector is immutable after creation; the operator's two keys are both permanently tied to a Plant's identity, while `component` is identical across every Plant and could never disambiguate one Plant's pods from another's. |
| `managed-by: kustomize` vs `plant-operator` | So `kubectl get -l app.kubernetes.io/managed-by=plant-operator` returns operator-managed objects and nothing else. It is also the label the operator's informer cache is narrowed by. |
| `serviceAccountName: buddy-api` vs the Plant's name | Each Plant gets its own identity. What must *not* differ — and is asserted — is that the field is set at all. |
| Untagged image vs `spec.image` | The static manifest's tag comes from the kustomization's `images:` transformer, which `make deploy` overrides with an immutable git SHA. |
| A second NodePort Service, static only | A nodePort is a cluster-wide singleton. Every Plant creating one at 30080 would mean the second Plant on a cluster never gets a Service. |
| PDB named `-pdb` in the operator | Six of the seven children take the Plant's bare name; suffixing keeps one uniform naming rule instead of one that depends on which kinds happen to collide. |
| 3 namespace-wide NetworkPolicies vs 1 Plant-scoped one | `podSelector: {}` is a claim over every pod in the namespace. Two Plants sharing a namespace would each own an identical object and fight over it forever, and deleting either would remove the other's default-deny. A NetworkPolicy is deny-by-default for every pod it selects, so scoping to the Plant costs nothing. |
| Probes: `timeoutSeconds` / `successThreshold` / `scheme` set in Go, omitted in YAML | The reconciler diffs the builder's output against the live object every pass. A field left unset in Go would never equal the server's defaulted value, producing a permanent phantom diff and a write on every reconcile. |

## Consequences

- Tightening a probe, a limit, or a security-context field in one description and
  not the other fails `make test`. The duplication is still duplication, but it
  can no longer drift silently, which was the actual risk.
- The drift test pins the intended differences as tightly as the intended
  agreements. Removing `serviceAccountName` from the static manifest, or letting
  the NodePort drift off 30080, fails the same test — the differences are
  asserted, not skipped.
- A reviewer gets both entry points: `kubectl kustomize deploy/kustomize/base` to
  read the whole workload as YAML in one screen, and `resources.go` to read the
  same workload as the thing an operator reconciles toward.
- The drift test reads the raw YAML rather than running `kubectl kustomize`, so
  it stays a pure unit test with no external binary. The cost is that the two
  kustomize-applied labels and the image tag are absent from what it parses; each
  assertion that would care about that handles it explicitly.
- If a third description ever appears (a Helm chart in Plan 3 is the obvious
  candidate), it must either be generated from one of these two or gain its own
  drift assertions. Three hand-maintained copies of one workload would not be
  defensible, and this ADR should be revisited at that point rather than
  stretched.
