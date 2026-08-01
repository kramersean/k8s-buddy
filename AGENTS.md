# K8s Buddy Agent Instructions

## Project Vision

K8s Buddy is a personal Kubernetes observability sandbox.

It is a friendly “talking plant” application that demonstrates:
- Kubernetes self-healing
- random pod and readiness failures
- Prometheus metrics
- structured logs
- traces
- Grafana dashboards
- clear local demo workflows

The app should feel alive. It should report statuses like:
- “I’m feeling leafy and stable 🌱”
- “Lost a leaf, but I’m recovering.”
- “I’m not feeling too hot.”
- “I’m ready to rock and roll 🌿”

## Current Goal

Build toward a local demo where:

1. A user starts a kind cluster.
2. K8s Buddy deploys locally.
3. buddy-api exposes health, readiness, work, and status endpoints.
4. chaos-buddy causes controlled failure.
5. Kubernetes restarts or recovers affected pods.
6. Prometheus captures the behavior.
7. Grafana makes the recovery visible.
8. README explains the whole demo clearly.

## Hard Constraints

- Work only inside this repository.
- Do not access files outside the repo.
- Do not read or expose secrets.
- Do not touch SSH keys, cloud credentials, kubeconfigs outside expected local development usage, or personal files.
- Do not deploy to real cloud infrastructure.
- Prefer kind, local Docker, local Helm, and dry-run validation.
- Make small, reviewable changes.
- Add or update tests when behavior changes.
- Run validation commands after changes.
- Do not rewrite the entire project unless explicitly asked.
- Do not delete major files without explaining why.

## Development Style

Prefer vertical slices over giant rewrites.

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

## Validation Expectations

Use the most relevant checks available, such as:

```bash
go test ./...
pytest
docker compose config
kubectl apply --dry-run=client -f k8s/
helm template ./charts/k8s-buddy