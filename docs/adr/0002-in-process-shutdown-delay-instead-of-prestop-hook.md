# 2. In-process shutdown delay instead of an exec preStop hook

## Status

Accepted

## Context

Removing a pod from service without dropping requests requires a delay between
"this pod stops being ready" and "this pod stops accepting connections."

Marking a pod not-ready does not remove it from Service endpoints instantly.
The kubelet must observe the failing readiness probe, the endpoints controller
must update the Endpoints/EndpointSlice object, and then every node's
kube-proxy must receive and apply that update before it stops sending the pod
new connections. That propagation is real wall-clock time, spread across the
whole cluster. A process that closes its listener the moment SIGTERM arrives
will refuse connections that a not-yet-updated kube-proxy is still routing to
it — dropped requests during what should have been a clean rollout. This is the
single most common way a "graceful shutdown" implementation is not actually
graceful: it looks correct in a local test, where nothing else is watching
readiness, and only fails under real load-balanced traffic.

The conventional fix is a `lifecycle.preStop` hook running `exec: ["sleep",
"5"]`, which delays SIGTERM delivery long enough for endpoint removal to
propagate. That hook cannot work in this image. `build/Dockerfile.buddy-api`
ships `gcr.io/distroless/static-debian12:nonroot`, which has no `/bin/sh` and
no `/bin/sleep` to exec into (see ADR 0005 for why that base image was chosen).
An exec preStop would fail on every single termination, surfacing as a
`FailedPreStopHook` event — a loud, permanent misconfiguration rather than a
silent no-op.

The `httpGet`-based preStop variant would at least run, but it buys nothing
here: it fires and returns immediately, and it does not extend or shorten
`terminationGracePeriodSeconds`. It would be decoration.

## Decision

buddy-api implements the delay itself, in `gracefulShutdown` in
`cmd/buddy-api/main.go`, in a fixed order:

1. `SetReady(false)` immediately on SIGTERM, before anything else, so the clock
   on endpoint removal starts as early as possible.
2. Sleep `BUDDY_SHUTDOWN_DELAY` (default 5s, set in the ConfigMap) to let that
   readiness change propagate through Endpoints/EndpointSlice and every node's
   kube-proxy.
3. `http.Server.Shutdown` with a 15s drain timeout, so requests already in
   flight finish instead of being cut off.

No `lifecycle.preStop` hook is defined on the container.
`terminationGracePeriodSeconds: 30` gives that sequence (5s delay + up to 15s
drain + margin) comfortable room before the kubelet sends SIGKILL.

## Consequences

- The shutdown ordering is executable, unit-testable Go rather than a
  YAML-and-shell contract split across two files, and each phase logs as it
  happens, so the ordering is directly observable in `kubectl logs` rather than
  taken on faith.
- The delay is configurable per environment through `BUDDY_SHUTDOWN_DELAY`
  without editing the Deployment.
- The distroless base image stays viable; nothing forces a shell back into the
  runtime image.
- The coupling is that `terminationGracePeriodSeconds` must always exceed
  `BUDDY_SHUTDOWN_DELAY` + the 15s drain timeout. Raising the delay without
  raising the grace period means the kubelet SIGKILLs the process mid-drain.
  Both values are in this repository, but not in the same file.
- Every future K8s Buddy component that terminates gracefully must implement
  this sequence itself; it cannot be added from outside as a pod-spec fragment.
