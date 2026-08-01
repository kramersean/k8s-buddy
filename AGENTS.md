# K8s Buddy Agent Instructions

## Project Vision

K8s Buddy is a portfolio project whose job is to prove Kubernetes competence to
a technical reviewer in under five minutes. It does that by making cluster
self-healing *visible*: a friendly "talking plant" workload reports its mood,
controlled chaos wilts it, and Kubernetes brings it back while Prometheus and
Grafana record the whole arc.

Two reviewers matter, and both must come away convinced:

- A recruiter or hiring manager who reads only the README and looks at screenshots.
- A senior Kubernetes engineer who opens `internal/controller/` and the CI workflows.

The central design decision is that the plant is a **Custom Resource**, not a
hardcoded Deployment. Applying a `Plant` manifest causes a custom operator to
create and continuously reconcile the workload behind it — CRDs,
reconciliation loops, status subresources, owner references and garbage
collection, finalizers, admission webhooks, leader election, and
least-privilege RBAC are all genuinely exercised, not simulated.

## The Four Components

All Go, in a single module (`github.com/sean-kramer/k8s-buddy`):

1. **buddy-api** — the plant itself. An HTTP service exposing `/healthz`,
   `/readyz`, `/status` (plant-themed mood JSON), `/work` (simulated load with
   configurable latency and error rate), and `/metrics`. Mood is a pure
   function of health inputs, isolated in `internal/mood` so it is
   unit-testable without HTTP or Kubernetes.
2. **plant-operator** — a controller-runtime operator owning the `Plant`
   custom resource (`buddy.k8s-buddy.io/v1alpha1`). Reconciles Deployment,
   Service, ConfigMap, PodDisruptionBudget, HorizontalPodAutoscaler, and
   ServiceMonitor as owned resources.
3. **chaos-buddy** — a controlled failure injector with deliberately narrow
   RBAC (list/delete pods matching one label selector, in one namespace, and
   nothing else). Modes: pod-kill, readiness-flap, latency, cpu-burn, oom.
4. **Observability** — kube-prometheus-stack plus Loki, with dashboards and
   alerting rules committed to the repo and provisioned automatically, never
   clicked together by hand.

See `docs/superpowers/specs/2026-07-31-k8s-buddy-platform-showcase-design.md`
for the full design and `docs/adr/` for the reasoning behind specific
decisions.

## Local-Only Constraint

The entire demo runs on a local `kind` cluster. There is no deployment to real
cloud infrastructure, no managed Kubernetes service, and no external
dependency the reviewer would need credentials for. `make demo` on a clean
machine with Docker is the whole install story.

## How to Validate

The Makefile is the single entry point; use its targets rather than raw
`go`/`docker`/`kubectl` invocations so local runs and CI never drift apart.

```bash
make help            # list every available target

# Static checks and tests
make fmt              # gofmt -w across the module
make vet              # go vet ./...
make lint             # golangci-lint, pinned version, installed into .tools/
make test             # unit tests
make test-race        # unit tests with the race detector (needs cgo; CI runs this on ubuntu-latest)
make test-cover       # unit tests with a coverage profile, then a per-function report

# Build
make build            # compile every ./cmd/* binary into bin/
make docker-build     # build the buddy-api image, tagged with the short git SHA and :dev

# Local cluster
make kind-up          # create the kind cluster (k8s-buddy) if it does not exist
make kind-load        # load the built image (SHA tag and :dev) into the kind cluster
make kind-down        # delete the kind cluster
make deploy           # apply the manifests, pinning the image to the immutable git SHA
make undeploy         # remove the manifests from the current context
make status           # pods/services/PDB plus rollout status in the k8s-buddy namespace
make logs             # tail logs from every buddy-api pod
make demo             # end-to-end: kind-up -> build -> load -> deploy -> wait -> hack/demo.sh

# Housekeeping
make clean            # remove bin/, .build/, and coverage output
make tools            # install/update pinned developer tooling into .tools/
make tools-clean      # remove .tools/
make rename-module    # rewrite the module path everywhere (MODULE=github.com/you/repo)
```

`make deploy` never applies `deploy/kustomize/base` directly. The base pins a
mutable `:dev` tag, and re-applying a mutable tag after a rebuild produces a
byte-identical PodSpec, so no rollout happens and the cluster silently keeps
running the old image. `deploy` renders a generated overlay under `.build/`
(gitignored) that pins the immutable short git SHA instead. Use it rather than
a raw `kubectl apply -k`.

Prefer `kubectl apply --dry-run=client` and `helm template` before applying
anything for real.

## Hard Constraints

- Work only inside this repository.
- Do not access files outside the repo.
- Do not read or expose secrets.
- Do not touch SSH keys, cloud credentials, kubeconfigs outside expected local
  development usage, or personal files.
- Do not deploy to real cloud infrastructure.
- Prefer kind, local Docker, local Helm, and dry-run validation.
- Make small, reviewable changes.
- Add or update tests when behavior changes.
- Run validation commands after changes.
- Do not delete major files without explaining why.

## Development Style

Prefer vertical slices over giant rewrites. Each change should leave the
project in a demonstrable state.

Good:
- Add `/status` endpoint with tests.
- Add one metric and document it.
- Add one chaos mode and validate it.
- Add one Grafana dashboard panel.

Bad:
- Rewrite the whole app.
- Replace the stack.
- Add a huge framework without need.
- Make architecture more complex than the demo requires.
