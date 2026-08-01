# Plan 1 of 3 — Foundation: buddy-api and the first self-healing demo

**Spec:** `docs/superpowers/specs/2026-07-31-k8s-buddy-platform-showcase-design.md`
**Branch:** `feat/platform-showcase`
**Outcome when complete:** `make demo` brings up a multi-node kind cluster running
buddy-api, and deleting a pod visibly heals while the plant reports its mood. CI
lints, tests, and builds on every push.

Plans 2 (Plant CRD + operator) and 3 (chaos, observability, delivery, docs) follow
this one and are written after it lands.

---

## Global Constraints

Every task must honor these. Values are exact — copy them verbatim, never paraphrase.

**Module and toolchain**
- Go module path: `github.com/sean-kramer/k8s-buddy`
  *(Assumption — the user's GitHub org/username was not specified. Task 1 must
  provide a `make rename-module MODULE=...` target so this is a one-command change.)*
- `go.mod` declares `go 1.25`. Installed toolchain is Go 1.26.5.
- Single Go module at the repository root. No nested modules.

**Naming**
- Kubernetes namespace: `k8s-buddy`
- kind cluster name: `k8s-buddy`
- Image repository prefix: `ghcr.io/sean-kramer/k8s-buddy`
- Image names: `buddy-api`, `plant-operator`, `chaos-buddy`
- API group (for Plan 2, fixed now so nothing drifts): `buddy.k8s-buddy.io`,
  version `v1alpha1`, Kind `Plant`, plural `plants`, shortName `pl`

**Ports**
- buddy-api serves HTTP **and** `/metrics` on `8080` (single listener, container
  port name `http`)
- plant-operator: metrics `8081`, health probes `8082`, webhook `9443` (Plan 2)

**Standard labels** — every generated Kubernetes object carries all five:
```
app.kubernetes.io/name: <component name>
app.kubernetes.io/instance: <release or plant name>
app.kubernetes.io/component: <api|operator|chaos>
app.kubernetes.io/part-of: k8s-buddy
app.kubernetes.io/managed-by: <helm|kustomize|plant-operator>
```

**Metrics** — all Prometheus metric names are prefixed `buddy_`. Exactly these in Plan 1:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `buddy_health_score` | Gauge | none | Current health, 0–100 |
| `buddy_mood` | Gauge | `mood` | 1 for the active mood, 0 for all others |
| `buddy_ready` | Gauge | none | 1 when readiness passes, else 0 |
| `buddy_work_requests_total` | Counter | `outcome` (`success`\|`warning`\|`failure`) | Work requests |
| `buddy_work_duration_seconds` | Histogram | `outcome` | Work latency |
| `buddy_http_requests_total` | Counter | `path`, `method`, `code` | All HTTP requests |
| `buddy_build_info` | Gauge | `version`, `commit`, `go_version` | Always 1 |

**Mood ladder** — health score maps to mood by these exact thresholds:

| Score range | Mood string | Message |
|---|---|---|
| `>= 95` | `leafy` | `I'm feeling leafy and stable` |
| `>= 80` | `sprouting` | `I'm ready to rock and roll` |
| `>= 60` | `thirsty` | `Could use a drink, but I'm managing.` |
| `>= 40` | `lost-a-leaf` | `Lost a leaf, but I'm recovering.` |
| `>= 20` | `not-too-hot` | `I'm not feeling too hot.` |
| `< 20` | `wilting` | `I'm wilting. Send help.` |

Scores are clamped to `[0, 100]`. A score of exactly `95` is `leafy`; exactly
`94.999` is `sprouting`. Boundaries are inclusive at the lower edge.

**Security posture** — every container spec, in every manifest, in every task:
```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: ["ALL"]
  seccompProfile:
    type: RuntimeDefault
```
Every container also sets resource `requests` and `limits`. No exceptions, and no
`latest` image tags anywhere.

**Code style**
- Structured logging via `log/slog` with a JSON handler. No `fmt.Println`, no `log.Printf`.
- No `panic` outside `main()` initialization.
- Exported identifiers carry doc comments beginning with the identifier name.
- Errors are wrapped with `fmt.Errorf("...: %w", err)`; never discarded with `_`.
- Every task ends with `make lint test` passing.

