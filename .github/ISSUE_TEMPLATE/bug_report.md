---
name: Bug report
about: Something in K8s Buddy is broken or behaves differently than documented
title: "[bug] "
labels: bug
assignees: ''
---

**Before filing:** check the README's ["What this is not"](../../README.md#what-this-is-not)
and [ADR 0008](../../docs/adr/0008-deferred-to-plan-3.md) — several absences
in this project (no HPA, three dropped chaos modes, no `PlantSpec.chaos`)
are deliberate and documented, not bugs.

## What happened

A clear description of the incorrect behavior.

## What you expected

What should have happened instead — ideally with a pointer to the README,
an ADR, or a code comment that describes the expected behavior, if one
exists.

## How to reproduce

The exact commands, in order, starting from a clean state if possible:

```bash
make kind-down    # if you want to rule out accumulated cluster state
make demo-operator
# ...
```

## Environment

- OS:
- Docker version (`docker --version`):
- kind version (`kind --version`):
- kubectl version (`kubectl version --client`):
- Go version (`go version`), if building from source:
- Output of `git rev-parse HEAD`:

## Logs / output

```
paste the relevant kubectl describe / logs / make output here
```

If this involves the operator, `kubectl -n k8s-buddy-system logs deploy/plant-operator --tail=200`
is usually the fastest way to see what it thinks happened. If it involves a
`Plant` stuck in a bad state, `kubectl -n k8s-buddy-plants describe plant <name>`
first — see the [runbook](../../docs/runbook/README.md) for what a given
`Degraded` reason means.
