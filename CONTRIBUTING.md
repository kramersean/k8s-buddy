# Contributing

K8s Buddy is a portfolio project, but it's built and tested the way a real
platform team would build it — small, reviewable changes, tests alongside
behavior, and CI as the actual gate, not a formality. If you're reading this
to send a PR, or just to understand how this repository stays consistent
with itself, this is the guide.

## Before you start

Read [`AGENTS.md`](AGENTS.md) — it covers the Makefile-as-single-entry-point
convention, why Plants live in `k8s-buddy-plants` and not `default`, and the
hard constraints every change in this repo respects (no cloud deployment,
no secrets committed, small vertical slices over broad rewrites).

For anything non-trivial, skim the relevant [ADR](docs/adr/) first. If your
change would contradict one, the ADR needs to be superseded explicitly (a
new, numbered ADR that says so), not silently worked around.

## Local setup

```bash
git clone https://github.com/kramersean/k8s-buddy.git
cd k8s-buddy
make tools     # installs golangci-lint, controller-gen, setup-envtest into .tools/, pinned versions
make build     # compiles every ./cmd/* binary
make test      # unit tests (fast; excludes envtest)
```

See the [README's quickstart](README.md#quickstart) for the full
kind-cluster path, and `make help` for the complete target list — it is the
source of truth for what's runnable, and this document will drift out of
sync with it eventually, so trust `make help` over prose here.

## Making a change

1. **Small, vertical slices.** One endpoint, one metric, one chaos mode, one
   dashboard panel — each change should leave the project in a
   demonstrable, working state. Not a rewrite of the whole app, not a
   framework swap.
2. **Tests alongside behavior**, not after it. `internal/mood` and
   `internal/chaos`'s decision logic are pure functions specifically so they
   can be tested without a cluster — if you're adding logic that *could* be
   pure, keep it that way and test it directly rather than only through an
   integration path.
3. **Run validation before you open a PR:**

   ```bash
   make fmt lint vet test
   make manifests generate   # if you touched +kubebuilder markers
   git diff --exit-code config/ api/   # CI fails the build on drift here — check it locally first
   ```

   If your change touches `internal/controller`, also run
   `make test-envtest` — it boots a real `kube-apiserver` + `etcd` and is
   the single highest-signal test in this repository. It is not part of
   `make test` (it costs real wall-clock time and a one-time binary
   download), so it's easy to forget; CI will not let you.
4. **Regenerate what's generated, never hand-edit it.**
   `config/crd/bases/`, `config/rbac/role.yaml`, `config/webhook/manifests.yaml`,
   and `api/v1alpha1/zz_generated.deepcopy.go` are all produced by
   `make manifests generate` from `+kubebuilder` markers. `charts/k8s-buddy/crds/`
   is kept in sync with `config/crd/bases/` by `make helm-sync-crd`. CI
   enforces all of this with `git diff --exit-code` — a hand-edit to any of
   these files will pass locally and fail in CI the moment someone
   regenerates on top of it.
5. **If you touch a security-relevant default** (a `SecurityContext` field,
   an RBAC verb, a `NetworkPolicy` rule, the webhook allowlist), say so
   explicitly in the PR description, the way [ADR 0009](docs/adr/0009-webhook-certificate-strategy.md)
   calls out its own RBAC increase rather than folding it silently into a
   larger commit.

## Conventional commits

This repository uses [Conventional Commits](https://www.conventionalcommits.org/)
(`feat:`, `fix:`, `docs:`, `ci:`, `chore:`, ...) — look at `git log` for the
exact style in practice. A commit message should explain *why*, not just
restate the diff.

## Adding an ADR

An ADR is warranted when a decision would change the answer to "why does
this repository look the way it does," has a plausible alternative a
competent engineer might expect instead, or constrains future work in a way
that isn't obvious from the code alone. Copy the Context / Decision /
Consequences structure from any existing file in [`docs/adr/`](docs/adr/),
number it sequentially, and never edit an accepted ADR's decision after the
fact — a reversal gets a new ADR that supersedes the old one, which stays in
place, marked superseded. The one narrow exception, and when it applies, is
spelled out in [ADR 0001](docs/adr/0001-record-architecture-decisions.md)
itself: a dated, commit-referenced status amendment to one item already
recorded in a catalog-style ADR (ADR 0008 is the current example), never a
rewrite of the original reasoning.

## CI

`.github/workflows/ci.yaml` runs six jobs on every PR: `lint`, `test`
(including `test-envtest`), `build` (multi-arch images, SBOM), `scan`
(Trivy, fails on HIGH/CRITICAL), `manifests` (kubeconform + Helm lint/test),
and `e2e` (a real 3-node kind cluster, the operator, the webhooks, and a
live garbage-collection proof). Every one of these calls a `make` target —
never a raw `go`/`docker`/`kubectl` command — specifically so a green
`make <target>` locally means the same thing in CI. If a CI job fails and
you can't reproduce it locally with the equivalent `make` target, that
mismatch is itself worth reporting.

## Reporting issues

Use the issue templates: [`.github/ISSUE_TEMPLATE/bug_report.md`](.github/ISSUE_TEMPLATE/bug_report.md)
for something that's broken, [`.github/ISSUE_TEMPLATE/feature_request.md`](.github/ISSUE_TEMPLATE/feature_request.md)
for something that isn't here yet. If you're unsure whether something is a
bug or a documented limitation, check [the README's "What this is not"](README.md#what-this-is-not)
and [ADR 0008](docs/adr/0008-deferred-to-plan-3.md) first — several
absences here (the HPA, three chaos modes, `PlantSpec.chaos`) are
deliberate and recorded, not oversights.
