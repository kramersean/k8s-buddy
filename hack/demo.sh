#!/usr/bin/env bash
#
# hack/demo.sh -- K8s Buddy's self-healing demo.
#
# Deletes one buddy-api pod, then proves Kubernetes replaces it and the
# service stays reachable throughout. This script is meant to be watched:
# every phase prints a human-readable header, and it becomes Task 7's CI
# end-to-end assertion, so its exit code is the actual pass/fail signal --
# not decorative output.
#
# Exit codes:
#   0  recovery completed within RECOVERY_TIMEOUT_SECONDS
#   1  a prerequisite was missing (kubectl, cluster, namespace, curl)
#   2  recovery did not complete within RECOVERY_TIMEOUT_SECONDS
set -euo pipefail

NAMESPACE="k8s-buddy"
APP_LABEL="app.kubernetes.io/name=buddy-api"
NODEPORT_URL="http://localhost:30080"
RECOVERY_TIMEOUT_SECONDS=90
EXPECTED_REPLICAS=3

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------
header() {
	echo
	echo "=== $1 ==="
}

fail() {
	echo "FATAL: $1" >&2
	exit 1
}

# ---------------------------------------------------------------------------
# Phase 0: prerequisites
# ---------------------------------------------------------------------------
header "Checking prerequisites"

command -v kubectl >/dev/null 2>&1 || fail "kubectl not found on PATH"
command -v curl >/dev/null 2>&1 || fail "curl not found on PATH"
echo "kubectl and curl are present."

kubectl cluster-info >/dev/null 2>&1 || fail "cluster is not reachable (check your kubeconfig / context)"
echo "cluster is reachable."

kubectl get namespace "$NAMESPACE" >/dev/null 2>&1 || fail "namespace '$NAMESPACE' does not exist -- run 'make deploy' first"
echo "namespace '$NAMESPACE' exists."

# ---------------------------------------------------------------------------
# Phase 1: current state
# ---------------------------------------------------------------------------
header "Current pods"
kubectl -n "$NAMESPACE" get pods -l "$APP_LABEL" -o wide

# ---------------------------------------------------------------------------
# Phase 2: baseline status
# ---------------------------------------------------------------------------
header "Baseline: curl /status through NodePort 30080"
BASELINE_STATUS="$(curl -sf "$NODEPORT_URL/status")" || fail "could not reach $NODEPORT_URL/status"
echo "$BASELINE_STATUS"
BASELINE_MOOD="$(echo "$BASELINE_STATUS" | grep -o '"mood":"[^"]*"' || true)"
echo
echo "Plant mood before chaos: ${BASELINE_MOOD:-<unparsed>}"

# ---------------------------------------------------------------------------
# Phase 3: chaos -- delete one pod
# ---------------------------------------------------------------------------
header "Injecting chaos: deleting one buddy-api pod"
VICTIM="$(kubectl -n "$NAMESPACE" get pods -l "$APP_LABEL" -o jsonpath='{.items[0].metadata.name}')"
[ -n "$VICTIM" ] || fail "could not find a buddy-api pod to delete"
echo "Deleting pod: $VICTIM"
kubectl -n "$NAMESPACE" delete pod "$VICTIM" --wait=false

# ---------------------------------------------------------------------------
# Phase 4: poll until fully recovered
# ---------------------------------------------------------------------------
header "Watching recovery (polling every 1s, timeout ${RECOVERY_TIMEOUT_SECONDS}s)"
START_TIME=$(date +%s)
RECOVERED=0

while true; do
	NOW=$(date +%s)
	ELAPSED=$((NOW - START_TIME))

	READY_COUNT="$(kubectl -n "$NAMESPACE" get pods -l "$APP_LABEL" \
		-o jsonpath='{range .items[*]}{.status.containerStatuses[0].ready}{"\n"}{end}' 2>/dev/null \
		| grep -c '^true$' || true)"
	TOTAL_COUNT="$(kubectl -n "$NAMESPACE" get pods -l "$APP_LABEL" --no-headers 2>/dev/null | wc -l | tr -d ' ')"

	echo "[t=${ELAPSED}s] Ready: ${READY_COUNT}/${TOTAL_COUNT} (want ${EXPECTED_REPLICAS}/${EXPECTED_REPLICAS})"
	kubectl -n "$NAMESPACE" get pods -l "$APP_LABEL" --no-headers 2>/dev/null | sed 's/^/    /'

	if [ "$READY_COUNT" -eq "$EXPECTED_REPLICAS" ] && [ "$TOTAL_COUNT" -eq "$EXPECTED_REPLICAS" ]; then
		RECOVERED=1
		break
	fi

	if [ "$ELAPSED" -ge "$RECOVERY_TIMEOUT_SECONDS" ]; then
		break
	fi

	sleep 1
done

END_TIME=$(date +%s)
RECOVERY_SECONDS=$((END_TIME - START_TIME))

if [ "$RECOVERED" -ne 1 ]; then
	header "RECOVERY FAILED"
	echo "buddy-api did not reach ${EXPECTED_REPLICAS}/${EXPECTED_REPLICAS} Ready within ${RECOVERY_TIMEOUT_SECONDS}s."
	kubectl -n "$NAMESPACE" get pods -l "$APP_LABEL" -o wide
	exit 2
fi

header "Recovery complete"
echo "All ${EXPECTED_REPLICAS} pods Ready again after ${RECOVERY_SECONDS}s (timeout was ${RECOVERY_TIMEOUT_SECONDS}s)."

# ---------------------------------------------------------------------------
# Phase 5: post-recovery status
# ---------------------------------------------------------------------------
header "Post-recovery: curl /status through NodePort 30080"
FINAL_STATUS="$(curl -sf "$NODEPORT_URL/status")" || fail "could not reach $NODEPORT_URL/status after recovery"
echo "$FINAL_STATUS"
FINAL_MOOD="$(echo "$FINAL_STATUS" | grep -o '"mood":"[^"]*"' || true)"
echo
echo "Plant mood after recovery: ${FINAL_MOOD:-<unparsed>}"

header "Summary"
echo "Deleted pod:      $VICTIM"
echo "Recovery time:    ${RECOVERY_SECONDS}s (limit ${RECOVERY_TIMEOUT_SECONDS}s)"
echo "Result:           SELF-HEALED"

exit 0
