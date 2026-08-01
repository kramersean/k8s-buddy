#!/usr/bin/env bash
#
# hack/demo.sh -- K8s Buddy's self-healing demo.
#
# Deletes one buddy-api pod and proves two things: Kubernetes replaces it,
# and the Service stays reachable while that happens. The second claim is
# measured, not asserted -- a background poller hits /status every 200ms for
# the entire chaos-and-recovery window and the summary reports the resulting
# availability percentage, whatever it turns out to be.
#
# The demo also generates real /work load before taking its baseline, so the
# mood the plant reports is derived from live error-rate and latency signals
# rather than being a constant.
#
# This script is meant to be watched: every phase prints a human-readable
# header. It is also the CI end-to-end assertion, so its exit code is the
# real pass/fail signal, not decorative output.
#
# Exit codes:
#   0  recovery completed within RECOVERY_TIMEOUT_SECONDS
#   1  a prerequisite was missing (kubectl, cluster, namespace, curl), the
#      deployment was not already healthy before chaos, or /status broke its
#      response contract
#   2  recovery did not complete within RECOVERY_TIMEOUT_SECONDS
set -euo pipefail

NAMESPACE="k8s-buddy"
DEPLOYMENT="buddy-api"
APP_LABEL="app.kubernetes.io/name=buddy-api"
NODEPORT_URL="http://localhost:30080"
RECOVERY_TIMEOUT_SECONDS=90

# Every curl gets a hard ceiling. Without one, a black-holed NodePort (the
# exact failure mode a misscoped NetworkPolicy produces -- TCP connects,
# then nothing, no RST) leaves curl waiting forever, and the demo hangs
# until CI's 20-minute job timeout kills it with no useful output instead of
# failing in seconds with a clear message.
CURL_MAX_TIME=5

# How many /work requests to fire before taking the baseline. The mood
# engine derives its error rate and p95 latency from a rolling window of
# recent /work observations, so a pod that has served none reports a
# perfect score by definition. ~20 requests spread across the replicas is
# enough for each to have a populated window.
WORK_REQUESTS=20

# Availability sampling interval, in seconds, for the background poller.
AVAILABILITY_POLL_INTERVAL=0.2

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
# Availability poller
#
# Runs in the background for the whole chaos-and-recovery window, appending
# one line per sample to AVAIL_LOG. A file is used rather than shell
# variables because the poller runs in a subshell, whose variables cannot be
# read back by the parent.
# ---------------------------------------------------------------------------
AVAIL_LOG=""
POLL_PID=""

cleanup() {
	if [ -n "$POLL_PID" ]; then
		kill "$POLL_PID" 2>/dev/null || true
		wait "$POLL_PID" 2>/dev/null || true
		POLL_PID=""
	fi
	if [ -n "$AVAIL_LOG" ] && [ -f "$AVAIL_LOG" ]; then
		rm -f "$AVAIL_LOG"
	fi
}
trap cleanup EXIT

start_availability_poll() {
	AVAIL_LOG="$(mktemp)"
	(
		while true; do
			if curl -sf --max-time "$CURL_MAX_TIME" -o /dev/null "$NODEPORT_URL/status"; then
				echo ok >>"$AVAIL_LOG"
			else
				echo fail >>"$AVAIL_LOG"
			fi
			sleep "$AVAILABILITY_POLL_INTERVAL"
		done
	) &
	POLL_PID=$!
}

stop_availability_poll() {
	if [ -n "$POLL_PID" ]; then
		kill "$POLL_PID" 2>/dev/null || true
		wait "$POLL_PID" 2>/dev/null || true
		POLL_PID=""
	fi
}

