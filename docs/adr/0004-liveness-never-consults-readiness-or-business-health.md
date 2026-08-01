# 4. Liveness never consults readiness or business health

## Status

Accepted

## Context

buddy-api exposes three health-shaped endpoints: `/healthz` (liveness),
`/readyz` (readiness), and `/status` (the plant's mood and health score).
Because all three answer some form of "how are you?", the tempting
implementation is to have one feed the others — `/healthz` returning 503 when
the pod is not ready, or when the health score is low.

That is one of the most common Kubernetes misconfigurations, and the two probes
answer genuinely different questions:

- **Liveness** asks "is this process wedged, and must it be killed and
  restarted?" A failure here causes the kubelet to restart the container.
- **Readiness** asks "should traffic be sent here right now?" A failure here
  removes the pod from Service endpoints and nothing else.

A plant that is merely thirsty — high error rate, latency over budget — is not
wedged. Neither is a pod that `chaos-buddy` has deliberately marked not-ready
for a demo, nor one in phase 1 of its graceful shutdown sequence (ADR 0002),
which marks itself not-ready on purpose and stays that way for several seconds
while still serving in-flight requests.

If `/healthz` mirrored `/readyz`, every one of those states would cause a
container restart. The consequences compound: the restart destroys the very
in-flight requests graceful shutdown exists to protect; it resets the in-memory
`/work` observation window that the mood is computed from, so the pod comes
back reporting a perfect score and the real signal disappears; and it hides
a recoverable degradation behind a CrashLoopBackOff, which reads as "the
application is broken" rather than "the application is accurately reporting bad
news." A degraded-but-running pod would be converted into a crash-looping one
by the health check that was supposed to protect it.

## Decision

`/healthz` always returns 200 whenever the process is alive and able to run a
handler at all. It never consults `s.ready`, the mood engine, or the health
score.

`/readyz` alone reflects readiness. `/status` alone reflects business health.
The liveness probe is a guard against a wedged process — one that can no longer
execute a handler — and nothing else. `startupProbe` also points at `/healthz`,
for the same reason: startup is about "did the process come up", not "is it
healthy enough to serve."

## Consequences

- A degraded pod stays running and stays observable. Its mood, health score,
  and metrics remain readable for exactly as long as the degradation lasts,
  which is what makes the demo watchable at all.
- Chaos experiments target readiness, never liveness, and therefore never
  produce restarts. Pod-kill chaos is a separate, deliberate mechanism.
- A genuinely deadlocked process is still restarted, because a wedged process
  cannot serve `/healthz` either — the failure mode liveness exists for is
  still covered.
- The trade-off is that a process which is alive but functionally useless (for
  example, permanently unable to reach a dependency it needs) will never be
  restarted by the kubelet. That is the correct call here: buddy-api has no
  such dependency, and a restart would not fix one if it did.
- `TestHealthz_AlwaysOK_EvenWhenNotReady` in `internal/api` is the regression
  guard; it must not be weakened.