**Testing**
- Standard library `testing` plus `github.com/stretchr/testify/require`. No other
  assertion library.
- Table-driven tests for pure logic.
- Tests assert real behavior. A test that only checks "does not panic" is not a test.

---

## Interfaces

Names and signatures later tasks depend on. Do not rename them.

```go
// package internal/mood
type Mood string
const (
    MoodLeafy      Mood = "leafy"
    MoodSprouting  Mood = "sprouting"
    MoodThirsty    Mood = "thirsty"
    MoodLostALeaf  Mood = "lost-a-leaf"
    MoodNotTooHot  Mood = "not-too-hot"
    MoodWilting    Mood = "wilting"
)
// AllMoods lists every mood, healthiest first.
func AllMoods() []Mood
// Message returns the plant-themed message for a mood.
func (m Mood) Message() string
// Signals are the inputs to the health calculation.
type Signals struct {
    Ready        bool
    ErrorRate    float64 // 0..1 over the recent window
    P95Latency   time.Duration
    LatencyBudget time.Duration
    RestartCount int
}
// Score returns a health score in [0,100] derived from s.
func (s Signals) Score() float64
// FromScore maps a health score to its Mood.
func FromScore(score float64) Mood
// Report is the /status response body.
type Report struct {
    Mood        Mood      `json:"mood"`
    Message     string    `json:"message"`
    HealthScore float64   `json:"healthScore"`
    Ready       bool      `json:"ready"`
    Species     string    `json:"species"`
    Name        string    `json:"name"`
    Uptime      string    `json:"uptime"`
    CheckedAt   time.Time `json:"checkedAt"`
}

// package internal/telemetry
type Metrics struct{ /* unexported prometheus collectors */ }
func NewMetrics(reg prometheus.Registerer, bi BuildInfo) *Metrics
func (m *Metrics) ObserveWork(outcome string, d time.Duration)
func (m *Metrics) ObserveHTTP(path, method string, code int)
func (m *Metrics) SetHealth(score float64, mood mood.Mood, ready bool)
type BuildInfo struct{ Version, Commit, GoVersion string }
func NewLogger(level slog.Level, w io.Writer) *slog.Logger

// package internal/api
type Config struct {
    PlantName            string
    Species              string
    LatencyBudget        time.Duration
    WorkErrorRate        float64
    WorkMinDelay         time.Duration
    WorkMaxDelay         time.Duration
    EnableChaosEndpoints bool
    // Rand is the randomness source for /work. Nil means a time-seeded
    // source; tests inject a fixed seed for determinism.
    Rand *rand.Rand
}
type Server struct{ /* ... */ }
func New(cfg Config, log *slog.Logger, m *telemetry.Metrics, reg *prometheus.Registry) *Server
// Handler returns the fully-wired HTTP handler.
func (s *Server) Handler() http.Handler
// SetReady flips readiness; used by chaos and by graceful shutdown.
func (s *Server) SetReady(ready bool)
```

---

## Task 1 — Repository scaffold

**Files:** `go.mod`, `Makefile`, `.golangci.yml`, `AGENTS.md` (rewrite),
`.claude/agents/*.md` (7 files), delete `.cursor/`, `docs/adr/0001-record-architecture-decisions.md`

Create the Go module and the developer entry point.

- `go mod init github.com/sean-kramer/k8s-buddy`. Add `github.com/stretchr/testify`
  and `github.com/prometheus/client_golang` as dependencies.
- **Makefile** is the single entry point; CI calls only Makefile targets so the two
  cannot drift. Required targets, each with a `##` comment for a `help` target that
  self-documents via `awk`:
  `help` (default), `fmt`, `vet`, `lint`, `test`, `test-cover`, `build`,
  `docker-build`, `clean`, `rename-module`, `tools`.
  - `lint` downloads `golangci-lint` into `.tools/` if absent — pinned version,
    never `@latest`.
  - `rename-module MODULE=x` rewrites the module path in `go.mod` and every import.
  - Use `.DEFAULT_GOAL := help` and `SHELL := /usr/bin/env bash`.