# parse_mood extracts the "mood" value from a /status body, or prints
# nothing if the key is absent.
parse_mood() {
	echo "$1" | grep -o '"mood":"[^"]*"' | head -1 | sed 's/.*"mood":"\([^"]*\)".*/\1/'
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
# Phase 1: healthy baseline
#
# Assert the deployment is ALREADY fully rolled out before any chaos is
# injected. Without this the demo happily runs against a deployment that was
# broken before it started, then blames the resulting outage on the pod it
# deleted -- reporting a recovery failure whose real cause is something this
# script never touched.
# ---------------------------------------------------------------------------
header "Baseline health: deployment must already be rolled out"
kubectl -n "$NAMESPACE" rollout status "deployment/$DEPLOYMENT" --timeout=60s ||
	fail "deployment/$DEPLOYMENT is not healthy BEFORE chaos was injected -- fix the deployment first; this run would have misattributed a pre-existing outage to the pod deletion"

# The replica count is read from the live Deployment rather than hardcoded,
# so this script cannot drift out of sync with deployment.yaml (or with an
# overlay that scales it).
EXPECTED_REPLICAS="$(kubectl -n "$NAMESPACE" get "deployment/$DEPLOYMENT" -o jsonpath='{.spec.replicas}')"
case "$EXPECTED_REPLICAS" in
'' | *[!0-9]*) fail "could not read .spec.replicas from deployment/$DEPLOYMENT (got '$EXPECTED_REPLICAS')" ;;
esac
[ "$EXPECTED_REPLICAS" -gt 0 ] || fail "deployment/$DEPLOYMENT has .spec.replicas=$EXPECTED_REPLICAS; nothing to demo"
echo "deployment/$DEPLOYMENT declares $EXPECTED_REPLICAS replicas."

header "Current pods"
kubectl -n "$NAMESPACE" get pods -l "$APP_LABEL" -o wide

# ---------------------------------------------------------------------------
# Phase 2: generate load, then take the baseline
#
# The mood is computed from a rolling window of recent /work observations.
# With an empty window there is nothing to be unhappy about, so /status
# would report a perfect score no matter what the service is doing. Driving
# real traffic first is what makes the reported mood a measurement.
# ---------------------------------------------------------------------------
header "Generating load: $WORK_REQUESTS requests to /work"
WORK_OK=0
WORK_ERR=0
i=0
while [ "$i" -lt "$WORK_REQUESTS" ]; do
	if curl -sf --max-time "$CURL_MAX_TIME" -o /dev/null "$NODEPORT_URL/work"; then
		WORK_OK=$((WORK_OK + 1))
	else
		# /work returns 500 for a SIMULATED failure (BUDDY_WORK_ERROR_RATE
		# defaults to 0.05), so a few of these are expected and are exactly
		# the signal that should move the mood -- they are not an outage.
		WORK_ERR=$((WORK_ERR + 1))
	fi
	i=$((i + 1))
done
echo "/work: $WORK_OK ok, $WORK_ERR non-2xx (simulated failures are expected at BUDDY_WORK_ERROR_RATE)"

[ "$WORK_OK" -gt 0 ] || fail "every /work request failed -- the service is not usable, so there is nothing to demo"

header "Baseline: curl /status through NodePort 30080"
BASELINE_STATUS="$(curl -sf --max-time "$CURL_MAX_TIME" "$NODEPORT_URL/status")" || fail "could not reach $NODEPORT_URL/status"
echo "$BASELINE_STATUS"
BASELINE_MOOD="$(parse_mood "$BASELINE_STATUS")"
[ -n "$BASELINE_MOOD" ] ||
	fail "/status returned no \"mood\" key -- the /status response contract is broken. Body was: $BASELINE_STATUS"
echo
echo "Plant mood before chaos: $BASELINE_MOOD"

