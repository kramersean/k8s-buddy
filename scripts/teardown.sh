#!/bin/bash
set -e

CLUSTER_NAME="k8s-buddy"
NAMESPACE="k8s-buddy"
MONITORING_NS="monitoring"

echo "🧹 k8s-buddy Teardown"
echo "====================="
echo ""

read -p "❓ Are you sure you want to teardown everything? [y/N] " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "👋 Cancelled!"
    exit 0
fi

echo "🔄 Cleaning up k8s-buddy applications..."
kubectl delete -k "${SCRIPT_DIR}/../k8s/overlays/local-kind/" --ignore-not-found=true 2>/dev/null || true

echo "🔄 Cleaning up monitoring namespace..."
kubectl delete namespace "${MONITORING_NS}" --ignore-not-found=true 2>/dev/null || true

echo "🔄 Cleaning up k8s-buddy namespace..."
kubectl delete namespace "${NAMESPACE}" --ignore-not-found=true 2>/dev/null || true

echo "🔄 Deleting kind cluster..."
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    kind delete cluster --name "${CLUSTER_NAME}"
    echo "✅ Kind cluster '${CLUSTER_NAME}' deleted"
else
    echo "⚠️  Kind cluster '${CLUSTER_NAME}' not found, skipping..."
fi

echo ""
echo "✅ Teardown complete!"
echo ""
echo "🧹 Cleanup any leftover Docker images:"
echo "   docker images | grep -E '(buddy-api|buddy-worker|chaos-buddy)'"
echo "   docker rmi <image_ids>"
