---
name: Feature request
about: Suggest something K8s Buddy doesn't do yet
title: "[feature] "
labels: enhancement
assignees: ''
---

**Before filing:** K8s Buddy is a portfolio project with a deliberately
bounded scope, not a platform aiming for feature completeness. Check
whether what you're proposing is already an intentional non-goal:

- The README's ["What this is not"](../../README.md#what-this-is-not) —
  no cloud deployment, no persistence, no auth on buddy-api, no service
  mesh, no multi-cluster, no HPA (permanent, see
  [ADR 0008](../../docs/adr/0008-deferred-to-plan-3.md)).
- The design spec's ["Explicit Non-Goals"](../../docs/superpowers/specs/2026-07-31-k8s-buddy-platform-showcase-design.md#explicit-non-goals) —
  no Argo CD/GitOps, no OpenTelemetry tracing, no cloud deployment.

If your idea is one of these, it's likely to be closed as out of scope
rather than implemented — that's a statement about this project's intended
size, not about the idea's merit.

## What's missing

A clear description of the capability or behavior you'd like to see.

## Why it matters

What does this let a reviewer see or understand that they currently can't?
K8s Buddy's whole point is proving Kubernetes concepts are genuinely
exercised, not simulated — a feature request lands best when it names
*which* concept it would newly demonstrate, or which existing demonstration
it would make clearer.

## Proposed approach (optional)

If you have a rough idea of how this would fit the existing architecture
(`cmd/{buddy-api,plant-operator,chaos-buddy}`, `internal/{mood,chaos,controller}`),
sketch it here. It doesn't need to be a full design — a pointer at which
package would own the new behavior is enough to start a conversation.