- **`.golangci.yml`** enabling at minimum: `errcheck`, `govet`, `staticcheck`,
  `revive`, `gosec`, `misspell`, `unconvert`, `unparam`, `bodyclose`, `errorlint`,
  `gocritic`. Exclude `_test.go` from `gosec` and `unparam` only.
- **Rewrite `AGENTS.md`** to describe the project as specified in the design doc:
  vision, the four components, the local-only constraint, how to validate, and the
  vertical-slice development style. Keep the existing hard constraints about not
  touching secrets, not deploying to cloud, and preferring kind. Drop the
  "do not rewrite the entire project" line — that rewrite is now done and the
  sentence would be stale.
- **Convert `.cursor/agents/*.md` to `.claude/agents/*.md`.** The seven source
  files are malformed: each file's entire body was collapsed into its YAML
  `description:` field. For each, recover the intended content from that field and
  emit a correct Claude Code agent file with frontmatter `name`, a one-line
  `description` that says *when to use* the agent, and `tools`, followed by the
  real body as markdown. Preserve each agent's original intent and responsibilities.
  Two of the seven (`repo-architect`, `observability-agent`) additionally have a
  stray `## Subagent N:` heading and an unterminated ` ```md ` fence embedded in the
  description — strip that packaging. Then `git rm -r .cursor`.
- **ADR 0001** recording that this project uses ADRs, in the standard
  Context / Decision / Consequences form.

**Verify:** `make help` lists every target; `make fmt vet` is clean;
`go build ./...` succeeds; `.cursor/` is gone; all 7 files exist under
`.claude/agents/` with valid frontmatter.

---

## Task 2 — Mood engine (`internal/mood`)

**Files:** `internal/mood/mood.go`, `internal/mood/mood_test.go`

Pure logic, no Kubernetes and no HTTP imports. This is the most-read file in the
repo for a reviewer judging code quality — make it exemplary.

Implement the `Mood` type, constants, `AllMoods`, `Message`, `Signals`, `Score`,
`FromScore`, **and `Report` plus its constructor `NewReport`** exactly as given in
the Interfaces block. `Report` lives here rather than in `internal/api` so that
mood, message, and health score are always derived together and cannot disagree;
`NewReport` is the only way Task 4 is allowed to build one.

`NewReport(s Signals, name, species string, uptime time.Duration) Report` computes
the score once and derives mood and message from it. `CheckedAt` reads a
package-level unexported `var now = time.Now` so tests can stub the clock.

`Signals.Score()` computes a weighted composite, clamped to `[0,100]`:
- Not ready is dominant: if `Ready` is false, the score can never exceed `35`.
- Error-rate component: `(1 - ErrorRate) * 50` points.
- Latency component: `30` points scaled linearly down to `0` as `P95Latency` goes
  from `0` to `LatencyBudget`; beyond the budget it contributes `0`, never negative.
- Readiness component: `20` points when `Ready`, else `0`.
- Restart penalty: subtract `2` points per restart, capped at `10` total.
- If `LatencyBudget <= 0`, treat the latency component as full marks (`30`) rather
  than dividing by zero.

**Tests** must cover, table-driven: every mood boundary exactly (95, 94.9, 80,
79.9, 60, 59.9, 40, 39.9, 20, 19.9, 0, 100); clamping above 100 and below 0;
the not-ready ceiling; zero `LatencyBudget`; latency far beyond budget; restart
penalty cap; and that `AllMoods()` returns all six in healthiest-first order and
that every mood has a non-empty distinct `Message()`.

**Verify:** `go test ./internal/mood/... -v` all pass; `make lint` clean.

---

## Task 3 — Telemetry (`internal/telemetry`)

**Files:** `internal/telemetry/metrics.go`, `internal/telemetry/logging.go`,
`internal/telemetry/metrics_test.go`, `internal/telemetry/logging_test.go`

- `NewLogger(level, w)` returns a `*slog.Logger` with a `slog.JSONHandler`, and a
  `ReplaceAttr` that renames the time key to `ts` and the message key to `msg`.
- `NewMetrics(reg, bi)` registers exactly the metrics in the Global Constraints
  table against the supplied `prometheus.Registerer` — never the default registry,
  so tests can use a fresh one. `buddy_build_info` is set to 1 with its labels at
  construction.
- `SetHealth` sets `buddy_health_score`, `buddy_ready`, and sets `buddy_mood` to 1
  for the active mood and 0 for every other mood in `mood.AllMoods()` — so the
  series never goes stale in Grafana.
- Histogram buckets for `buddy_work_duration_seconds`:
  `[]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}`.

**Tests** use `prometheus/client_golang/prometheus/testutil` to assert exact
exposition output for `buddy_health_score`, `buddy_ready`, and that exactly one
`buddy_mood` series is 1 after `SetHealth`. Logging tests assert the emitted JSON
has `ts`, `level`, and `msg` keys by unmarshalling into a map.

**Verify:** `go test ./internal/telemetry/... -v` all pass.

---

## Task 4 — HTTP server (`internal/api`) and `cmd/buddy-api`

**Files:** `internal/api/server.go`, `internal/api/handlers.go`,
`internal/api/middleware.go`, `internal/api/server_test.go`,
`cmd/buddy-api/main.go`, `Makefile` (add one target)

This task also adds a `test-race` Makefile target (`go test -race ./...`,
with a `##` help comment). It is not part of the default `test` target.

