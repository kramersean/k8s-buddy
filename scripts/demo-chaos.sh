#!/bin/bash
set -e

NAMESPACE="k8s-buddy"
CHAOS_POD=$(kubectl get pod -n "${NAMESPACE}" -l app=chaos-buddy -o jsonpath='{.items[0].metadata.name}')

echo "🌪️  k8s-buddy Chaos Demo"
echo "========================"
echo ""

if [ -z "${CHAOS_POD}" ]; then
    echo "❌ chaos-buddy pod not found!"
    exit 1
fi

echo "📊 Current chaos status:"
kubectl exec -n "${NAMESPACE}" "${CHAOS_POD}" -- /app/chaos-buddy /status
echo ""

# Check if chaos is already enabled
STATUS=$(kubectl exec -n "${NAMESPACE}" "${CHAOS_POD}" -- /app/chaos-buddy /status 2>/dev/null | grep -o '"chaos_enabled": [^,]*' | cut -d' ' -f2 || echo "false")

echo "🔄 Enabling chaos mode..."
kubectl exec -n "${NAMESPACE}" "${CHAOS_POD}" -- /app/chaos-buddy /toggle
echo ""

echo "⏳ Chaos is now enabled! Here's what to watch:"
echo ""
echo "📊 Metrics to observe:"
echo "   - buddy_api_errors_total should increase"
echo "   - kube_pod_container_status_restarts_total should spike"
echo "   - buddy_api_request_duration_seconds should show latency spikes"
echo ""
echo "🖥️  Terminal 1 - Watch pod restarts:"
echo "   kubectl get pods -n ${NAMESPACE} -w"
echo ""
echo "🖥️  Terminal 2 - Watch errors:"
echo "   kubectl port-forward -n monitoring svc/prometheus-server 9090:80"
echo "   # Then query: rate(buddy_api_errors_total[1m])"
echo ""
echo "🖥️  Terminal 3 - Watch Grafana:"
echo "   kubectl port-forward -n monitoring svc/grafana 3000:3000"
echo "   # Open: http://localhost:3000/d/k8s-buddy-dashboard"
echo ""
echo "📝 The chaos-buddy will:"
echo "   - Randomly kill pods in the k8s-buddy namespace"
echo "   - Flip the ConfigMap to trigger readiness failures"
echo "   - Do this every ~60 seconds"
echo ""
echo "⏹️  To disable chaos:"
echo "   kubectl exec -n ${NAMESPACE} ${CHAOS_POD} -- /app/chaos-buddy /toggle"
echo ""
echo "🔄 Current chaos toggle command:"
echo "   kubectl exec -n ${NAMESPACE} ${CHAOS_POD} -- /app/chaos-buddy /toggle"
