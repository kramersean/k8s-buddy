# 1. Record architecture decisions

## Status

Accepted

## Context

K8s Buddy is a portfolio project designed to prove Kubernetes competence to two
kinds of reviewer: a recruiter skimming the README, and a senior Kubernetes
engineer reading the code and the CI workflows. Several decisions in this
project are non-obvious and would otherwise draw a reasonable question from
that second reviewer — why a custom operator instead of a plain Deployment,
why both Helm and Kustomize, why kind instead of minikube, why readiness-based
chaos instead of liveness-based. Without a written record, those answers exist
only in a contributor's head (or an LLM's context window) and are lost the
moment the branch merges.

## Decision

We will record every architecturally significant decision as an Architecture
Decision Record (ADR) under `docs/adr/`, numbered sequentially, using the
standard Context / Decision / Consequences form established by this document.

An ADR is warranted when a decision:

- Would change the answer to "why does this repository look the way it does,"
- Has a plausible alternative a competent engineer might reasonably expect
  instead, or
- Constrains future work in a way that is not obvious from the code alone.

ADRs are immutable once accepted. A decision that is later reversed gets a new
ADR that supersedes the old one; the old one is left in place, marked
superseded, so the history of *why* is never erased.

## Consequences

- Reviewers can answer "why" questions by reading `docs/adr/` instead of
  guessing from commit history or asking the author.
- Every task in the project's build sequence that makes a debatable
  architectural choice is expected to add or update an ADR alongside the code.
- The overhead is small — one Markdown file per decision — and is paid back
  the first time a reviewer or future contributor would otherwise have had to
  reverse-engineer the reasoning.