Routes on `net/http`'s `http.ServeMux` with method patterns (`GET /healthz`), no
third-party router.

- `GET /healthz` — always `200 {"status":"ok"}` while the process is alive. It must
  **not** consult readiness or mood; a merely unhappy plant must not be restarted
  by the kubelet. Put that reasoning in a code comment — reviewers look for it.
- `GET /readyz` — `200` when ready, `503` with `{"status":"not ready"}` otherwise.
- `GET /status` — the `mood.Report` JSON. Build it **only** via `mood.NewReport`
  (added in Task 2); do not assemble the struct field-by-field, or mood and score
  can drift apart.
- `GET /work` — sleeps a random duration in `[WorkMinDelay, WorkMaxDelay]`, then
  returns `success`, or `warning` when the roll exceeds the budget, or `failure`
  (HTTP 500) with probability `WorkErrorRate`. Records `ObserveWork`. The
  randomness source must be injectable so tests are deterministic — take a
  `*rand.Rand` in `Config` or an unexported field set by a test helper.
- `POST /chaos/readiness` — flips readiness, body `{"ready":false}`. Guarded: only
  registered when `Config.EnableChaosEndpoints` is true. Plan 3's chaos-buddy uses it.
- `GET /metrics` — `promhttp.HandlerFor(reg, ...)`.
- Middleware: recovers panics into a 500 plus an error log; records
  `ObserveHTTP` with the **route pattern**, not the raw path, so metric
  cardinality stays bounded; injects a request ID and logs one structured line
  per request.

`cmd/buddy-api/main.go`:
- Config from environment with defaults: `BUDDY_NAME` (`fernie`), `BUDDY_SPECIES`
  (`fern`), `BUDDY_PORT` (`8080`), `BUDDY_LOG_LEVEL` (`info`),
  `BUDDY_LATENCY_BUDGET` (`250ms`), `BUDDY_WORK_ERROR_RATE` (`0.05`),
  `BUDDY_WORK_MIN_DELAY` (`10ms`), `BUDDY_WORK_MAX_DELAY` (`200ms`),
  `BUDDY_ENABLE_CHAOS_ENDPOINTS` (`false`). Invalid values are a startup error with
  a clear message, never a silent fallback.
- `version`, `commit` as `var` set by `-ldflags -X`.
- `http.Server` with `ReadHeaderTimeout: 10s` (gosec requires it).
- **Graceful shutdown**, and it must be correct because the spec sells zero-downtime
  rollouts: on SIGTERM/SIGINT — (1) `SetReady(false)` immediately, (2) sleep a
  configurable `BUDDY_SHUTDOWN_DELAY` (default `5s`) so kube-proxy removes the pod
  from endpoints, (3) `srv.Shutdown(ctx)` with a `15s` timeout, (4) log each phase.
