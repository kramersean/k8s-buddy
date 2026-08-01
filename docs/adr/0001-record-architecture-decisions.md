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

The one narrow exception: an ADR that is itself a catalog of several small,
independently-tracked items — ADR 0008 is the case that exists today, three
deferred-or-dropped items under one roof — may record a status change to ONE
of those items as a dated, commit-referenced amendment appended to that
item's own section, with the ADR's Status header updated to point at it
("Accepted. Amended twice since: ..."), rather than forking a new ADR number
for what is a one-line status flip on an already-recorded item. The exception
is deliberately narrow and does not weaken the rule above:

- It applies only to *amending the status* of an item already recorded in
  that same catalog ADR — never to rewriting or deleting the original
  reasoning, which stays exactly as first written.
- The amendment is additive and dated: it names the commit, states plainly
  what changed, and reads as a note appended next to the original text, not a
  replacement of it. A reader still sees both the original "why" and the
  later "why it changed," in one place, in order.
- Any decision that does not already live in such a catalog, or any change
  substantial enough to need its own Context/Decision/Consequences, still
  gets a new, superseding ADR exactly as described above.

This is a concession to project scale, stated rather than left as a silent
inconsistency: forking a new ADR number for every status change to one line
of a three-item deferred-items list would fragment a single coherent
narrative across several files for no reader's benefit. A decision large
enough to need its own reasoning still gets its own ADR.

## Consequences

- Reviewers can answer "why" questions by reading `docs/adr/` instead of
  guessing from commit history or asking the author.
- Every task in the project's build sequence that makes a debatable
  architectural choice is expected to add or update an ADR alongside the code.
- The overhead is small — one Markdown file per decision — and is paid back
  the first time a reviewer or future contributor would otherwise have had to
  reverse-engineer the reasoning.