# ---------------------------------------------------------------------------
# Phase 3: chaos -- delete one pod, with availability under measurement
# ---------------------------------------------------------------------------
header "Starting availability poll (/status every ${AVAILABILITY_POLL_INTERVAL}s)"
start_availability_poll
echo "Polling in the background; every sample from here until recovery counts."

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

	# Exclude $VICTIM by name from both counts. Immediately after
	# `kubectl delete --wait=false`, the victim pod object still exists
	# (Terminating) and can still report ready:true until its next 2s
	# readiness probe catches up -- if it were counted, a poll that lands
	# before the ReplicaSet has even created a replacement could see
	# READY==3 && TOTAL==3 (the 2 survivors plus the not-yet-terminated
	# victim) and declare success having observed zero chaos. Filtering
	# the victim's own line out of every count means recovery can only be
	# declared once a genuinely NEW pod (the replacement) exists and is
	# Ready alongside the 2 surviving pods -- the victim's own transient
	# ready:true can no longer contribute to hitting 3/3.
	PODS_LINE="$(kubectl -n "$NAMESPACE" get pods -l "$APP_LABEL" --no-headers 2>/dev/null | grep -v "^${VICTIM}[[:space:]]" || true)"

	if [ -n "$PODS_LINE" ]; then
		TOTAL_COUNT="$(echo "$PODS_LINE" | wc -l | tr -d ' ')"
		READY_COUNT="$(echo "$PODS_LINE" | awk '{print $2}' | grep -c '^1/1$' || true)"
	else
		TOTAL_COUNT=0
		READY_COUNT=0
	fi

	echo "[t=${ELAPSED}s] Ready (excluding deleted pod ${VICTIM}): ${READY_COUNT}/${TOTAL_COUNT} (want ${EXPECTED_REPLICAS}/${EXPECTED_REPLICAS})"
	kubectl -n "$NAMESPACE" get pods -l "$APP_LABEL" --no-headers 2>/dev/null | sed 's/^/    /' || true

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

# Stop measuring the moment recovery is decided, so the availability number
# covers exactly the chaos-and-recovery window and nothing else.
stop_availability_poll

TOTAL_POLLS="$(wc -l <"$AVAIL_LOG" | tr -d ' ')"
OK_POLLS="$(grep -c '^ok$' "$AVAIL_LOG" || true)"
FAILED_POLLS=$((TOTAL_POLLS - OK_POLLS))
AVAILABILITY="$(awk -v ok="$OK_POLLS" -v total="$TOTAL_POLLS" \
	'BEGIN { if (total == 0) printf "n/a"; else printf "%.2f%%", (ok / total) * 100 }')"

if [ "$RECOVERED" -ne 1 ]; then
	header "RECOVERY FAILED"
	echo "buddy-api did not reach ${EXPECTED_REPLICAS}/${EXPECTED_REPLICAS} Ready within ${RECOVERY_TIMEOUT_SECONDS}s."
	echo "Availability during the attempt: $AVAILABILITY ($OK_POLLS/$TOTAL_POLLS samples)"
	kubectl -n "$NAMESPACE" get pods -l "$APP_LABEL" -o wide
	exit 2
fi

header "Recovery complete"
echo "All ${EXPECTED_REPLICAS} pods Ready again after ${RECOVERY_SECONDS}s (timeout was ${RECOVERY_TIMEOUT_SECONDS}s)."

# ---------------------------------------------------------------------------
# Phase 5: post-recovery status
# ---------------------------------------------------------------------------
header "Post-recovery: curl /status through NodePort 30080"
FINAL_STATUS="$(curl -sf --max-time "$CURL_MAX_TIME" "$NODEPORT_URL/status")" || fail "could not reach $NODEPORT_URL/status after recovery"
echo "$FINAL_STATUS"
FINAL_MOOD="$(parse_mood "$FINAL_STATUS")"
[ -n "$FINAL_MOOD" ] ||
	fail "/status returned no \"mood\" key after recovery -- the /status response contract is broken. Body was: $FINAL_STATUS"
echo
echo "Plant mood after recovery: $FINAL_MOOD"

header "Summary"
echo "Deleted pod:      $VICTIM"
echo "Recovery time:    ${RECOVERY_SECONDS}s (limit ${RECOVERY_TIMEOUT_SECONDS}s)"
echo "Mood before:      $BASELINE_MOOD"
echo "Mood after:       $FINAL_MOOD"
echo "Availability:     $AVAILABILITY ($OK_POLLS ok / $FAILED_POLLS failed, $TOTAL_POLLS samples every ${AVAILABILITY_POLL_INTERVAL}s)"
echo "Result:           SELF-HEALED"

if [ "$FAILED_POLLS" -ne 0 ]; then
	echo
	echo "NOTE: $FAILED_POLLS of $TOTAL_POLLS availability samples failed. The service"
	echo "      recovered, but it was NOT reachable for the entire window. That is"
	echo "      reported rather than hidden; a zero-downtime claim requires 100%."
fi

exit 0