- Emit a startup log line with version, commit, and the resolved config.

**Tests** (`internal/api`, using `httptest`): `/healthz` returns 200 even when not
ready; `/readyz` returns 503 when not ready and 200 when ready; `/status` decodes
into `mood.Report` with a mood matching its score; `/work` with a seeded RNG and
`WorkErrorRate: 1.0` returns 500 and increments the `failure` counter, and with
`0.0` returns 200; `/metrics` exposition contains `buddy_build_info`; the panic
middleware turns a panicking handler into a 500 without killing the process;
`ObserveHTTP` records the route pattern for a 404-ish path rather than the raw URL.

**Verify:** `make test` passes; `make lint` clean; `go run ./cmd/buddy-api` then
`curl localhost:8080/status` returns a mood.

*Note on `-race`:* the race detector needs cgo and a C toolchain, which this
Windows development box does not have. `make test` therefore runs without
`-race`, and a separate `make test-race` target exists for platforms that
support it. CI runs `test-race` on `ubuntu-latest` (see Task 7). Do not add
`-race` to the default `test` target.

---

## Task 5 — Container image and kind cluster

**Files:** `build/Dockerfile.buddy-api`, `.dockerignore`,
`deploy/kind/kind-config.yaml`, Makefile targets

- **Dockerfile**, multi-stage: build on `golang:1.26-alpine` with
  `CGO_ENABLED=0`, `-trimpath`, and `-ldflags "-s -w -X main.version=... -X main.commit=..."`;
  final stage `gcr.io/distroless/static-debian12:nonroot`, `USER 65532:65532`,
  `EXPOSE 8080`, `ENTRYPOINT ["/buddy-api"]`. Accept `VERSION` and `COMMIT` build
  args. Add an `OCI` label block (`org.opencontainers.image.source`, `.title`,
  `.description`, `.licenses`).
- **`.dockerignore`** excluding `.git`, `bin/`, `.tools/`, `docs/`, `*.md`.
- **kind config**: cluster `k8s-buddy`, one `control-plane` and **two** `worker`
  nodes so anti-affinity and PDB behavior are real. Add
  `extraPortMappings` on the control plane for host `30080 → 30080` and
  `30300 → 30300` (NodePort access for buddy-api and, later, Grafana).
- **Makefile targets:** `kind-up` (creates the cluster if absent, idempotent),
  `kind-down`, `docker-build` (builds the image tagged with the short git SHA),
  `kind-load` (`kind load docker-image` into cluster `k8s-buddy`).

**Verify:** `make docker-build` produces an image;
`docker run --rm -p 8080:8080 <img>` then `curl /healthz` returns 200;
`docker image inspect` shows user `65532`; `make kind-up` creates a 3-node cluster
and `kubectl get nodes` shows 3 `Ready`; `make kind-load` succeeds.

---

## Task 6 — Kubernetes manifests and the first self-healing demo

**Files:** `deploy/kustomize/base/{kustomization.yaml,namespace.yaml,deployment.yaml,service.yaml,configmap.yaml,pdb.yaml,networkpolicy.yaml,serviceaccount.yaml}`,
`hack/demo.sh`, Makefile targets

- **Namespace** `k8s-buddy` labeled for Pod Security Admission `restricted`:
  `pod-security.kubernetes.io/enforce: restricted`, plus `audit` and `warn` at the
  same level, and `.../enforce-version: latest`.
- **Deployment**: 3 replicas; the security context from Global Constraints;
  requests `cpu: 50m, memory: 64Mi`, limits `cpu: 200m, memory: 128Mi`;
  `livenessProbe` → `/healthz`, `readinessProbe` → `/readyz` (`periodSeconds: 2`,
  `failureThreshold: 2`, so chaos is visible fast), `startupProbe` → `/healthz`;
  `lifecycle.preStop` `sleep 5` via `exec` (distroless has no shell — use
  `httpGet` against `/healthz` **or** set `terminationGracePeriodSeconds: 30` and
  rely on the in-process shutdown delay from Task 4; choose the in-process route
  and comment why, since distroless genuinely has no `/bin/sleep`);
  `topologySpreadConstraints` across `kubernetes.io/hostname` with
  `whenUnsatisfiable: ScheduleAnyway`; env from the ConfigMap.
