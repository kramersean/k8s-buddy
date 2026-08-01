# 7. No finalizer on Plant

## Status

Accepted

Supersedes the finalizer behavior introduced in Plan 2, Task 3.

## Context

The `plant-operator` originally added a finalizer, `buddy.k8s-buddy.io/finalizer`,
to every `Plant` it observed, and removed it again when the Plant was deleted.
The removal path did no cleanup: it logged a line, emitted a `PlantDeleting`
Event, and removed the finalizer. Its own doc comment explained, correctly, why
it deleted nothing —

> Every one of them carries a controller owner reference back to this Plant, and
> Kubernetes' own garbage collector removes owned objects automatically once
> their owner is gone. […] Trusting garbage collection, not reimplementing it, is
> the point.

— which is precisely the argument against having the finalizer at all.

A finalizer is a **blocking** mechanism. Once it is on the object, the API server
records a `deletionTimestamp` on `kubectl delete` and then refuses to remove the
object until every finalizer string is gone. The only actor that can remove
`buddy.k8s-buddy.io/finalizer` is the operator. So the actual, load-bearing
effect of the finalizer was:

- **The operator is running.** `kubectl delete plant fernie` takes one extra
  round trip and then completes. No cleanup happens that would not have happened
  anyway.
- **The operator is down** — crashed, scaled to zero, mid-upgrade, wedged behind
  a bad image, or simply not deployed on a cluster where the CRD still is —
  `kubectl delete plant fernie` returns successfully and the Plant does not go
  away. It sits with a `deletionTimestamp` indefinitely. `kubectl delete
  namespace` containing it hangs in `Terminating`. The recovery is hand-editing
  `metadata.finalizers` with `kubectl patch --type=merge`, which is exactly the
  kind of folklore operation this project should not be teaching.

That is a real availability liability bought with no cleanup benefit. It also
sits badly against the repo's own stated thesis, which is that garbage collection
via owner references should be *trusted* rather than reimplemented: keeping a
finalizer that cleans nothing is hedging against the mechanism the project is
trying to demonstrate confidence in.

The counter-argument for keeping it — "operators normally have finalizers, and a
portfolio project should show one" — is a case for demonstrating a *correct* use
of a finalizer, not for shipping an inert one. An inert finalizer is not a
demonstration of the pattern; it is a demonstration of cargo-culting it.

## Decision

`plant-operator` adds no finalizer to a `Plant`. The `plantFinalizer` constant,
the add-and-return branch in `Reconcile`, and the `reconcileDelete` method are
removed entirely. `Reconcile` returns immediately for a Plant carrying a
`deletionTimestamp`, since there is nothing useful it can do for an object the
garbage collector is already handling.

All six owned children — Deployment, Service, ConfigMap, PodDisruptionBudget,
ServiceAccount, NetworkPolicy — continue to carry a controller owner reference
back to their Plant, set by `controllerutil.SetControllerReference`. Kubernetes'
own garbage collector removes them when the Plant goes away. That is the
mechanism, and it is the only mechanism.

The `plants/finalizers` RBAC rule is deliberately **retained**. It is unrelated to
finalizers this controller writes: the `OwnerReferencesPermissionEnforcement`
admission plugin, on clusters that enable it, requires `update` on the owner's
`finalizers` subresource before accepting an owner reference with
`blockOwnerDeletion: true`, which `SetControllerReference` sets unconditionally.
Dropping that rule would make the operator work on kind and fail on a hardened
cluster.

## Consequences

- `kubectl delete plant fernie` completes immediately and unconditionally,
  whether or not the operator is running. A cluster whose operator is broken can
  still be cleaned up with `kubectl`.
- The envtest deletion case asserts this directly: it no longer polls with
  `require.Eventually` for the object to disappear, it requires `IsNotFound` on
  the very next `Get`. A Plant that lingers means something is holding a
  finalizer, and the stricter assertion is what catches a reintroduction.
- A second envtest case asserts `metadata.finalizers` is **empty** — not merely
  that this particular string is absent — so reintroducing the same liability
  under a different name fails too.
- The `PlantDeleting` Event is gone. `kubectl describe plant` no longer shows a
  deletion Event, because there is no longer a reconcile pass during deletion to
  emit one from. Deletion is now visible only as the object disappearing and its
  children following, which is what deletion looks like for every built-in
  Kubernetes type.
- There is a genuine, narrow loss: an operator that is down when a Plant is
  deleted has no opportunity to observe that it happened. Nothing this operator
  does needs that opportunity today.
- **This decision reverses the moment the operator owns state Kubernetes cannot
  garbage-collect** — a cloud DNS record, an object-storage bucket, a row in an
  external database, a registration with a third-party service. Owner references
  only reach Kubernetes objects. The instant `plant-operator` creates something
  outside the cluster, a finalizer becomes the correct and necessary way to
  guarantee cleanup, and this ADR is superseded rather than amended. The test
  asserting "no finalizers at all" is the tripwire that will force that
  conversation rather than letting one reappear quietly.