- **Service**: ClusterIP `buddy-api` on port 80 → `http`, plus a second
  NodePort service `buddy-api-nodeport` on `30080` for the demo.
- **PDB**: `minAvailable: 2`.
- **NetworkPolicy**: default-deny ingress and egress for the namespace, plus
  explicit allows for DNS egress to `kube-system`, and ingress to port 8080 from
  within the namespace.
- **ServiceAccount** `buddy-api` with `automountServiceAccountToken: false` — the
  API needs no cluster access, and saying so explicitly is the point.
- **`hack/demo.sh`** — the narrated demo. `set -euo pipefail`. Steps: verify
  prerequisites; show pods; curl `/status` through the NodePort and print the
  mood; delete one pod; poll and print pod state every second until all 3 are
  `Ready` again; print elapsed recovery time; curl `/status` again. Human-readable
  output with clear section headers. It must exit non-zero if recovery does not
  complete within 90 seconds — this script becomes the CI e2e assertion in Plan 3.
- **Makefile**: `deploy` (`kubectl apply -k deploy/kustomize/base`), `undeploy`,
  `demo` (= `kind-up` → `docker-build` → `kind-load` → `deploy` → wait for rollout
  → `hack/demo.sh`), `status`, `logs`.

**Verify (run these for real, do not assume):**
`kubectl apply -k deploy/kustomize/base --dry-run=client` clean;
`make demo` end-to-end on the real kind cluster;
`kubectl get pods -n k8s-buddy` shows 3/3 Ready;
deleting a pod results in a replacement reaching Ready and `hack/demo.sh` exiting 0;
`kubectl -n k8s-buddy exec` confirms nothing runs as root
(`kubectl get pod -o jsonpath='{.spec.containers[0].securityContext}'`).

---

## Task 7 — CI

**Files:** `.github/workflows/ci.yaml`, `.github/dependabot.yaml`,
`.github/workflows/codeql.yaml`

`ci.yaml`, triggered on push and pull_request to `main`, with
`permissions: contents: read` at the top level and least-privilege bumps per job:

- `lint` — `actions/setup-go` with `cache: true`, then `make lint` and a
  `gofmt -l` check that fails if output is non-empty.
- `test` — runs on `ubuntu-latest`, which has a C toolchain, so it invokes
  `make test-race` (race detector enabled) followed by `make test-cover`, and
  uploads coverage as an artifact. The race detector must run somewhere, and
  Linux CI is that somewhere.
- `build` — `docker/setup-buildx-action`, build for `linux/amd64,linux/arm64`,
  push only on a `main` push (not on PRs), and generate an SBOM with
  `anchore/sbom-action`.
- `scan` — `aquasecurity/trivy-action` in `fs` mode over the repo and `image` mode
  over the built image, `severity: HIGH,CRITICAL`, `exit-code: 1`.
- `manifests` — `kubeconform` against real Kubernetes schemas over
  `deploy/kustomize/base`, strict mode.

All action versions pinned to a tag (`@v4`), never `@master`. Add a `concurrency`
block cancelling superseded runs on the same ref. Dependabot config covers
`gomod`, `github-actions`, and `docker`, weekly.

**Verify:** `actionlint` if available, otherwise a careful read plus
`python -c "import yaml,sys;[yaml.safe_load(open(f)) for f in sys.argv[1:]]"`
over each workflow file to prove the YAML parses. Confirm every `run:` step
invokes a Makefile target that actually exists.

---

## Out of scope for Plan 1

The Plant CRD and operator (Plan 2); chaos-buddy, Prometheus/Grafana/Loki, the
Helm chart, Kustomize overlays, admission webhooks, the CI e2e job, and the
README/ADR/runbook set (Plan 3). Do not build ahead.
